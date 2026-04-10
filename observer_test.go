package flowguard

import (
	"context"
	"sync"
	"testing"
	"time"
)

// testObserver records events for assertion
type testObserver struct {
	mu           sync.Mutex
	successes    int
	failures     int
	retries      int
	stateChanges [][2]State
	rateLimited  int
}

func (o *testObserver) OnSuccess(time.Duration) {
	o.mu.Lock()
	o.successes++
	o.mu.Unlock()
}

func (o *testObserver) OnFailure(error, time.Duration) {
	o.mu.Lock()
	o.failures++
	o.mu.Unlock()
}

func (o *testObserver) OnRetry(attempt int, err error) {
	o.mu.Lock()
	o.retries++
	o.mu.Unlock()
}

func (o *testObserver) OnStateChange(from, to State) {
	o.mu.Lock()
	o.stateChanges = append(o.stateChanges, [2]State{from, to})
	o.mu.Unlock()
}

func (o *testObserver) OnRateLimited() {
	o.mu.Lock()
	o.rateLimited++
	o.mu.Unlock()
}

func TestObserver_CBStateChanges(t *testing.T) {
	obs := &testObserver{}
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(2),
		WithOpenTimeout(time.Second),
		WithCircuitBreakerClock(clk),
		WithCircuitBreakerObserver(obs),
	)

	ctx := context.Background()
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })
	cb.Do(ctx, func(ctx context.Context) error { return errBoom })

	if obs.failures != 2 {
		t.Fatalf("expected 2 failure events, got %d", obs.failures)
	}

	// should have Closed→Open
	if len(obs.stateChanges) != 1 || obs.stateChanges[0] != [2]State{StateClosed, StateOpen} {
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
	clk := newMockClock(time.Now())
	r := NewRetry(
		WithMaxRetries(3),
		WithRetryClock(clk),
		WithRetryObserver(obs),
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
	rl := NewRateLimiter(10, 1, WithRateLimiterObserver(obs))

	rl.Allow() // takes the token
	rl.Allow() // should trigger OnRateLimited

	if obs.rateLimited != 1 {
		t.Fatalf("expected 1 rate limited event, got %d", obs.rateLimited)
	}
}
