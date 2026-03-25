package flowguard

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"
)

// Retry executes a function with configurable retry logic.
// Supports constant, exponential, and jittered backoff.
type Retry struct {
	maxRetries int
	baseDelay  time.Duration
	maxBackoff time.Duration
	jitter     float64 // 0.0 to 1.0
	constant   bool    // if true, use constant backoff instead of exponential
	retryIf    func(error) bool
	clock      Clock
	observer   Observer
}

// RetryOption configures a Retry.
type RetryOption func(*Retry)

// NewRetry creates a retry with exponential backoff.
// Defaults: 3 retries, 100ms base, 30s max, 0.2 jitter.
func NewRetry(opts ...RetryOption) *Retry {
	r := &Retry{
		maxRetries: 3,
		baseDelay:  100 * time.Millisecond,
		maxBackoff: 30 * time.Second,
		jitter:     0.2,
		clock:      defaultClock{},
		observer:   noopObserver{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func WithMaxRetries(n int) RetryOption {
	return func(r *Retry) { r.maxRetries = n }
}

func WithConstantBackoff(d time.Duration) RetryOption {
	return func(r *Retry) {
		r.baseDelay = d
		r.constant = true
	}
}

func WithExponentialBackoff(base time.Duration) RetryOption {
	return func(r *Retry) {
		r.baseDelay = base
		r.constant = false
	}
}

// WithJitter sets the jitter factor (0.0 to 1.0).
// Uses Full Jitter from the AWS architecture blog:
// sleep = random_between(0, min(cap, base * 2^attempt))
func WithJitter(j float64) RetryOption {
	return func(r *Retry) { r.jitter = j }
}

func WithMaxBackoff(d time.Duration) RetryOption {
	return func(r *Retry) { r.maxBackoff = d }
}

// WithRetryIf sets a predicate for which errors should be retried.
// By default, everything except context.Canceled and context.DeadlineExceeded.
func WithRetryIf(fn func(error) bool) RetryOption {
	return func(r *Retry) { r.retryIf = fn }
}

func WithRetryClock(c Clock) RetryOption {
	return func(r *Retry) { r.clock = c }
}

func WithRetryObserver(o Observer) RetryOption {
	return func(r *Retry) { r.observer = o }
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

		// permanent errors should not be retried (used by Policy)
		if _, ok := err.(*permanentError); ok {
			return err
		}

		// don't retry context errors
		if err == context.Canceled || err == context.DeadlineExceeded {
			return err
		}

		// check custom retry predicate
		if r.retryIf != nil && !r.retryIf(err) {
			return err
		}

		// was that the last attempt?
		if attempt == r.maxRetries {
			break
		}

		r.observer.OnRetry(attempt+1, err)

		delay := r.calcDelay(attempt)
		// this is a bit gnarly but we need to respect both the clock and ctx
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

	return fmt.Errorf("flowguard: after %d retries: %w", r.maxRetries, lastErr)
}

func (r *Retry) calcDelay(attempt int) time.Duration {
	if r.constant {
		return r.applyJitter(r.baseDelay)
	}

	// exponential: base * 2^attempt
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
	// Full Jitter: sleep = random(0, d)
	// But we scale by jitter factor so 0.2 means ±20%
	jit := float64(d) * r.jitter
	delta := (rand.Float64()*2 - 1) * jit // -jit to +jit
	result := time.Duration(float64(d) + delta)
	if result < 0 {
		result = 0
	}
	return result
}
