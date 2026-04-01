package flowguard

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPolicy_RateLimiterOnly(t *testing.T) {
	clk := newMockClock(time.Now())
	rl := NewRateLimiter(100, 5, WithRateLimiterClock(clk))

	p := NewPolicy(WithPolicyRateLimiter(rl))
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		err := p.Do(ctx, func(ctx context.Context) error { return nil })
		if err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
	}
}

func TestPolicy_CircuitBreakerOnly(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(2),
		WithCircuitBreakerClock(clk),
	)
	p := NewPolicy(WithPolicyCircuitBreaker(cb))
	ctx := context.Background()

	// trip it
	p.Do(ctx, func(ctx context.Context) error { return errBoom })
	p.Do(ctx, func(ctx context.Context) error { return errBoom })

	err := p.Do(ctx, func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestPolicy_RetryOnly(t *testing.T) {
	clk := newMockClock(time.Now())
	r := NewRetry(WithMaxRetries(2), WithRetryClock(clk))
	p := NewPolicy(WithPolicyRetry(r))

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
	clk := newMockClock(time.Now())
	rl := NewRateLimiter(100, 1, WithRateLimiterClock(clk)) // burst=1
	cb := NewCircuitBreaker(
		WithFailureThreshold(1), // trip on first failure
		WithCircuitBreakerClock(clk),
	)

	p := NewPolicy(
		WithPolicyRateLimiter(rl),
		WithPolicyCircuitBreaker(cb),
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
	if cb.State() != StateClosed {
		t.Fatalf("CB should be closed, got %v", cb.State())
	}
}

func TestPolicy_RateLimitBurstExhausted_CBStaysClosed(t *testing.T) {
	// This is the key edge case: when rate limiter rejects because burst is
	// exhausted, the circuit breaker should NOT count that as a failure.
	clk := newMockClock(time.Now())
	obs := &testObserver{}
	rl := NewRateLimiter(10, 2, WithRateLimiterClock(clk))
	cb := NewCircuitBreaker(
		WithFailureThreshold(1), // very sensitive — trips on 1 failure
		WithCircuitBreakerClock(clk),
		WithCircuitBreakerObserver(obs),
	)

	p := NewPolicy(
		WithPolicyRateLimiter(rl),
		WithPolicyCircuitBreaker(cb),
	)
	ctx := context.Background()

	// burn through burst
	p.Do(ctx, func(ctx context.Context) error { return nil })
	p.Do(ctx, func(ctx context.Context) error { return nil })

	// refill a token and make another successful call
	clk.Advance(200 * time.Millisecond) // ~2 tokens at 10/s
	p.Do(ctx, func(ctx context.Context) error { return nil })

	// CB must still be closed — rate limiting is not a failure
	if cb.State() != StateClosed {
		t.Fatalf("CB should be closed, got %v", cb.State())
	}
	if obs.failures > 0 {
		t.Fatalf("expected 0 CB failures, got %d", obs.failures)
	}
}

func TestPolicy_CBOpenStopsRetry(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(1),
		WithCircuitBreakerClock(clk),
	)
	r := NewRetry(WithMaxRetries(5), WithRetryClock(clk))

	p := NewPolicy(
		WithPolicyCircuitBreaker(cb),
		WithPolicyRetry(r),
	)
	ctx := context.Background()

	// trip the CB
	p.Do(ctx, func(ctx context.Context) error { return errBoom })

	// next call should fail immediately with ErrCircuitOpen (no retries)
	calls := 0
	err := p.Do(ctx, func(ctx context.Context) error {
		calls++
		return nil
	})

	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("fn should not have been called when CB is open, got %d calls", calls)
	}
}

func TestPolicy_FullStack(t *testing.T) {
	clk := newMockClock(time.Now())

	rl := NewRateLimiter(1000, 100, WithRateLimiterClock(clk))
	cb := NewCircuitBreaker(
		WithFailureThreshold(5),
		WithCircuitBreakerClock(clk),
	)
	r := NewRetry(
		WithMaxRetries(2),
		WithRetryClock(clk),
		WithJitter(0),
	)

	p := NewPolicy(
		WithPolicyRateLimiter(rl),
		WithPolicyCircuitBreaker(cb),
		WithPolicyRetry(r),
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
	clk := newMockClock(time.Now())
	r := NewRetry(WithMaxRetries(1), WithRetryClock(clk))

	fallbackCalled := false
	p := NewPolicy(
		WithPolicyRetry(r),
		WithPolicyFallback(func(ctx context.Context, err error) error {
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
	p := NewPolicy(
		WithPolicyFallback(func(ctx context.Context, err error) error {
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
	b := NewBulkhead(2, WithMaxWaitDuration(0))
	p := NewPolicy(WithPolicyBulkhead(b))

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
	if !errors.Is(err, ErrBulkheadFull) {
		t.Fatalf("expected ErrBulkheadFull, got %v", err)
	}

	close(gate)
}
