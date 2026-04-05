package flowguard

import (
	"context"
	"sync/atomic"
	"time"
)

// LoadShedder protects a server from overload by adaptively limiting
// in-flight requests. Uses the same AIMD algorithm as TCP congestion
// control — increase the limit on success, halve it on latency spikes.
//
// Unlike a static bulkhead, this adapts to your server's actual capacity.
// Think of it as server-side backpressure.
//
// Inspired by Netflix's concurrency-limits library.
type LoadShedder struct {
	inflight atomic.Int64
	limit    atomic.Int64

	minLimit         int64
	maxLimit         int64
	latencyThreshold time.Duration // if a call takes longer than this, treat as overload

	observer Observer
}

// LoadShedderOption configures a LoadShedder.
type LoadShedderOption func(*LoadShedder)

// NewLoadShedder creates an adaptive load shedder.
// initialLimit is the starting concurrency limit.
// latencyThreshold: calls slower than this trigger a limit decrease.
func NewLoadShedder(initialLimit int, latencyThreshold time.Duration, opts ...LoadShedderOption) *LoadShedder {
	ls := &LoadShedder{
		minLimit:         5,
		maxLimit:         1000,
		latencyThreshold: latencyThreshold,
		observer:         noopObserver{},
	}
	ls.limit.Store(int64(initialLimit))
	for _, opt := range opts {
		opt(ls)
	}
	return ls
}

// WithLoadShedLimits sets the min and max concurrency bounds.
func WithLoadShedLimits(min, max int) LoadShedderOption {
	return func(ls *LoadShedder) {
		ls.minLimit = int64(min)
		ls.maxLimit = int64(max)
	}
}

// WithLoadShedObserver attaches an observer.
func WithLoadShedObserver(o Observer) LoadShedderOption {
	return func(ls *LoadShedder) { ls.observer = o }
}

// Do runs fn if the server isn't overloaded. Rejects with ErrLoadShed
// if the current in-flight count exceeds the adaptive limit.
func (ls *LoadShedder) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	// check if we're over the limit
	current := ls.inflight.Add(1)
	limit := ls.limit.Load()

	if current > limit {
		ls.inflight.Add(-1)
		ls.observer.OnRateLimited()
		return ErrLoadShed
	}

	start := time.Now()
	err := fn(ctx)
	lat := time.Since(start)

	ls.inflight.Add(-1)

	// adapt the limit based on the result
	if err != nil || lat > ls.latencyThreshold {
		// overloaded — multiplicative decrease
		ls.decrease()
		ls.observer.OnFailure(err, lat)
	} else {
		// happy path — additive increase
		ls.increase()
		ls.observer.OnSuccess(lat)
	}

	return err
}

// CurrentLimit returns the current adaptive concurrency limit.
func (ls *LoadShedder) CurrentLimit() int {
	return int(ls.limit.Load())
}

// Inflight returns the current number of in-flight requests.
func (ls *LoadShedder) Inflight() int {
	return int(ls.inflight.Load())
}

func (ls *LoadShedder) increase() {
	for {
		old := ls.limit.Load()
		next := old + 1
		if next > ls.maxLimit {
			next = ls.maxLimit
		}
		if ls.limit.CompareAndSwap(old, next) {
			return
		}
	}
}

func (ls *LoadShedder) decrease() {
	for {
		old := ls.limit.Load()
		next := old / 2
		if next < ls.minLimit {
			next = ls.minLimit
		}
		if ls.limit.CompareAndSwap(old, next) {
			return
		}
	}
}
