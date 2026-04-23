// Package hedge implements hedged requests — Google's "Tail at Scale"
// pattern for reducing P99 latency on idempotent operations at the cost
// of ~5% extra load.
package hedge

import (
	"context"
	"time"

	"github.com/yabanci/flowguard/observer"
)

// Hedge fires redundant copies of a slow call and returns the first success.
//
// IMPORTANT: fn must be safe to call concurrently and should be idempotent.
// All hedged calls share the same context, so cancelling one cancels all.
type Hedge struct {
	delay     time.Duration
	maxHedges int
	observer  observer.Observer
}

// Option configures a Hedge.
type Option func(*Hedge)

// New creates a hedger. delay is how long to wait before sending the first
// hedge — think of it as your P95 or P99 latency.
func New(delay time.Duration, opts ...Option) *Hedge {
	h := &Hedge{
		delay:     delay,
		maxHedges: 1,
		observer:  observer.Noop{},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// WithMaxHedges sets how many extra requests to send. Default is 1.
// Setting to 2 means up to 3 total requests (original + 2 hedges).
func WithMaxHedges(n int) Option {
	return func(h *Hedge) { h.maxHedges = n }
}

// WithObserver attaches an observer.
func WithObserver(o observer.Observer) Option {
	return func(h *Hedge) { h.observer = o }
}

// Do runs fn, and if it doesn't complete within the delay, fires additional
// hedged copies. Returns the result of whichever finishes first successfully.
// If all copies fail, returns the first error.
func (h *Hedge) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	total := 1 + h.maxHedges

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		err error
		idx int
	}
	results := make(chan result, total)

	start := time.Now()
	run := func(i int) {
		err := fn(ctx)
		results <- result{err: err, idx: i}
	}

	go run(0)

	var firstErr error
	received := 0
	hedgesLaunched := 0

	for received < total {
		select {
		case r := <-results:
			received++
			if r.err == nil {
				h.observer.OnSuccess(time.Since(start))
				cancel()
				return nil
			}
			if firstErr == nil {
				firstErr = r.err
			}

			if hedgesLaunched < h.maxHedges {
				hedgesLaunched++
				h.observer.OnRetry(hedgesLaunched, r.err)
				go run(hedgesLaunched)
				continue
			}

			if received >= 1+hedgesLaunched {
				h.observer.OnFailure(firstErr, time.Since(start))
				return firstErr
			}

		case <-time.After(h.delay):
			if hedgesLaunched < h.maxHedges {
				hedgesLaunched++
				h.observer.OnRetry(hedgesLaunched, nil)
				go run(hedgesLaunched)
			}
		}
	}

	return firstErr
}
