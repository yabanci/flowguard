// Package ratelimit provides three rate limiter strategies that share one
// interface: token bucket (constant rate + burst), sliding window (fixed
// memory, API-quota style), and AIMD (TCP-style additive-increase /
// multiplicative-decrease for auto-tuning against opaque upstream limits).
package ratelimit

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yabanci/flowguard/clock"
	"github.com/yabanci/flowguard/observer"
)

// ErrLimited is returned on the Wait path when the caller context is
// already cancelled. Allow() signals the same condition by returning false.
var ErrLimited = errors.New("flowguard/ratelimit: rate limited")

// Limiter controls how frequently operations are allowed to happen.
// Supports token bucket, sliding window, and AIMD strategies through
// the three constructors in this package.
type Limiter struct {
	mu sync.Mutex

	// token bucket fields
	rate     float64
	burst    int
	tokens   float64
	lastTime time.Time

	// sliding window fields
	windowSize  time.Duration
	windowLimit int
	buckets     []int
	bucketCount int
	bucketIdx   int
	bucketStart time.Time
	bucketWidth time.Duration
	isWindow    bool

	// AIMD fields
	aimdCurrent int
	aimdMin     int
	aimdMax     int
	aimdCount   int
	isAIMD      bool

	clock    clock.Clock
	observer observer.Observer
}

// Option configures a Limiter.
type Option func(*Limiter)

// WithClock sets a custom clock — mainly useful for tests.
func WithClock(c clock.Clock) Option {
	return func(l *Limiter) { l.clock = c }
}

// WithObserver attaches an observer for metrics/logging.
func WithObserver(o observer.Observer) Option {
	return func(l *Limiter) { l.observer = o }
}

// NewTokenBucket sets up a token bucket limiter.
// rate is tokens/sec, burst is the max tokens that can accumulate.
func NewTokenBucket(rate float64, burst int, opts ...Option) *Limiter {
	l := &Limiter{
		rate:     rate,
		burst:    burst,
		tokens:   float64(burst),
		clock:    clock.Real(),
		observer: observer.Noop{},
	}
	for _, opt := range opts {
		opt(l)
	}
	l.lastTime = l.clock.Now()
	return l
}

// NewSlidingWindow creates a rate limiter using a sliding window counter.
// The window is split into 10 sub-buckets and summed — more accurate than
// a fixed window, cheaper than a full sliding log.
func NewSlidingWindow(limit int, window time.Duration, opts ...Option) *Limiter {
	n := 10
	l := &Limiter{
		windowSize:  window,
		windowLimit: limit,
		buckets:     make([]int, n),
		bucketCount: n,
		bucketWidth: window / time.Duration(n),
		isWindow:    true,
		clock:       clock.Real(),
		observer:    observer.Noop{},
	}
	for _, opt := range opts {
		opt(l)
	}
	l.bucketStart = l.clock.Now()
	return l
}

// NewAIMD creates an adaptive rate limiter using Additive Increase,
// Multiplicative Decrease — same idea as TCP congestion control.
// On success the limit goes up by 1, on failure it gets halved.
// initial/min/max define the range the limit can swing in.
func NewAIMD(initial, min, max int, opts ...Option) *Limiter {
	l := &Limiter{
		aimdCurrent: initial,
		aimdMin:     min,
		aimdMax:     max,
		isAIMD:      true,
		clock:       clock.Real(),
		observer:    observer.Noop{},
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Wait blocks until a token is available or ctx is cancelled.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		l.mu.Lock()
		if l.isWindow {
			ok := l.windowAllow()
			l.mu.Unlock()
			if ok {
				return nil
			}
			dur := l.bucketWidth / 2
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-l.sleepChan(dur):
			}
			continue
		}

		if l.isAIMD {
			ok := l.aimdAllow()
			l.mu.Unlock()
			if ok {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-l.sleepChan(100 * time.Millisecond):
			}
			continue
		}

		l.refill()
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration(float64(time.Second) / l.rate)
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.sleepChan(wait):
		}
	}
}

func (l *Limiter) sleepChan(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	go func() {
		l.clock.Sleep(d)
		ch <- l.clock.Now()
	}()
	return ch
}

// Allow tries to take a token without blocking. Returns true if allowed.
func (l *Limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isWindow {
		return l.windowAllow()
	}
	if l.isAIMD {
		return l.aimdAllow()
	}

	l.refill()
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	l.observer.OnRateLimited()
	return false
}

// Reserve returns how long you'd need to wait for the next token.
// Doesn't actually consume anything. Returns 0 for sliding-window and AIMD.
func (l *Limiter) Reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isWindow || l.isAIMD {
		return 0
	}

	l.refill()
	if l.tokens >= 1 {
		return 0
	}
	return time.Duration(float64(time.Second) * (1 - l.tokens) / l.rate)
}

// refill adds tokens based on elapsed time. Must be called with lock held.
func (l *Limiter) refill() {
	now := l.clock.Now()
	elapsed := now.Sub(l.lastTime).Seconds()
	l.lastTime = now

	l.tokens += elapsed * l.rate
	if l.tokens > float64(l.burst) {
		l.tokens = float64(l.burst)
	}
}

func (l *Limiter) windowAllow() bool {
	l.advanceBuckets()

	total := 0
	for _, c := range l.buckets {
		total += c
	}
	if total >= l.windowLimit {
		l.observer.OnRateLimited()
		return false
	}
	l.buckets[l.bucketIdx]++
	return true
}

func (l *Limiter) advanceBuckets() {
	now := l.clock.Now()
	elapsed := now.Sub(l.bucketStart)
	advance := int(elapsed / l.bucketWidth)

	if advance <= 0 {
		return
	}
	if advance >= l.bucketCount {
		for i := range l.buckets {
			l.buckets[i] = 0
		}
		l.bucketIdx = 0
		l.bucketStart = now
		return
	}

	for i := 0; i < advance; i++ {
		l.bucketIdx = (l.bucketIdx + 1) % l.bucketCount
		l.buckets[l.bucketIdx] = 0
	}
	l.bucketStart = l.bucketStart.Add(time.Duration(advance) * l.bucketWidth)
}

func (l *Limiter) aimdAllow() bool {
	if l.aimdCount >= l.aimdCurrent {
		l.observer.OnRateLimited()
		return false
	}
	l.aimdCount++
	return true
}

// OnSuccess signals a successful operation — increases the AIMD limit by 1.
// No-op if this isn't an AIMD limiter.
func (l *Limiter) OnSuccess() {
	if !l.isAIMD {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.aimdCount = 0
	if l.aimdCurrent < l.aimdMax {
		l.aimdCurrent++
	}
}

// OnFailure signals a failed operation — halves the AIMD limit.
// No-op if this isn't an AIMD limiter.
func (l *Limiter) OnFailure() {
	if !l.isAIMD {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.aimdCount = 0
	l.aimdCurrent = l.aimdCurrent / 2
	if l.aimdCurrent < l.aimdMin {
		l.aimdCurrent = l.aimdMin
	}
}

// CurrentLimit returns the current AIMD limit. Useful for monitoring.
func (l *Limiter) CurrentLimit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.aimdCurrent
}
