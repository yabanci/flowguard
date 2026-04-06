package flowguard

import (
	"context"
	"testing"
	"time"
)

func FuzzRateLimiter(f *testing.F) {
	f.Add(float64(10), 5)
	f.Add(float64(1), 1)
	f.Add(float64(1000), 100)
	f.Add(float64(0.1), 1)

	f.Fuzz(func(t *testing.T, rate float64, burst int) {
		if rate <= 0 || rate > 1e9 || burst < 0 || burst > 1e6 {
			t.Skip()
		}

		rl := NewRateLimiter(rate, burst)

		// should never panic
		for i := 0; i < burst+5; i++ {
			rl.Allow()
		}
		rl.Reserve()
	})
}

func FuzzCircuitBreaker(f *testing.F) {
	f.Add(1, 1, 100, true)
	f.Add(5, 2, 1000, false)
	f.Add(10, 3, 500, true)

	f.Fuzz(func(t *testing.T, failThresh, successThresh, timeout int, shouldFail bool) {
		if failThresh <= 0 || failThresh > 100 {
			t.Skip()
		}
		if successThresh <= 0 || successThresh > 100 {
			t.Skip()
		}
		if timeout <= 0 || timeout > 60000 {
			t.Skip()
		}

		clk := newMockClock(time.Now())
		cb := NewCircuitBreaker(
			WithFailureThreshold(failThresh),
			WithSuccessThreshold(successThresh),
			WithOpenTimeout(time.Duration(timeout)*time.Millisecond),
			WithCircuitBreakerClock(clk),
		)

		ctx := context.Background()
		fn := func(ctx context.Context) error {
			if shouldFail {
				return errBoom
			}
			return nil
		}

		// run a bunch of calls — should never panic
		for i := 0; i < failThresh+successThresh+10; i++ {
			cb.Do(ctx, fn)
		}

		// check state is valid
		s := cb.State()
		if s != StateClosed && s != StateOpen && s != StateHalfOpen {
			t.Fatalf("invalid state: %v", s)
		}

		// advance time and try again
		clk.Advance(time.Duration(timeout+1) * time.Millisecond)
		cb.Do(ctx, func(ctx context.Context) error { return nil })

		s = cb.State()
		if s != StateClosed && s != StateOpen && s != StateHalfOpen {
			t.Fatalf("invalid state after recovery: %v", s)
		}
	})
}

func FuzzRetry(f *testing.F) {
	f.Add(0, 10, true)
	f.Add(3, 100, false)
	f.Add(5, 1000, true)

	f.Fuzz(func(t *testing.T, maxRetries, backoffMs int, failAll bool) {
		if maxRetries < 0 || maxRetries > 20 {
			t.Skip()
		}
		if backoffMs < 0 || backoffMs > 10000 {
			t.Skip()
		}

		clk := newMockClock(time.Now())
		r := NewRetry(
			WithMaxRetries(maxRetries),
			WithExponentialBackoff(time.Duration(backoffMs)*time.Millisecond),
			WithMaxBackoff(time.Duration(backoffMs*10)*time.Millisecond),
			WithJitter(0),
			WithRetryClock(clk),
		)

		calls := 0
		err := r.Do(context.Background(), func(ctx context.Context) error {
			calls++
			if failAll {
				return errBoom
			}
			if calls <= maxRetries/2 {
				return errBoom
			}
			return nil
		})

		// invariant: never more than maxRetries+1 calls
		if calls > maxRetries+1 {
			t.Fatalf("too many calls: %d (max retries: %d)", calls, maxRetries)
		}

		if failAll {
			if err == nil {
				t.Fatal("should have failed")
			}
		}
	})
}

func FuzzAdaptiveCB(f *testing.F) {
	f.Add(10, 50, 5, 7) // windowSize, thresholdPct, minSamples, numFailures
	f.Add(5, 80, 3, 4)
	f.Add(100, 30, 10, 20)

	f.Fuzz(func(t *testing.T, windowSize, thresholdPct, minSamples, numFailures int) {
		if windowSize <= 0 || windowSize > 1000 {
			t.Skip()
		}
		if thresholdPct <= 0 || thresholdPct >= 100 {
			t.Skip()
		}
		if minSamples <= 0 || minSamples > windowSize {
			t.Skip()
		}
		if numFailures < 0 || numFailures > windowSize*2 {
			t.Skip()
		}

		threshold := float64(thresholdPct) / 100.0

		clk := newMockClock(time.Now())
		cb := NewAdaptiveCircuitBreaker(windowSize, threshold, minSamples,
			WithCircuitBreakerClock(clk),
			WithOpenTimeout(time.Second),
		)

		ctx := context.Background()
		for i := 0; i < numFailures; i++ {
			cb.Do(ctx, func(ctx context.Context) error { return errBoom })
		}

		// should never panic, state should be valid
		s := cb.State()
		if s != StateClosed && s != StateOpen && s != StateHalfOpen {
			t.Fatalf("invalid state: %v", s)
		}

		// error rate should be between 0 and 1
		rate := cb.ErrorRate()
		if rate < 0 || rate > 1 {
			t.Fatalf("invalid error rate: %f", rate)
		}
	})
}

func FuzzLoadShedder(f *testing.F) {
	f.Add(10, 5, 100)
	f.Add(1, 1, 10)
	f.Add(50, 5, 200)

	f.Fuzz(func(t *testing.T, initialLimit, minLimit, maxLimit int) {
		if initialLimit <= 0 || initialLimit > 10000 {
			t.Skip()
		}
		if minLimit <= 0 || minLimit > initialLimit {
			t.Skip()
		}
		if maxLimit < initialLimit || maxLimit > 100000 {
			t.Skip()
		}

		ls := NewLoadShedder(initialLimit, 50*time.Millisecond,
			WithLoadShedLimits(minLimit, maxLimit),
		)

		ctx := context.Background()

		// run some calls
		for i := 0; i < 20; i++ {
			ls.Do(ctx, func(ctx context.Context) error { return nil })
		}

		// invariant: limit stays in bounds
		lim := ls.CurrentLimit()
		if lim < minLimit || lim > maxLimit {
			t.Fatalf("limit %d outside bounds [%d, %d]", lim, minLimit, maxLimit)
		}

		// decrease should stay in bounds
		ls.Do(ctx, func(ctx context.Context) error { return errBoom })
		lim = ls.CurrentLimit()
		if lim < minLimit {
			t.Fatalf("limit %d below min %d after decrease", lim, minLimit)
		}
	})
}
