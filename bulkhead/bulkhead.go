// Package bulkhead limits concurrent access to a resource so a slow
// downstream cannot exhaust the caller's goroutines. Named after ship
// bulkheads that contain flooding to one compartment.
package bulkhead

import (
	"context"
	"errors"
	"time"

	"github.com/yabanci/flowguard/observer"
)

// ErrFull is returned when every concurrency slot is occupied and the
// configured wait timeout has elapsed.
var ErrFull = errors.New("flowguard/bulkhead: full")

// Bulkhead is the concurrency limiter. Only maxConcurrent goroutines
// run fn at once; the rest either wait or get rejected with ErrFull.
type Bulkhead struct {
	sem      chan struct{}
	maxWait  time.Duration
	observer observer.Observer
}

// Option configures a Bulkhead.
type Option func(*Bulkhead)

// New creates a bulkhead with the given max concurrency.
// By default, callers wait indefinitely for a slot. Use WithMaxWait to fail fast.
func New(maxConcurrent int, opts ...Option) *Bulkhead {
	b := &Bulkhead{
		sem:      make(chan struct{}, maxConcurrent),
		observer: observer.Noop{},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// WithMaxWait sets how long callers wait for a slot before giving up.
// Zero means reject immediately if full (like a tryAcquire).
func WithMaxWait(d time.Duration) Option {
	return func(b *Bulkhead) { b.maxWait = d }
}

// WithObserver attaches an observer.
func WithObserver(o observer.Observer) Option {
	return func(b *Bulkhead) { b.observer = o }
}

// Do runs fn if a concurrency slot is available. Blocks until a slot opens,
// the wait timeout expires, or ctx is cancelled.
func (b *Bulkhead) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := b.acquire(ctx); err != nil {
		return err
	}
	defer b.release()

	start := time.Now()
	err := fn(ctx)
	lat := time.Since(start)

	if err != nil {
		b.observer.OnFailure(err, lat)
	} else {
		b.observer.OnSuccess(lat)
	}
	return err
}

// ActiveCount returns how many slots are currently occupied.
func (b *Bulkhead) ActiveCount() int {
	return len(b.sem)
}

func (b *Bulkhead) acquire(ctx context.Context) error {
	select {
	case b.sem <- struct{}{}:
		return nil
	default:
	}

	if b.maxWait == 0 {
		b.observer.OnRateLimited()
		return ErrFull
	}

	timer := time.NewTimer(b.maxWait)
	defer timer.Stop()

	select {
	case b.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		b.observer.OnRateLimited()
		return ErrFull
	}
}

func (b *Bulkhead) release() {
	<-b.sem
}
