package flowguard

import (
	"context"
	"sync"
	"time"
)

// Counts tracks request stats for the circuit breaker.
// Exposed so users can write custom TripFunc logic.
type Counts struct {
	Requests             int
	TotalSuccesses       int
	TotalFailures        int
	ConsecutiveSuccesses int
	ConsecutiveFailures  int
}

// CircuitBreaker implements the circuit breaker pattern.
// Three states: Closed (normal) → Open (failing) → HalfOpen (probing).
type CircuitBreaker struct {
	mu sync.Mutex

	state    State
	counts   Counts
	expiry   time.Time // when Open state expires → transitions to HalfOpen
	halfOpen int       // current number of in-flight half-open probes

	failureThreshold  int
	successThreshold  int
	openTimeout       time.Duration
	halfOpenMaxCalls  int
	tripFn            func(Counts) bool
	clock             Clock
	observer          Observer
}

// CircuitBreakerOption configures a CircuitBreaker.
type CircuitBreakerOption func(*CircuitBreaker)

// NewCircuitBreaker creates a circuit breaker with sensible defaults.
// Trips after 5 consecutive failures, probes after 30s, needs 2 successes to close.
func NewCircuitBreaker(opts ...CircuitBreakerOption) *CircuitBreaker {
	cb := &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: 5,
		successThreshold: 2,
		openTimeout:      30 * time.Second,
		halfOpenMaxCalls: 1,
		clock:            defaultClock{},
		observer:         noopObserver{},
	}
	for _, opt := range opts {
		opt(cb)
	}
	return cb
}

func WithFailureThreshold(n int) CircuitBreakerOption {
	return func(cb *CircuitBreaker) { cb.failureThreshold = n }
}

func WithSuccessThreshold(n int) CircuitBreakerOption {
	return func(cb *CircuitBreaker) { cb.successThreshold = n }
}

func WithOpenTimeout(d time.Duration) CircuitBreakerOption {
	return func(cb *CircuitBreaker) { cb.openTimeout = d }
}

func WithHalfOpenMaxCalls(n int) CircuitBreakerOption {
	return func(cb *CircuitBreaker) { cb.halfOpenMaxCalls = n }
}

// WithTripFunc sets a custom function to decide when to trip.
// If set, it replaces the default consecutive-failure check.
func WithTripFunc(fn func(Counts) bool) CircuitBreakerOption {
	return func(cb *CircuitBreaker) { cb.tripFn = fn }
}

func WithCircuitBreakerClock(c Clock) CircuitBreakerOption {
	return func(cb *CircuitBreaker) { cb.clock = c }
}

func WithCircuitBreakerObserver(o Observer) CircuitBreakerOption {
	return func(cb *CircuitBreaker) { cb.observer = o }
}

// Do runs fn through the circuit breaker.
// Returns ErrCircuitOpen or ErrTooManyRequests if the call is rejected.
func (cb *CircuitBreaker) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := cb.beforeCall(); err != nil {
		return err
	}

	start := cb.clock.Now()
	err := fn(ctx)
	lat := cb.clock.Now().Sub(start)

	cb.afterCall(err, lat)
	return err
}

// State returns the current state. Thread-safe.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.checkExpiry()
	return cb.state
}

// GetCounts returns current counts. Mostly for debugging/testing.
func (cb *CircuitBreaker) GetCounts() Counts {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.counts
}

func (cb *CircuitBreaker) beforeCall() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.checkExpiry()

	switch cb.state {
	case StateClosed:
		cb.counts.Requests++
		return nil
	case StateOpen:
		return ErrCircuitOpen
	case StateHalfOpen:
		if cb.halfOpen >= cb.halfOpenMaxCalls {
			return ErrTooManyRequests
		}
		cb.halfOpen++
		cb.counts.Requests++
		return nil
	}
	return nil
}

func (cb *CircuitBreaker) afterCall(err error, lat time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err == nil {
		cb.onSuccess(lat)
	} else {
		cb.onFailure(err, lat)
	}
}

func (cb *CircuitBreaker) onSuccess(lat time.Duration) {
	cb.counts.TotalSuccesses++
	cb.counts.ConsecutiveSuccesses++
	cb.counts.ConsecutiveFailures = 0

	cb.observer.OnSuccess(lat)

	if cb.state == StateHalfOpen {
		cb.halfOpen--
		if cb.counts.ConsecutiveSuccesses >= cb.successThreshold {
			cb.setState(StateClosed)
		}
	}
}

func (cb *CircuitBreaker) onFailure(err error, lat time.Duration) {
	cb.counts.TotalFailures++
	cb.counts.ConsecutiveFailures++
	cb.counts.ConsecutiveSuccesses = 0

	cb.observer.OnFailure(err, lat)

	switch cb.state {
	case StateClosed:
		if cb.shouldTrip() {
			cb.setState(StateOpen)
		}
	case StateHalfOpen:
		cb.halfOpen--
		// any failure in half-open → back to open
		cb.setState(StateOpen)
	}
}

func (cb *CircuitBreaker) shouldTrip() bool {
	if cb.tripFn != nil {
		return cb.tripFn(cb.counts)
	}
	return cb.counts.ConsecutiveFailures >= cb.failureThreshold
}

func (cb *CircuitBreaker) setState(s State) {
	if cb.state == s {
		return
	}
	prev := cb.state
	cb.state = s
	cb.observer.OnStateChange(prev, s)

	// reset counters on state change
	cb.counts = Counts{}
	cb.halfOpen = 0

	if s == StateOpen {
		cb.expiry = cb.clock.Now().Add(cb.openTimeout)
	}
}

// checkExpiry transitions from Open → HalfOpen when the timeout passes.
// Must be called with lock held.
func (cb *CircuitBreaker) checkExpiry() {
	if cb.state == StateOpen && cb.clock.Now().After(cb.expiry) {
		cb.setState(StateHalfOpen)
	}
}
