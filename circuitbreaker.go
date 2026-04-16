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
//
// Two modes:
//   - Classic: trips after N consecutive failures (default)
//   - Adaptive: trips when error rate exceeds a threshold over a sliding window
type CircuitBreaker struct {
	mu sync.Mutex

	state  State
	counts Counts
	expiry time.Time // when Open state expires → transitions to HalfOpen

	// NOTE: we use a buffered channel as a semaphore for half-open probes.
	// Previously this was a plain int counter, but that had a subtle race
	// where two goroutines could both pass the check before either incremented.
	halfOpenSem chan struct{}

	failureThreshold int
	successThreshold int
	openTimeout      time.Duration
	halfOpenMaxCalls int
	tripFn           func(Counts) bool
	clock            Clock
	observer         Observer

	// --- adaptive / sliding window fields ---
	adaptive         bool
	windowSize       int     // how many recent results to track
	errorThreshold   float64 // 0.0-1.0, trip when error rate exceeds this
	windowResults    []bool  // ring buffer: true=success, false=failure
	windowIdx        int
	windowFilled     bool // have we filled the ring at least once?
	minWindowSamples int  // don't trip until we have at least this many samples
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
	cb.halfOpenSem = make(chan struct{}, cb.halfOpenMaxCalls)
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

// NewAdaptiveCircuitBreaker creates a circuit breaker that trips based on
// error rate over a sliding window, not just consecutive failures.
//
// Example: windowSize=100, errorThreshold=0.5 means "trip if >50% of the
// last 100 calls failed". This is way more robust than counting consecutive
// failures — a single success doesn't reset everything.
//
// minSamples is how many calls must happen before the error rate is checked.
// Set it to something reasonable (10-20) to avoid tripping on the first error.
func NewAdaptiveCircuitBreaker(windowSize int, errorThreshold float64, minSamples int, opts ...CircuitBreakerOption) *CircuitBreaker {
	cb := &CircuitBreaker{
		state:            StateClosed,
		successThreshold: 2,
		openTimeout:      30 * time.Second,
		halfOpenMaxCalls: 1,
		clock:            defaultClock{},
		observer:         noopObserver{},
		adaptive:         true,
		windowSize:       windowSize,
		errorThreshold:   errorThreshold,
		windowResults:    make([]bool, windowSize),
		minWindowSamples: minSamples,
	}
	for _, opt := range opts {
		opt(cb)
	}
	cb.halfOpenSem = make(chan struct{}, cb.halfOpenMaxCalls)
	return cb
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
		// try to acquire a half-open probe slot (non-blocking)
		select {
		case cb.halfOpenSem <- struct{}{}:
			cb.counts.Requests++
			return nil
		default:
			return ErrTooManyRequests
		}
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

	if cb.adaptive {
		cb.recordResult(true)
	}

	cb.observer.OnSuccess(lat)

	if cb.state == StateHalfOpen {
		<-cb.halfOpenSem
		if cb.counts.ConsecutiveSuccesses >= cb.successThreshold {
			cb.setState(StateClosed)
		}
	}
}

func (cb *CircuitBreaker) onFailure(err error, lat time.Duration) {
	cb.counts.TotalFailures++
	cb.counts.ConsecutiveFailures++
	cb.counts.ConsecutiveSuccesses = 0

	if cb.adaptive {
		cb.recordResult(false)
	}

	cb.observer.OnFailure(err, lat)

	switch cb.state {
	case StateClosed:
		if cb.shouldTrip() {
			cb.setState(StateOpen)
		}
	case StateHalfOpen:
		<-cb.halfOpenSem
		cb.setState(StateOpen)
	}
}

func (cb *CircuitBreaker) shouldTrip() bool {
	if cb.tripFn != nil {
		return cb.tripFn(cb.counts)
	}
	if cb.adaptive {
		return cb.adaptiveShouldTrip()
	}
	return cb.counts.ConsecutiveFailures >= cb.failureThreshold
}

// --- adaptive sliding window logic ---

func (cb *CircuitBreaker) recordResult(success bool) {
	cb.windowResults[cb.windowIdx] = success
	cb.windowIdx = (cb.windowIdx + 1) % cb.windowSize
	if cb.windowIdx == 0 {
		cb.windowFilled = true
	}
}

func (cb *CircuitBreaker) adaptiveShouldTrip() bool {
	n := cb.windowSize
	if !cb.windowFilled {
		n = cb.windowIdx
	}
	if n < cb.minWindowSamples {
		return false // not enough data yet
	}

	failures := 0
	for i := 0; i < n; i++ {
		if !cb.windowResults[i] {
			failures++
		}
	}
	rate := float64(failures) / float64(n)
	return rate > cb.errorThreshold
}

// ErrorRate returns the current error rate from the sliding window.
// Returns 0 if not enough samples yet. Only meaningful for adaptive CB.
func (cb *CircuitBreaker) ErrorRate() float64 {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if !cb.adaptive {
		return 0
	}
	n := cb.windowSize
	if !cb.windowFilled {
		n = cb.windowIdx
	}
	if n == 0 {
		return 0
	}
	failures := 0
	for i := 0; i < n; i++ {
		if !cb.windowResults[i] {
			failures++
		}
	}
	return float64(failures) / float64(n)
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
	if cb.adaptive {
		cb.windowIdx = 0
		cb.windowFilled = false
		for i := range cb.windowResults {
			cb.windowResults[i] = false
		}
	}

	if s == StateOpen {
		cb.expiry = cb.clock.Now().Add(cb.openTimeout)
	}
	if s == StateHalfOpen {
		// drain the semaphore so it's fresh
		for {
			select {
			case <-cb.halfOpenSem:
			default:
				return
			}
		}
	}
}

// checkExpiry transitions from Open → HalfOpen when the timeout passes.
// Must be called with lock held.
func (cb *CircuitBreaker) checkExpiry() {
	if cb.state == StateOpen && cb.clock.Now().After(cb.expiry) {
		cb.setState(StateHalfOpen)
	}
}
