package flowguard

import (
	"context"
	"sync"
	"time"
)

// RateLimiter controls how frequently operations are allowed to happen.
// Supports token bucket, sliding window, and AIMD strategies.
type RateLimiter struct {
	mu sync.Mutex

	// token bucket fields
	rate     float64 // tokens per second
	burst    int     // max tokens
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
	aimdCount   int // requests in current "epoch"
	isAIMD      bool

	clock    Clock
	observer Observer
}

// NewRateLimiter sets up a token bucket limiter.
// rate is tokens/sec, burst is the max tokens that can accumulate.
func NewRateLimiter(rate float64, burst int, opts ...RateLimiterOption) *RateLimiter {
	rl := &RateLimiter{
		rate:     rate,
		burst:    burst,
		tokens:   float64(burst), // start full
		clock:    defaultClock{},
		observer: noopObserver{},
	}
	for _, opt := range opts {
		opt(rl)
	}
	rl.lastTime = rl.clock.Now()
	return rl
}

// NewSlidingWindowLimiter creates a rate limiter using a sliding window counter.
// We split the window into 10 sub-buckets and sum them up — more accurate
// than a fixed window, cheaper than a full sliding log.
func NewSlidingWindowLimiter(limit int, window time.Duration, opts ...RateLimiterOption) *RateLimiter {
	n := 10 // number of sub-buckets, hardcoded for now
	// TODO: maybe expose this as an option later
	rl := &RateLimiter{
		windowSize:  window,
		windowLimit: limit,
		buckets:     make([]int, n),
		bucketCount: n,
		bucketWidth: window / time.Duration(n),
		isWindow:    true,
		clock:       defaultClock{},
		observer:    noopObserver{},
	}
	for _, opt := range opts {
		opt(rl)
	}
	rl.bucketStart = rl.clock.Now()
	return rl
}

// NewAIMDLimiter creates an adaptive rate limiter using Additive Increase,
// Multiplicative Decrease — same idea as TCP congestion control.
// On success the limit goes up by 1, on failure it gets halved.
// initial/min/max define the range the limit can swing in.
func NewAIMDLimiter(initial, min, max int, opts ...RateLimiterOption) *RateLimiter {
	rl := &RateLimiter{
		aimdCurrent: initial,
		aimdMin:     min,
		aimdMax:     max,
		isAIMD:      true,
		clock:       defaultClock{},
		observer:    noopObserver{},
	}
	for _, opt := range opts {
		opt(rl)
	}
	return rl
}

// Wait blocks until a token is available or ctx is cancelled.
func (rl *RateLimiter) Wait(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		rl.mu.Lock()
		if rl.isWindow {
			ok := rl.windowAllow()
			rl.mu.Unlock()
			if ok {
				return nil
			}
			// wait a bit and try again — not ideal but simple
			dur := rl.bucketWidth / 2
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-rl.sleepChan(dur):
			}
			continue
		}

		if rl.isAIMD {
			ok := rl.aimdAllow()
			rl.mu.Unlock()
			if ok {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-rl.sleepChan(100 * time.Millisecond):
			}
			continue
		}

		// token bucket
		rl.refill()
		if rl.tokens >= 1 {
			rl.tokens--
			rl.mu.Unlock()
			return nil
		}
		// figure out how long until next token
		wait := time.Duration(float64(time.Second) / rl.rate)
		rl.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rl.sleepChan(wait):
		}
	}
}

func (rl *RateLimiter) sleepChan(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	go func() {
		rl.clock.Sleep(d)
		ch <- rl.clock.Now()
	}()
	return ch
}

// Allow tries to take a token without blocking. Returns true if allowed.
func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.isWindow {
		return rl.windowAllow()
	}
	if rl.isAIMD {
		return rl.aimdAllow()
	}

	rl.refill()
	if rl.tokens >= 1 {
		rl.tokens--
		return true
	}
	if rl.observer != nil {
		rl.observer.OnRateLimited()
	}
	return false
}

// Reserve returns how long you'd need to wait for the next token.
// Doesn't actually consume anything.
func (rl *RateLimiter) Reserve() time.Duration {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.isWindow || rl.isAIMD {
		// not really meaningful for these strategies
		return 0
	}

	rl.refill()
	if rl.tokens >= 1 {
		return 0
	}
	return time.Duration(float64(time.Second) * (1 - rl.tokens) / rl.rate)
}

// refill adds tokens based on elapsed time. Must be called with lock held.
func (rl *RateLimiter) refill() {
	now := rl.clock.Now()
	elapsed := now.Sub(rl.lastTime).Seconds()
	rl.lastTime = now

	rl.tokens += elapsed * rl.rate
	if rl.tokens > float64(rl.burst) {
		rl.tokens = float64(rl.burst)
	}
}

// --- sliding window internals ---

func (rl *RateLimiter) windowAllow() bool {
	rl.advanceBuckets()

	total := 0
	for _, c := range rl.buckets {
		total += c
	}
	if total >= rl.windowLimit {
		if rl.observer != nil {
			rl.observer.OnRateLimited()
		}
		return false
	}
	rl.buckets[rl.bucketIdx]++
	return true
}

func (rl *RateLimiter) advanceBuckets() {
	now := rl.clock.Now()
	elapsed := now.Sub(rl.bucketStart)
	advance := int(elapsed / rl.bucketWidth)

	if advance <= 0 {
		return
	}
	if advance >= rl.bucketCount {
		// whole window passed, reset everything
		for i := range rl.buckets {
			rl.buckets[i] = 0
		}
		rl.bucketIdx = 0
		rl.bucketStart = now
		return
	}

	for i := 0; i < advance; i++ {
		rl.bucketIdx = (rl.bucketIdx + 1) % rl.bucketCount
		rl.buckets[rl.bucketIdx] = 0
	}
	rl.bucketStart = rl.bucketStart.Add(time.Duration(advance) * rl.bucketWidth)
}

// --- AIMD internals ---

func (rl *RateLimiter) aimdAllow() bool {
	if rl.aimdCount >= rl.aimdCurrent {
		if rl.observer != nil {
			rl.observer.OnRateLimited()
		}
		return false
	}
	rl.aimdCount++
	return true
}

// OnSuccess signals a successful operation — increases the AIMD limit by 1.
// No-op if this isn't an AIMD limiter.
func (rl *RateLimiter) OnSuccess() {
	if !rl.isAIMD {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.aimdCount = 0 // reset epoch
	if rl.aimdCurrent < rl.aimdMax {
		rl.aimdCurrent++
	}
}

// OnFailure signals a failed operation — halves the AIMD limit.
// No-op if this isn't an AIMD limiter.
func (rl *RateLimiter) OnFailure() {
	if !rl.isAIMD {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.aimdCount = 0
	rl.aimdCurrent = rl.aimdCurrent / 2
	if rl.aimdCurrent < rl.aimdMin {
		rl.aimdCurrent = rl.aimdMin
	}
}

// CurrentLimit returns the current AIMD limit. Useful for monitoring.
func (rl *RateLimiter) CurrentLimit() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.aimdCurrent
}
