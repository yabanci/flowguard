package flowguard

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests run for a few seconds under sustained load to catch
// timing bugs that only show up over time.

func TestStress_RateLimiterUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	rl := NewRateLimiter(100, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var allowed, rejected atomic.Int64
	var wg sync.WaitGroup

	// 10 goroutines hammering for 3 seconds
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if rl.Allow() {
					allowed.Add(1)
				} else {
					rejected.Add(1)
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()
	t.Logf("3s stress: allowed=%d rejected=%d total=%d",
		allowed.Load(), rejected.Load(), allowed.Load()+rejected.Load())

	// at 100/s over 3s, should have allowed roughly 300 (±burst)
	if a := allowed.Load(); a < 200 || a > 500 {
		t.Fatalf("allowed count %d seems off for 100/s over 3s", a)
	}
}

func TestStress_CircuitBreakerMixedLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	cb := NewCircuitBreaker(
		WithFailureThreshold(10),
		WithOpenTimeout(200*time.Millisecond),
		WithSuccessThreshold(3),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var total, errors, opens atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				err := cb.Do(ctx, func(ctx context.Context) error {
					if rand.Intn(10) < 3 { // 30% error rate
						return errBoom
					}
					return nil
				})
				total.Add(1)
				if err == ErrCircuitOpen || err == ErrTooManyRequests {
					opens.Add(1)
				} else if err != nil {
					errors.Add(1)
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// state should be valid
	s := cb.State()
	if s != StateClosed && s != StateOpen && s != StateHalfOpen {
		t.Fatalf("invalid final state: %v", s)
	}

	t.Logf("3s stress: total=%d errors=%d opens=%d final_state=%s",
		total.Load(), errors.Load(), opens.Load(), s)
}

func TestStress_LoadShedderConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	ls := NewLoadShedder(30, 20*time.Millisecond,
		WithLoadShedLimits(5, 100),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var ok, shed atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				err := ls.Do(ctx, func(ctx context.Context) error {
					// simulate variable work
					time.Sleep(time.Duration(5+rand.Intn(30)) * time.Millisecond)
					return nil
				})
				if err == ErrLoadShed {
					shed.Add(1)
				} else if err == nil {
					ok.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	lim := ls.CurrentLimit()
	if lim < 5 || lim > 100 {
		t.Fatalf("INVARIANT VIOLATED: final limit %d outside bounds [5, 100]", lim)
	}

	t.Logf("3s stress: ok=%d shed=%d final_limit=%d",
		ok.Load(), shed.Load(), lim)
}

func TestStress_BulkheadUnderPressure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	maxConc := 10
	b := NewBulkhead(maxConc, WithMaxWaitDuration(50*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var peak atomic.Int32
	var current atomic.Int32
	var ok, rejected atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				err := b.Do(ctx, func(ctx context.Context) error {
					n := current.Add(1)
					// track peak
					for {
						p := peak.Load()
						if n <= p || peak.CompareAndSwap(p, n) {
							break
						}
					}
					time.Sleep(time.Duration(1+rand.Intn(10)) * time.Millisecond)
					current.Add(-1)
					return nil
				})
				if err != nil {
					rejected.Add(1)
				} else {
					ok.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	if p := peak.Load(); p > int32(maxConc) {
		t.Fatalf("INVARIANT VIOLATED: peak concurrency %d > max %d", p, maxConc)
	}

	t.Logf("3s stress: ok=%d rejected=%d peak=%d",
		ok.Load(), rejected.Load(), peak.Load())
}

func TestStress_PolicyEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	p := NewPolicy(
		WithPolicyRateLimiter(NewRateLimiter(200, 50)),
		WithPolicyCircuitBreaker(NewCircuitBreaker(WithFailureThreshold(20))),
		WithPolicyRetry(NewRetry(
			WithMaxRetries(2),
			WithConstantBackoff(time.Millisecond),
			WithJitter(0),
		)),
		WithPolicyBulkhead(NewBulkhead(20, WithMaxWaitDuration(100*time.Millisecond))),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var ok, failed atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				err := p.Do(ctx, func(ctx context.Context) error {
					if rand.Intn(10) < 2 { // 20% error
						return errBoom
					}
					time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)
					return nil
				})
				if err != nil && err != context.DeadlineExceeded {
					failed.Add(1)
				} else if err == nil {
					ok.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	t.Logf("3s stress policy: ok=%d failed=%d", ok.Load(), failed.Load())

	if ok.Load() == 0 {
		t.Fatal("expected at least some successful calls")
	}
}
