package flowguard_test

import (
	"context"
	"testing"
	"time"

	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/internal/fakeclock"
	"github.com/yabanci/flowguard/observer"
	"github.com/yabanci/flowguard/ratelimit"
	"github.com/yabanci/flowguard/retry"
)

func TestObserver_CBStateChanges(t *testing.T) {
	obs := &testObserver{}
	clk := fakeclock.New(time.Now())
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithOpenTimeout(time.Second),
		circuitbreaker.WithClock(clk),
		circuitbreaker.WithObserver(obs),
	)

	ctx := context.Background()
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })

	if obs.failures != 2 {
		t.Fatalf("expected 2 failure events, got %d", obs.failures)
	}

	// should have Closed→Open
	if len(obs.stateChanges) != 1 || obs.stateChanges[0] != [2]observer.State{observer.StateClosed, observer.StateOpen} {
		t.Fatalf("unexpected state changes: %v", obs.stateChanges)
	}

	clk.Advance(2 * time.Second)
	cb.State() // trigger transition

	// should add Open→HalfOpen
	if len(obs.stateChanges) != 2 {
		t.Fatalf("expected 2 state changes, got %d", len(obs.stateChanges))
	}
}

func TestObserver_RetryEvents(t *testing.T) {
	obs := &testObserver{}
	clk := fakeclock.New(time.Now())
	r := retry.New(
		retry.WithMaxRetries(3),
		retry.WithClock(clk),
		retry.WithObserver(obs),
	)

	r.Do(context.Background(), func(ctx context.Context) error {
		return errBoom
	})

	if obs.retries != 3 {
		t.Fatalf("expected 3 retry events, got %d", obs.retries)
	}
}

func TestObserver_RateLimited(t *testing.T) {
	obs := &testObserver{}
	rl := ratelimit.NewTokenBucket(10, 1, ratelimit.WithObserver(obs))

	rl.Allow() // takes the token
	rl.Allow() // should trigger OnRateLimited

	if obs.rateLimited != 1 {
		t.Fatalf("expected 1 rate limited event, got %d", obs.rateLimited)
	}
}
