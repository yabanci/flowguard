package flowguard

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests verify structural invariants that must always hold,
// regardless of timing, concurrency, or input.

func TestInvariant_CB_OpenNeverCallsFn(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(1),
		WithCircuitBreakerClock(clk),
	)
	ctx := context.Background()

	// trip it
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })

	if cb.State() != StateOpen {
		t.Fatal("should be open")
	}

	// fn should never be called when open
	called := false
	cb.Do(ctx, func(ctx context.Context) error {
		called = true
		return nil
	})

	if called {
		t.Fatal("INVARIANT VIOLATED: fn was called when circuit breaker is open")
	}
}

func TestInvariant_CB_OpenNeverCallsFn_Concurrent(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(1),
		WithCircuitBreakerClock(clk),
	)
	ctx := context.Background()

	// trip it
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })

	var called atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cb.Do(ctx, func(ctx context.Context) error {
				called.Add(1)
				return nil
			})
		}()
	}
	wg.Wait()

	if called.Load() > 0 {
		t.Fatalf("INVARIANT VIOLATED: fn called %d times while circuit is open", called.Load())
	}
}

func TestInvariant_Retry_MaxCallCount(t *testing.T) {
	// retry must call fn at most maxRetries+1 times, always
	for maxRetries := 0; maxRetries <= 10; maxRetries++ {
		clk := newMockClock(time.Now())
		r := NewRetry(
			WithMaxRetries(maxRetries),
			WithRetryClock(clk),
			WithJitter(0),
		)

		calls := 0
		r.Do(context.Background(), func(ctx context.Context) error {
			calls++
			return errBoom
		})

		expected := maxRetries + 1
		if calls != expected {
			t.Fatalf("maxRetries=%d: expected exactly %d calls, got %d", maxRetries, expected, calls)
		}
	}
}

func TestInvariant_RateLimiter_NeverExceedsBurst(t *testing.T) {
	// Allow() should never return true more than burst times without time passing.
	// Use mock clock so real time doesn't cause refills.
	for _, burst := range []int{1, 5, 10, 50, 100} {
		clk := newMockClock(time.Now())
		rl := NewRateLimiter(1000, burst, WithRateLimiterClock(clk))

		allowed := 0
		for i := 0; i < burst*3; i++ {
			if rl.Allow() {
				allowed++
			}
		}

		if allowed > burst {
			t.Fatalf("burst=%d: allowed %d calls without time passing (should be <= %d)",
				burst, allowed, burst)
		}
	}
}

func TestInvariant_RateLimiter_NeverExceedsBurst_Concurrent(t *testing.T) {
	burst := 50
	rl := NewRateLimiter(1, burst) // 1/s rate, burst of 50

	var allowed atomic.Int32
	var wg sync.WaitGroup

	// fire 200 goroutines at once — should never get more than burst
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rl.Allow() {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got > int32(burst) {
		t.Fatalf("INVARIANT VIOLATED: %d allowed with burst=%d", got, burst)
	}
}

func TestInvariant_Bulkhead_NeverExceedsConcurrency(t *testing.T) {
	maxConc := 5
	b := NewBulkhead(maxConc, WithMaxWaitDuration(time.Second))
	ctx := context.Background()

	var peak atomic.Int32
	var current atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Do(ctx, func(ctx context.Context) error {
				n := current.Add(1)
				// track peak
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				current.Add(-1)
				return nil
			})
		}()
	}
	wg.Wait()

	if p := peak.Load(); p > int32(maxConc) {
		t.Fatalf("INVARIANT VIOLATED: peak concurrency %d exceeded max %d", p, maxConc)
	}
	t.Logf("peak concurrency: %d (max: %d)", peak.Load(), maxConc)
}

