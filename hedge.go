package flowguard

import (
	"context"
	"time"
)

// Hedge implements hedged requests — if the primary call is slow, fire a
// second (or third) attempt after a delay and take whichever finishes first.
//
// This is the "Tail at Scale" pattern from Google's 2013 paper. Instead of
// waiting for the slow P99 tail, you send a redundant request after the
// expected latency and race them. Dramatically reduces tail latency for
// idempotent operations at the cost of ~5% extra load.
//
// IMPORTANT: fn must be safe to call concurrently and should be idempotent.
// All hedged calls share the same context, so cancelling one cancels all.
type Hedge struct {
	delay      time.Duration // how long to wait before launching hedge
	maxHedges  int           // max extra requests (1 = at most 2 total)
	observer   Observer
}

// HedgeOption configures a Hedge.
type HedgeOption func(*Hedge)

// NewHedge creates a hedger. delay is how long to wait before sending
// the first hedge request. Think of it as your P95 or P99 latency.
func NewHedge(delay time.Duration, opts ...HedgeOption) *Hedge {
	h := &Hedge{
		delay:     delay,
		maxHedges: 1, // one extra request by default
		observer:  noopObserver{},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// WithMaxHedges sets how many extra requests to send. Default is 1.
// Setting to 2 means up to 3 total requests (original + 2 hedges).
// Be careful with this — each hedge is extra load on the downstream.
func WithMaxHedges(n int) HedgeOption {
	return func(h *Hedge) { h.maxHedges = n }
}

// WithHedgeObserver attaches an observer.
func WithHedgeObserver(o Observer) HedgeOption {
	return func(h *Hedge) { h.observer = o }
}

// Do runs fn, and if it doesn't complete within the delay, fires additional
// hedged copies. Returns the result of whichever finishes first successfully.
// If all copies fail, returns the first error.
func (h *Hedge) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	// total attempts = 1 (primary) + maxHedges
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

	// launch primary
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

			// if a call failed and we still have hedge budget, launch one immediately
			// instead of waiting for the delay timer
			if hedgesLaunched < h.maxHedges {
				hedgesLaunched++
				h.observer.OnRetry(hedgesLaunched, r.err)
				go run(hedgesLaunched)
				continue
			}

			// all launched copies reported back and none succeeded
			if received >= 1+hedgesLaunched {
				h.observer.OnFailure(firstErr, time.Since(start))
				return firstErr
			}

		case <-time.After(h.delay):
			if hedgesLaunched < h.maxHedges {
				hedgesLaunched++
				h.observer.OnRetry(hedgesLaunched, nil) // reuse OnRetry for hedge events
				go run(hedgesLaunched)
			}
		}
	}

	return firstErr
}
