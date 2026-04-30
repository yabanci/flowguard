// Package loadshed implements adaptive server-side backpressure.
// Uses the AIMD algorithm from TCP congestion control to grow the
// concurrency limit on success and halve it on latency spikes.
// Inspired by Netflix's concurrency-limits library.
package loadshed

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/yabanci/flowguard/observer"
)

// ErrShed is returned when the in-flight count exceeds the adaptive limit.
var ErrShed = errors.New("flowguard/loadshed: shed")

// Shedder protects a server from overload by adaptively limiting in-flight requests.
type Shedder struct {
	inflight atomic.Int64
	limit    atomic.Int64

	minLimit         int64
	maxLimit         int64
	latencyThreshold time.Duration

	observer observer.Observer
}

// Option configures a Shedder.
type Option func(*Shedder)

// New creates an adaptive load shedder.
// initialLimit is the starting concurrency limit; calls slower than
// latencyThreshold trigger a limit decrease.
func New(initialLimit int, latencyThreshold time.Duration, opts ...Option) *Shedder {
	s := &Shedder{
		minLimit:         5,
		maxLimit:         1000,
		latencyThreshold: latencyThreshold,
		observer:         observer.Noop{},
	}
	s.limit.Store(int64(initialLimit))
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithLimits sets the min and max concurrency bounds.
// min is clamped to 1: a zero minimum would allow the limit to reach 0,
// making Do() permanently reject all requests with no recovery path.
func WithLimits(min, max int) Option {
	return func(s *Shedder) {
		if min < 1 {
			min = 1
		}
		s.minLimit = int64(min)
		s.maxLimit = int64(max)
	}
}

// WithObserver attaches an observer.
func WithObserver(o observer.Observer) Option {
	return func(s *Shedder) { s.observer = o }
}

// Do runs fn if the server isn't overloaded. Rejects with ErrShed
// if the current in-flight count exceeds the adaptive limit.
func (s *Shedder) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	current := s.inflight.Add(1)
	limit := s.limit.Load()

	if current > limit {
		s.inflight.Add(-1)
		s.observer.OnRateLimited()
		return ErrShed
	}

	start := time.Now()
	err := fn(ctx)
	lat := time.Since(start)

	s.inflight.Add(-1)

	if err != nil || lat > s.latencyThreshold {
		s.decrease()
		s.observer.OnFailure(err, lat)
	} else {
		s.increase()
		s.observer.OnSuccess(lat)
	}

	return err
}

// CurrentLimit returns the current adaptive concurrency limit.
func (s *Shedder) CurrentLimit() int {
	return int(s.limit.Load())
}

// Inflight returns the current number of in-flight requests.
func (s *Shedder) Inflight() int {
	return int(s.inflight.Load())
}

func (s *Shedder) increase() {
	for {
		old := s.limit.Load()
		next := old + 1
		if next > s.maxLimit {
			next = s.maxLimit
		}
		if s.limit.CompareAndSwap(old, next) {
			return
		}
	}
}

func (s *Shedder) decrease() {
	for {
		old := s.limit.Load()
		next := old / 2
		if next < s.minLimit {
			next = s.minLimit
		}
		if s.limit.CompareAndSwap(old, next) {
			return
		}
	}
}
