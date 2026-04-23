// Package retry runs a function with configurable backoff. Exports
// Permanent(err) / IsPermanent so callers — notably flowguard.Policy —
// can mark an error non-retryable without a type assertion.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/yabanci/flowguard/clock"
	"github.com/yabanci/flowguard/observer"
)

// Retry executes a function with configurable retry logic.
// Supports constant, exponential, and jittered backoff.
type Retry struct {
	maxRetries int
	baseDelay  time.Duration
	maxBackoff time.Duration
	jitter     float64
	constant   bool
	retryIf    func(error) bool
	clock      clock.Clock
	observer   observer.Observer
}

// Option configures a Retry.
type Option func(*Retry)

// New creates a retry with exponential backoff.
// Defaults: 3 retries, 100ms base, 30s max, 0.2 jitter.
func New(opts ...Option) *Retry {
	r := &Retry{
		maxRetries: 3,
		baseDelay:  100 * time.Millisecond,
		maxBackoff: 30 * time.Second,
		jitter:     0.2,
		clock:      clock.Real(),
		observer:   observer.Noop{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func WithMaxRetries(n int) Option {
	return func(r *Retry) { r.maxRetries = n }
}

func WithConstantBackoff(d time.Duration) Option {
	return func(r *Retry) {
		r.baseDelay = d
		r.constant = true
	}
}

func WithExponentialBackoff(base time.Duration) Option {
	return func(r *Retry) {
		r.baseDelay = base
		r.constant = false
	}
}

// WithJitter sets the jitter factor (0.0 to 1.0). Uses Full Jitter:
// sleep = random_between(0, min(cap, base * 2^attempt)).
func WithJitter(j float64) Option {
	return func(r *Retry) { r.jitter = j }
}

func WithMaxBackoff(d time.Duration) Option {
	return func(r *Retry) { r.maxBackoff = d }
}

// WithRetryIf sets a predicate for which errors should be retried.
// By default, everything except context.Canceled and context.DeadlineExceeded.
func WithRetryIf(fn func(error) bool) Option {
	return func(r *Retry) { r.retryIf = fn }
}

func WithClock(c clock.Clock) Option {
	return func(r *Retry) { r.clock = c }
}

func WithObserver(o observer.Observer) Option {
	return func(r *Retry) { r.observer = o }
}

// permanent wraps errors that Retry must not retry.
type permanent struct{ err error }

func (p *permanent) Error() string { return p.err.Error() }
func (p *permanent) Unwrap() error { return p.err }

// Permanent wraps err so that Retry stops on it instead of retrying.
// The returned error unwraps to err, so errors.Is / errors.As still work.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanent{err: err}
}

// IsPermanent reports whether err was wrapped with Permanent.
func IsPermanent(err error) bool {
	var p *permanent
	return errors.As(err, &p)
}

// Unwrap returns the inner error if err was wrapped with Permanent, otherwise err.
func Unwrap(err error) error {
	var p *permanent
	if errors.As(err, &p) {
		return p.err
	}
	return err
}

// Do executes fn, retrying on failure according to the configured policy.
// Returns the last error wrapped with attempt count.
func (r *Retry) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	var lastErr error

	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := fn(ctx)
		if err == nil {
			return nil
		}

		lastErr = err

		if IsPermanent(err) {
			return err
		}

		if err == context.Canceled || err == context.DeadlineExceeded {
			return err
		}

		if r.retryIf != nil && !r.retryIf(err) {
			return err
		}

		if attempt == r.maxRetries {
			break
		}

		r.observer.OnRetry(attempt+1, err)

		delay := r.calcDelay(attempt)
		sleepDone := make(chan struct{})
		go func() {
			r.clock.Sleep(delay)
			close(sleepDone)
		}()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sleepDone:
		}
	}

	return fmt.Errorf("flowguard/retry: after %d retries: %w", r.maxRetries, lastErr)
}

func (r *Retry) calcDelay(attempt int) time.Duration {
	if r.constant {
		return r.applyJitter(r.baseDelay)
	}

	delay := float64(r.baseDelay) * math.Pow(2, float64(attempt))
	if delay > float64(r.maxBackoff) {
		delay = float64(r.maxBackoff)
	}
	return r.applyJitter(time.Duration(delay))
}

func (r *Retry) applyJitter(d time.Duration) time.Duration {
	if r.jitter <= 0 {
		return d
	}
	jit := float64(d) * r.jitter
	delta := (rand.Float64()*2 - 1) * jit //nolint:gosec // jitter doesn't need crypto rand
	result := time.Duration(float64(d) + delta)
	if result < 0 {
		result = 0
	}
	return result
}
