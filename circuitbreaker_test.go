package flowguard

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

func TestCB_ClosedToOpen(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(3),
		WithCircuitBreakerClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }

	for i := 0; i < 3; i++ {
		cb.Do(ctx, fail)
	}

	if cb.State() != StateOpen {
		t.Fatalf("expected Open, got %v", cb.State())
	}

	// calls should be rejected now
	err := cb.Do(ctx, func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestCB_OpenToHalfOpen(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(2),
		WithOpenTimeout(5*time.Second),
		WithCircuitBreakerClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }

	cb.Do(ctx, fail)
	cb.Do(ctx, fail)

	if cb.State() != StateOpen {
		t.Fatal("should be open")
	}

	// advance past open timeout
	clk.Advance(6 * time.Second)

	if cb.State() != StateHalfOpen {
		t.Fatalf("expected HalfOpen, got %v", cb.State())
	}
}

func TestCB_HalfOpenToClosed(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(2),
		WithSuccessThreshold(2),
		WithOpenTimeout(5*time.Second),
		WithCircuitBreakerClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }
	ok := func(ctx context.Context) error { return nil }

	// trip it
	cb.Do(ctx, fail)
	cb.Do(ctx, fail)

	clk.Advance(6 * time.Second) // → half-open

	// two successes should close it
	cb.Do(ctx, ok)
	cb.Do(ctx, ok)

	if cb.State() != StateClosed {
		t.Fatalf("expected Closed, got %v", cb.State())
	}
}

func TestCB_HalfOpenBackToOpen(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(2),
		WithOpenTimeout(5*time.Second),
		WithCircuitBreakerClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }

	cb.Do(ctx, fail)
	cb.Do(ctx, fail)

	clk.Advance(6 * time.Second)

	// fail in half-open → back to open
	cb.Do(ctx, fail)

	if cb.State() != StateOpen {
		t.Fatalf("expected Open after half-open failure, got %v", cb.State())
	}
}

func TestCB_HalfOpenMaxCalls(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(1),
		WithHalfOpenMaxCalls(1),
		WithOpenTimeout(time.Second),
		WithCircuitBreakerClock(clk),
	)

	ctx := context.Background()
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })

	clk.Advance(2 * time.Second) // → half-open

	// first call in half-open — should succeed and take the slot
	// use a channel to block fn so the slot stays occupied
	gate := make(chan struct{})
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		err := cb.Do(ctx, func(ctx context.Context) error {
			close(started)
			<-gate // block until we say so
			return nil
		})
		done <- err
	}()

	<-started // fn is running, slot is taken
	// second call should get ErrTooManyRequests
	err := cb.Do(ctx, func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrTooManyRequests) {
		t.Fatalf("expected ErrTooManyRequests, got %v", err)
	}

	close(gate) // let the first call finish
	<-done
}

func TestCB_CustomTripFunc(t *testing.T) {
	clk := newMockClock(time.Now())
	// trip when failure rate > 50% and at least 4 requests
	cb := NewCircuitBreaker(
		WithTripFunc(func(c Counts) bool {
			return c.Requests >= 4 && c.TotalFailures*2 > c.Requests
		}),
		WithCircuitBreakerClock(clk),
	)

	ctx := context.Background()
	ok := func(ctx context.Context) error { return nil }
	fail := func(ctx context.Context) error { return errBoom }

	cb.Do(ctx, ok)
	cb.Do(ctx, fail)
	cb.Do(ctx, fail)

	// 3 requests, 2 failures — shouldn't trip yet (need 4+ requests)
	if cb.State() != StateClosed {
		t.Fatal("shouldn't have tripped yet")
	}

	cb.Do(ctx, fail)
	// 4 requests, 3 failures (75%) — should trip
	if cb.State() != StateOpen {
		t.Fatal("should have tripped with custom func")
	}
}

func TestCB_SuccessesResetCounter(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(3),
		WithCircuitBreakerClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }
	ok := func(ctx context.Context) error { return nil }

	// 2 failures, then a success should reset consecutive count
	cb.Do(ctx, fail)
	cb.Do(ctx, fail)
	cb.Do(ctx, ok) // resets ConsecutiveFailures

	cb.Do(ctx, fail)
	cb.Do(ctx, fail)
	// only 2 consecutive now, not 4
	if cb.State() != StateClosed {
		t.Fatal("should still be closed — success reset the counter")
	}
}
