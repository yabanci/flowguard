// Package circuitbreaker implements the circuit breaker pattern in two
// modes: classic (trips after N consecutive failures) and adaptive (trips
// when the error rate over a sliding window exceeds a threshold).
package circuitbreaker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yabanci/flowguard/clock"
	"github.com/yabanci/flowguard/observer"
)

var (
	// ErrOpen is returned when a call is rejected because the breaker is open.
	ErrOpen = errors.New("flowguard/circuitbreaker: open")

	// ErrTooManyRequests is returned in half-open state when concurrent
	// probes already occupy every available slot.
	ErrTooManyRequests = errors.New("flowguard/circuitbreaker: too many requests in half-open state")
)

// Counts tracks request stats. Exposed so users can write custom TripFunc logic.
type Counts struct {
	Requests             int
	TotalSuccesses       int
	TotalFailures        int
	ConsecutiveSuccesses int
	ConsecutiveFailures  int
}

// Breaker is the circuit breaker. See New and NewAdaptive.
// Three states: Closed (normal) → Open (failing) → HalfOpen (probing).
type Breaker struct {
	mu sync.Mutex

	state  observer.State
	counts Counts
	expiry time.Time

	halfOpenSem chan struct{}

	failureThreshold int
	successThreshold int
	openTimeout      time.Duration
	halfOpenMaxCalls int
	tripFn           func(Counts) bool
	clock            clock.Clock
	observer         observer.Observer

	adaptive         bool
	windowSize       int
	errorThreshold   float64
	windowResults    []bool
	windowIdx        int
	windowFilled     bool
	minWindowSamples int
}

// Option configures a Breaker.
type Option func(*Breaker)

// New creates a breaker with sensible defaults: trips after 5 consecutive
// failures, probes after 30s, needs 2 successes to close.
func New(opts ...Option) *Breaker {
	b := &Breaker{
		state:            observer.StateClosed,
		failureThreshold: 5,
		successThreshold: 2,
		openTimeout:      30 * time.Second,
		halfOpenMaxCalls: 1,
		clock:            clock.Real(),
		observer:         observer.Noop{},
	}
	for _, opt := range opts {
		opt(b)
	}
	b.halfOpenSem = make(chan struct{}, b.halfOpenMaxCalls)
	return b
}

func WithFailureThreshold(n int) Option {
	return func(b *Breaker) { b.failureThreshold = n }
}

func WithSuccessThreshold(n int) Option {
	return func(b *Breaker) { b.successThreshold = n }
}

func WithOpenTimeout(d time.Duration) Option {
	return func(b *Breaker) { b.openTimeout = d }
}

func WithHalfOpenMaxCalls(n int) Option {
	return func(b *Breaker) { b.halfOpenMaxCalls = n }
}

// WithTripFunc replaces the default consecutive-failure check with a
// custom predicate over Counts.
func WithTripFunc(fn func(Counts) bool) Option {
	return func(b *Breaker) { b.tripFn = fn }
}

func WithClock(c clock.Clock) Option {
	return func(b *Breaker) { b.clock = c }
}

func WithObserver(o observer.Observer) Option {
	return func(b *Breaker) { b.observer = o }
}

// NewAdaptive creates a breaker that trips based on error rate over a
// sliding window. windowSize is how many recent results to track;
// errorThreshold is the fraction (0..1) that trips the breaker;
// minSamples is the minimum number of calls before the rate is checked.
func NewAdaptive(windowSize int, errorThreshold float64, minSamples int, opts ...Option) *Breaker {
	b := &Breaker{
		state:            observer.StateClosed,
		successThreshold: 2,
		openTimeout:      30 * time.Second,
		halfOpenMaxCalls: 1,
		clock:            clock.Real(),
		observer:         observer.Noop{},
		adaptive:         true,
		windowSize:       windowSize,
		errorThreshold:   errorThreshold,
		windowResults:    make([]bool, windowSize),
		minWindowSamples: minSamples,
	}
	for _, opt := range opts {
		opt(b)
	}
	b.halfOpenSem = make(chan struct{}, b.halfOpenMaxCalls)
	return b
}

// Do runs fn through the circuit breaker.
// Returns ErrOpen or ErrTooManyRequests if the call is rejected.
func (b *Breaker) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := b.beforeCall(); err != nil {
		return err
	}

	start := b.clock.Now()
	err := fn(ctx)
	lat := b.clock.Now().Sub(start)

	b.afterCall(err, lat)
	return err
}

