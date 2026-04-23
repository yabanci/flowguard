package flowguard_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/yabanci/flowguard"
	"github.com/yabanci/flowguard/bulkhead"
	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/hedge"
	"github.com/yabanci/flowguard/internal/fakeclock"
	"github.com/yabanci/flowguard/ratelimit"
	"github.com/yabanci/flowguard/retry"
)

// goroutineCheck captures goroutine count before a test and verifies
// it returns to baseline after. Catches leaked goroutines in Hedge,
// Retry, Wait, etc.
func goroutineCheck(t *testing.T) func() {
	t.Helper()
	before := runtime.NumGoroutine()
	return func() {
		t.Helper()
		// give goroutines a moment to wind down
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			after := runtime.NumGoroutine()
			// allow ±2 for test infrastructure goroutines
			if after <= before+2 {
				return
			}
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
		}
		after := runtime.NumGoroutine()
		if after > before+2 {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Errorf("goroutine leak: before=%d after=%d\n%s", before, after, buf[:n])
		}
	}
}

func TestLeak_HedgeFastSuccess(t *testing.T) {
	check := goroutineCheck(t)
	defer check()

	h := hedge.New(50 * time.Millisecond)
	for i := 0; i < 20; i++ {
		h.Do(context.Background(), func(ctx context.Context) error {
			return nil
		})
	}
}

func TestLeak_HedgeSlowPrimary(t *testing.T) {
	check := goroutineCheck(t)
	defer check()

	h := hedge.New(10 * time.Millisecond)
	for i := 0; i < 10; i++ {
		h.Do(context.Background(), func(ctx context.Context) error {
			select {
			case <-time.After(100 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}
}

func TestLeak_HedgeAllFail(t *testing.T) {
	check := goroutineCheck(t)
	defer check()

	h := hedge.New(5*time.Millisecond, hedge.WithMaxHedges(2))
	for i := 0; i < 10; i++ {
		h.Do(context.Background(), func(ctx context.Context) error {
			return errBoom
		})
	}
}

func TestLeak_HedgeCancelled(t *testing.T) {
	check := goroutineCheck(t)
	defer check()

	h := hedge.New(10 * time.Millisecond)
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		h.Do(ctx, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		cancel()
	}
}

func TestLeak_RetryExhausted(t *testing.T) {
	check := goroutineCheck(t)
	defer check()

	clk := fakeclock.New(time.Now())
	r := retry.New(retry.WithMaxRetries(5), retry.WithClock(clk), retry.WithJitter(0))

	for i := 0; i < 10; i++ {
		r.Do(context.Background(), func(ctx context.Context) error {
			return errBoom
		})
	}
}

func TestLeak_RateLimiterWait(t *testing.T) {
	check := goroutineCheck(t)
	defer check()

	rl := ratelimit.NewTokenBucket(1000, 10)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		rl.Wait(ctx)
	}
}

func TestLeak_RateLimiterWaitCancelled(t *testing.T) {
	check := goroutineCheck(t)
	defer check()

	rl := ratelimit.NewTokenBucket(1, 1)
	rl.Allow() // drain

	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		rl.Wait(ctx)
		cancel()
	}
}

func TestLeak_PolicyFullStack(t *testing.T) {
	check := goroutineCheck(t)
	defer check()

	clk := fakeclock.New(time.Now())
	p := flowguard.NewPolicy(
		flowguard.WithRateLimiter(ratelimit.NewTokenBucket(10000, 1000, ratelimit.WithClock(clk))),
		flowguard.WithCircuitBreaker(circuitbreaker.New(circuitbreaker.WithClock(clk))),
		flowguard.WithRetry(retry.New(retry.WithMaxRetries(2), retry.WithClock(clk), retry.WithJitter(0))),
		flowguard.WithBulkhead(bulkhead.New(100)),
	)

	for i := 0; i < 20; i++ {
		p.Do(context.Background(), func(ctx context.Context) error {
			return nil
		})
	}
}
