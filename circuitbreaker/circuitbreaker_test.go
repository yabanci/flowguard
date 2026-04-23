package circuitbreaker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yabanci/flowguard/internal/fakeclock"
	"github.com/yabanci/flowguard/observer"
)

var errBoom = errors.New("boom")

func TestCB_ClosedToOpen(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := New(
		WithFailureThreshold(3),
		WithClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }

	for i := 0; i < 3; i++ {
		cb.Do(ctx, fail)
	}

	if cb.State() != observer.StateOpen {
		t.Fatalf("expected Open, got %v", cb.State())
	}

	// calls should be rejected now
	err := cb.Do(ctx, func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("expected ErrOpen, got %v", err)
	}
}

func TestCB_OpenToHalfOpen(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := New(
		WithFailureThreshold(2),
		WithOpenTimeout(5*time.Second),
		WithClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }

	cb.Do(ctx, fail)
	cb.Do(ctx, fail)

	if cb.State() != observer.StateOpen {
		t.Fatal("should be open")
	}

	// advance past open timeout
	clk.Advance(6 * time.Second)

	if cb.State() != observer.StateHalfOpen {
		t.Fatalf("expected HalfOpen, got %v", cb.State())
	}
}

func TestCB_HalfOpenToClosed(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := New(
		WithFailureThreshold(2),
		WithSuccessThreshold(2),
		WithOpenTimeout(5*time.Second),
		WithClock(clk),
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

	if cb.State() != observer.StateClosed {
		t.Fatalf("expected Closed, got %v", cb.State())
	}
}

func TestCB_HalfOpenBackToOpen(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := New(
		WithFailureThreshold(2),
		WithOpenTimeout(5*time.Second),
		WithClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }

	cb.Do(ctx, fail)
	cb.Do(ctx, fail)

	clk.Advance(6 * time.Second)

	// fail in half-open → back to open
	cb.Do(ctx, fail)

	if cb.State() != observer.StateOpen {
		t.Fatalf("expected Open after half-open failure, got %v", cb.State())
	}
}

func TestCB_HalfOpenMaxCalls(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := New(
		WithFailureThreshold(1),
		WithHalfOpenMaxCalls(1),
		WithOpenTimeout(time.Second),
		WithClock(clk),
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
	clk := fakeclock.New(time.Now())
	// trip when failure rate > 50% and at least 4 requests
	cb := New(
		WithTripFunc(func(c Counts) bool {
			return c.Requests >= 4 && c.TotalFailures*2 > c.Requests
		}),
		WithClock(clk),
	)

	ctx := context.Background()
	ok := func(ctx context.Context) error { return nil }
	fail := func(ctx context.Context) error { return errBoom }

	cb.Do(ctx, ok)
	cb.Do(ctx, fail)
	cb.Do(ctx, fail)

	// 3 requests, 2 failures — shouldn't trip yet (need 4+ requests)
	if cb.State() != observer.StateClosed {
		t.Fatal("shouldn't have tripped yet")
	}

	cb.Do(ctx, fail)
	// 4 requests, 3 failures (75%) — should trip
	if cb.State() != observer.StateOpen {
		t.Fatal("should have tripped with custom func")
	}
}

func TestCB_SuccessesResetCounter(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := New(
		WithFailureThreshold(3),
		WithClock(clk),
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
	if cb.State() != observer.StateClosed {
		t.Fatal("should still be closed — success reset the counter")
	}
}

// --- adaptive circuit breaker tests ---

func TestAdaptiveCB_TripsOnErrorRate(t *testing.T) {
	clk := fakeclock.New(time.Now())
	// trip when >50% of last 10 calls fail, need at least 5 samples
	cb := NewAdaptive(10, 0.5, 5,
		WithClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }
	ok := func(ctx context.Context) error { return nil }

	// 4 failures — not enough samples yet (need 5)
	for i := 0; i < 4; i++ {
		cb.Do(ctx, fail)
	}
	if cb.State() != observer.StateClosed {
		t.Fatal("shouldn't trip yet — not enough samples")
	}

	// 5th failure — now 5/5 = 100% error rate, should trip
	cb.Do(ctx, fail)
	if cb.State() != observer.StateOpen {
		t.Fatalf("should have tripped, error rate is 100%%, state=%v", cb.State())
	}

	// wait for half-open
	clk.Advance(31 * time.Second)

	// succeed twice to close
	cb.Do(ctx, ok)
	cb.Do(ctx, ok)
	if cb.State() != observer.StateClosed {
		t.Fatal("should be closed after successes in half-open")
	}
}

func TestAdaptiveCB_DoesNotTripBelowThreshold(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := NewAdaptive(20, 0.5, 10,
		WithClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }
	ok := func(ctx context.Context) error { return nil }

	// 7 successes, 3 failures = 30% error rate
	for i := 0; i < 7; i++ {
		cb.Do(ctx, ok)
	}
	for i := 0; i < 3; i++ {
		cb.Do(ctx, fail)
	}

	// 30% < 50% threshold — should stay closed
	if cb.State() != observer.StateClosed {
		t.Fatal("should stay closed at 30% error rate")
	}
}

func TestAdaptiveCB_SlidingWindowEvicts(t *testing.T) {
	clk := fakeclock.New(time.Now())
	// window of 5, trip at >50%, min 3 samples
	cb := NewAdaptive(5, 0.5, 3,
		WithClock(clk),
	)

	ctx := context.Background()
	fail := func(ctx context.Context) error { return errBoom }
	ok := func(ctx context.Context) error { return nil }

	// fill window with failures: [F, F, F, F, F] = 100%
	for i := 0; i < 5; i++ {
		cb.Do(ctx, fail)
	}
	// this trips it
	if cb.State() != observer.StateOpen {
		t.Fatal("should have tripped")
	}

	// recover
	clk.Advance(31 * time.Second)
	cb.Do(ctx, ok)
	cb.Do(ctx, ok)
	// now closed again, window is reset

	// add 3 successes: [S, S, S] — 0% error
	cb.Do(ctx, ok)
	cb.Do(ctx, ok)
	cb.Do(ctx, ok)

	// add 1 failure: [S, S, S, F] = 25% — still under 50%
	cb.Do(ctx, fail)
	if cb.State() != observer.StateClosed {
		t.Fatal("should stay closed at 25%")
	}
}

func TestAdaptiveCB_ErrorRate(t *testing.T) {
	cb := NewAdaptive(10, 0.8, 5)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		cb.Do(ctx, func(ctx context.Context) error { return nil })
	}
	for i := 0; i < 2; i++ {
		cb.Do(ctx, func(ctx context.Context) error { return errBoom })
	}

	rate := cb.ErrorRate()
	// 2 failures out of 5 = 0.4
	if rate < 0.39 || rate > 0.41 {
		t.Fatalf("expected ~0.4 error rate, got %f", rate)
	}
}
