package flowguard_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yabanci/flowguard"
	"github.com/yabanci/flowguard/bulkhead"
	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/internal/fakeclock"
	"github.com/yabanci/flowguard/observer"
	"github.com/yabanci/flowguard/ratelimit"
	"github.com/yabanci/flowguard/retry"
)

func TestPolicy_RateLimiterOnly(t *testing.T) {
	clk := fakeclock.New(time.Now())
	rl := ratelimit.NewTokenBucket(100, 5, ratelimit.WithClock(clk))

	p := flowguard.NewPolicy(flowguard.WithRateLimiter(rl))
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		err := p.Do(ctx, func(ctx context.Context) error { return nil })
		if err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
	}
}

func TestPolicy_CircuitBreakerOnly(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithClock(clk),
	)
	p := flowguard.NewPolicy(flowguard.WithCircuitBreaker(cb))
	ctx := context.Background()

	// trip it
	p.Do(ctx, func(ctx context.Context) error { return errBoom })
	p.Do(ctx, func(ctx context.Context) error { return errBoom })

	err := p.Do(ctx, func(ctx context.Context) error { return nil })
	if !errors.Is(err, circuitbreaker.ErrOpen) {
		t.Fatalf("expected circuitbreaker.ErrOpen, got %v", err)
	}
}

func TestPolicy_RetryOnly(t *testing.T) {
	clk := fakeclock.New(time.Now())
	r := retry.New(retry.WithMaxRetries(2), retry.WithClock(clk))
	p := flowguard.NewPolicy(flowguard.WithRetry(r))

	calls := 0
	err := p.Do(context.Background(), func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return errBoom
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestPolicy_RateLimitDoesNotTripCB(t *testing.T) {
	clk := fakeclock.New(time.Now())
	rl := ratelimit.NewTokenBucket(100, 1, ratelimit.WithClock(clk)) // burst=1
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1), // trip on first failure
		circuitbreaker.WithClock(clk),
	)

	p := flowguard.NewPolicy(
		flowguard.WithRateLimiter(rl),
		flowguard.WithCircuitBreaker(cb),
	)
	ctx := context.Background()

	// first call succeeds (uses the one token)
	err := p.Do(ctx, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("first call should succeed: %v", err)
	}

	// now the limiter needs to refill — advance time so Wait doesn't block forever
	clk.Advance(time.Second)

	// this call should go through (token refilled)
	err = p.Do(ctx, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("second call should succeed: %v", err)
	}

	// CB should still be closed even though RL rejected some
	if cb.State() != observer.StateClosed {
		t.Fatalf("CB should be closed, got %v", cb.State())
	}
}

func TestPolicy_RateLimitBurstExhausted_CBStaysClosed(t *testing.T) {
	// This is the key edge case: when rate limiter rejects because burst is
	// exhausted, the circuit breaker should NOT count that as a failure.
	clk := fakeclock.New(time.Now())
	obs := &testObserver{}
	rl := ratelimit.NewTokenBucket(10, 2, ratelimit.WithClock(clk))
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1), // very sensitive — trips on 1 failure
		circuitbreaker.WithClock(clk),
		circuitbreaker.WithObserver(obs),
	)

	p := flowguard.NewPolicy(
		flowguard.WithRateLimiter(rl),
		flowguard.WithCircuitBreaker(cb),
	)
	ctx := context.Background()

	// burn through burst
	p.Do(ctx, func(ctx context.Context) error { return nil })
	p.Do(ctx, func(ctx context.Context) error { return nil })

	// refill a token and make another successful call
	clk.Advance(200 * time.Millisecond) // ~2 tokens at 10/s
	p.Do(ctx, func(ctx context.Context) error { return nil })

	// CB must still be closed — rate limiting is not a failure
	if cb.State() != observer.StateClosed {
		t.Fatalf("CB should be closed, got %v", cb.State())
	}
	if obs.failures > 0 {
		t.Fatalf("expected 0 CB failures, got %d", obs.failures)
	}
}

func TestPolicy_CBOpenStopsRetry(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithClock(clk),
	)
	r := retry.New(retry.WithMaxRetries(5), retry.WithClock(clk))

	p := flowguard.NewPolicy(
		flowguard.WithCircuitBreaker(cb),
		flowguard.WithRetry(r),
	)
	ctx := context.Background()

	// trip the CB
	p.Do(ctx, func(ctx context.Context) error { return errBoom })

	// next call should fail immediately with circuitbreaker.ErrOpen (no retries)
	calls := 0
	err := p.Do(ctx, func(ctx context.Context) error {
		calls++
		return nil
	})

	if !errors.Is(err, circuitbreaker.ErrOpen) {
		t.Fatalf("expected circuitbreaker.ErrOpen, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("fn should not have been called when CB is open, got %d calls", calls)
	}
}

func TestPolicy_FullStack(t *testing.T) {
	clk := fakeclock.New(time.Now())

	rl := ratelimit.NewTokenBucket(1000, 100, ratelimit.WithClock(clk))
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(5),
		circuitbreaker.WithClock(clk),
	)
	r := retry.New(
		retry.WithMaxRetries(2),
		retry.WithClock(clk),
		retry.WithJitter(0),
	)

	p := flowguard.NewPolicy(
		flowguard.WithRateLimiter(rl),
		flowguard.WithCircuitBreaker(cb),
		flowguard.WithRetry(r),
	)

	calls := 0
	err := p.Do(context.Background(), func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return errBoom
		}
		return nil
	})

	if err != nil {
		t.Fatalf("expected success on retry, got: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestPolicy_Fallback(t *testing.T) {
	clk := fakeclock.New(time.Now())
	r := retry.New(retry.WithMaxRetries(1), retry.WithClock(clk))

	fallbackCalled := false
	p := flowguard.NewPolicy(
		flowguard.WithRetry(r),
		flowguard.WithFallback(func(ctx context.Context, err error) error {
			fallbackCalled = true
			return nil // recover
		}),
	)

	err := p.Do(context.Background(), func(ctx context.Context) error {
		return errBoom
	})

	if err != nil {
		t.Fatalf("fallback should have recovered, got: %v", err)
	}
	if !fallbackCalled {
		t.Fatal("fallback was not called")
	}
}

func TestPolicy_FallbackNotCalledOnSuccess(t *testing.T) {
	fallbackCalled := false
	p := flowguard.NewPolicy(
		flowguard.WithFallback(func(ctx context.Context, err error) error {
			fallbackCalled = true
			return nil
		}),
	)

	err := p.Do(context.Background(), func(ctx context.Context) error {
		return nil
	})

	if err != nil {
		t.Fatal("unexpected error")
	}
	if fallbackCalled {
		t.Fatal("fallback should not be called on success")
	}
}

func TestPolicy_WithBulkhead(t *testing.T) {
	b := bulkhead.New(2, bulkhead.WithMaxWait(0))
	p := flowguard.NewPolicy(flowguard.WithBulkhead(b))

	ctx := context.Background()
	gate := make(chan struct{})

	// fill both slots
	for i := 0; i < 2; i++ {
		go p.Do(ctx, func(ctx context.Context) error {
			<-gate
			return nil
		})
	}
	time.Sleep(10 * time.Millisecond) // let them acquire

	// third should fail
	err := p.Do(ctx, func(ctx context.Context) error { return nil })
	if !errors.Is(err, bulkhead.ErrFull) {
		t.Fatalf("expected bulkhead.ErrFull, got %v", err)
	}

	close(gate)
}