// State returns the current state. Thread-safe.
func (b *Breaker) State() observer.State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checkExpiry()
	return b.state
}

// GetCounts returns current counts. Mostly for debugging/testing.
func (b *Breaker) GetCounts() Counts {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts
}

// ErrorRate returns the current error rate from the sliding window.
// Returns 0 if not enough samples yet. Only meaningful for adaptive breakers.
func (b *Breaker) ErrorRate() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.adaptive {
		return 0
	}
	n := b.windowSize
	if !b.windowFilled {
		n = b.windowIdx
	}
	if n == 0 {
		return 0
	}
	failures := 0
	for i := 0; i < n; i++ {
		if !b.windowResults[i] {
			failures++
		}
	}
	return float64(failures) / float64(n)
}

func (b *Breaker) beforeCall() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.checkExpiry()

	switch b.state {
	case observer.StateClosed:
		b.counts.Requests++
		return nil
	case observer.StateOpen:
		return ErrOpen
	case observer.StateHalfOpen:
		select {
		case b.halfOpenSem <- struct{}{}:
			b.counts.Requests++
			return nil
		default:
			return ErrTooManyRequests
		}
	}
	return nil
}

func (b *Breaker) afterCall(err error, lat time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err == nil {
		b.onSuccess(lat)
	} else {
		b.onFailure(err, lat)
	}
}

func (b *Breaker) onSuccess(lat time.Duration) {
	b.counts.TotalSuccesses++
	b.counts.ConsecutiveSuccesses++
	b.counts.ConsecutiveFailures = 0

	if b.adaptive {
		b.recordResult(true)
	}

	b.observer.OnSuccess(lat)

	if b.state == observer.StateHalfOpen {
		<-b.halfOpenSem
		if b.counts.ConsecutiveSuccesses >= b.successThreshold {
			b.setState(observer.StateClosed)
		}
	}
}

func (b *Breaker) onFailure(err error, lat time.Duration) {
	b.counts.TotalFailures++
	b.counts.ConsecutiveFailures++
	b.counts.ConsecutiveSuccesses = 0

	if b.adaptive {
		b.recordResult(false)
	}

	b.observer.OnFailure(err, lat)

	switch b.state {
	case observer.StateClosed:
		if b.shouldTrip() {
			b.setState(observer.StateOpen)
		}
	case observer.StateHalfOpen:
		<-b.halfOpenSem
		b.setState(observer.StateOpen)
	}
}

func (b *Breaker) shouldTrip() bool {
	if b.tripFn != nil {
		return b.tripFn(b.counts)
	}
	if b.adaptive {
		return b.adaptiveShouldTrip()
	}
	return b.counts.ConsecutiveFailures >= b.failureThreshold
}

func (b *Breaker) recordResult(success bool) {
	b.windowResults[b.windowIdx] = success
	b.windowIdx = (b.windowIdx + 1) % b.windowSize
	if b.windowIdx == 0 {
		b.windowFilled = true
	}
}

func (b *Breaker) adaptiveShouldTrip() bool {
	n := b.windowSize
	if !b.windowFilled {
		n = b.windowIdx
	}
	if n < b.minWindowSamples {
		return false
	}

	failures := 0
	for i := 0; i < n; i++ {
		if !b.windowResults[i] {
			failures++
		}
	}
	rate := float64(failures) / float64(n)
	return rate > b.errorThreshold
}

func (b *Breaker) setState(s observer.State) {
	if b.state == s {
		return
	}
	prev := b.state
	b.state = s
	b.observer.OnStateChange(prev, s)

	b.counts = Counts{}
	if b.adaptive {
		b.windowIdx = 0
		b.windowFilled = false
		for i := range b.windowResults {
			b.windowResults[i] = false
		}
	}

	if s == observer.StateOpen {
		b.expiry = b.clock.Now().Add(b.openTimeout)
	}
	if s == observer.StateHalfOpen {
		for {
			select {
			case <-b.halfOpenSem:
			default:
				return
			}
		}
	}
}

// checkExpiry transitions from Open → HalfOpen when the timeout passes.
// Must be called with lock held.
func (b *Breaker) checkExpiry() {
	if b.state == observer.StateOpen && b.clock.Now().After(b.expiry) {
		b.setState(observer.StateHalfOpen)
	}
}
