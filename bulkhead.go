package flowguard

import (
	"context"
	"time"
)

// Bulkhead limits concurrent access to a resource. Think of it as a
// bouncer at the door — only N goroutines get in at once, the rest
// either wait or get rejected.
//
// This prevents a slow downstream from eating all your goroutines.
// Named after ship bulkheads that contain flooding to one compartment.
type Bulkhead struct {
	sem      chan struct{}
	maxWait  time.Duration
	observer Observer
}

// BulkheadOption configures a Bulkhead.
type BulkheadOption func(*Bulkhead)

// NewBulkhead creates a bulkhead with the given max concurrency.
// By default, callers wait indefinitely for a slot. Use WithMaxWaitDuration
// to fail fast instead.
func NewBulkhead(maxConcurrent int, opts ...BulkheadOption) *Bulkhead {
	b := &Bulkhead{
		sem:      make(chan struct{}, maxConcurrent),
		observer: noopObserver{},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// WithMaxWaitDuration sets how long callers wait for a slot before giving up.
// Zero means reject immediately if full (like a tryAcquire).
func WithMaxWaitDuration(d time.Duration) BulkheadOption {
	return func(b *Bulkhead) { b.maxWait = d }
}

// WithBulkheadObserver attaches an observer.
func WithBulkheadObserver(o Observer) BulkheadOption {
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
	// fast path: try without blocking
	select {
	case b.sem <- struct{}{}:
		return nil
	default:
	}

	// slow path: wait with timeout
	if b.maxWait == 0 {
		// zero means don't wait at all
		b.observer.OnRateLimited()
		return ErrBulkheadFull
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
		return ErrBulkheadFull
	}
}

func (b *Bulkhead) release() {
	<-b.sem
}