func TestInvariant_LoadShedder_LimitStaysInBounds(t *testing.T) {
	min, max := 5, 50
	ls := NewLoadShedder(20, 10*time.Millisecond,
		WithLoadShedLimits(min, max),
	)
	ctx := context.Background()

	// hammer with failures to drive limit down
	for i := 0; i < 100; i++ {
		ls.Do(ctx, func(ctx context.Context) error { return errBoom })
		lim := ls.CurrentLimit()
		if lim < min || lim > max {
			t.Fatalf("INVARIANT VIOLATED: limit %d outside [%d, %d] after iteration %d",
				lim, min, max, i)
		}
	}

	// hammer with successes to drive limit up
	for i := 0; i < 200; i++ {
		ls.Do(ctx, func(ctx context.Context) error { return nil })
		lim := ls.CurrentLimit()
		if lim < min || lim > max {
			t.Fatalf("INVARIANT VIOLATED: limit %d outside [%d, %d] after success iteration %d",
				lim, min, max, i)
		}
	}
}

func TestInvariant_LoadShedder_LimitStaysInBounds_Concurrent(t *testing.T) {
	min, max := 3, 30
	ls := NewLoadShedder(10, 5*time.Millisecond,
		WithLoadShedLimits(min, max),
	)
	ctx := context.Background()

	var wg sync.WaitGroup
	var violated atomic.Bool

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ls.Do(ctx, func(ctx context.Context) error {
				if n%3 == 0 {
					time.Sleep(10 * time.Millisecond) // trigger decrease
				}
				return nil
			})
			lim := ls.CurrentLimit()
			if lim < min || lim > max {
				violated.Store(true)
			}
		}(i)
	}
	wg.Wait()

	if violated.Load() {
		t.Fatal("INVARIANT VIOLATED: limit went outside bounds under concurrency")
	}
}

func TestInvariant_CB_StateTransitions(t *testing.T) {
	// Valid transitions: Closed→Open, Open→HalfOpen, HalfOpen→Closed, HalfOpen→Open
	// Invalid: Closed→HalfOpen, Open→Closed
	clk := newMockClock(time.Now())
	obs := &testObserver{}
	cb := NewCircuitBreaker(
		WithFailureThreshold(2),
		WithSuccessThreshold(2),
		WithOpenTimeout(time.Second),
		WithCircuitBreakerClock(clk),
		WithCircuitBreakerObserver(obs),
	)

	ctx := context.Background()

	// Closed → Open
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })

	// Open → HalfOpen (via timeout)
	clk.Advance(2 * time.Second)
	cb.State() // trigger check

	// HalfOpen → Open (failure in half-open)
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })

	// Open → HalfOpen again
	clk.Advance(2 * time.Second)
	cb.State()

	// HalfOpen → Closed (successes)
	cb.Do(ctx, func(ctx context.Context) error { return nil })
	cb.Do(ctx, func(ctx context.Context) error { return nil })

	// verify all transitions are valid
	validTransitions := map[[2]State]bool{
		{StateClosed, StateOpen}:     true,
		{StateOpen, StateHalfOpen}:   true,
		{StateHalfOpen, StateOpen}:   true,
		{StateHalfOpen, StateClosed}: true,
	}

	for _, change := range obs.stateChanges {
		if !validTransitions[change] {
			t.Fatalf("INVARIANT VIOLATED: invalid state transition %s → %s",
				change[0], change[1])
		}
	}

	expected := [][2]State{
		{StateClosed, StateOpen},
		{StateOpen, StateHalfOpen},
		{StateHalfOpen, StateOpen},
		{StateOpen, StateHalfOpen},
		{StateHalfOpen, StateClosed},
	}

	if len(obs.stateChanges) != len(expected) {
		t.Fatalf("expected %d transitions, got %d: %v",
			len(expected), len(obs.stateChanges), obs.stateChanges)
	}
	for i, e := range expected {
		if obs.stateChanges[i] != e {
			t.Fatalf("transition %d: expected %v, got %v", i, e, obs.stateChanges[i])
		}
	}
}
