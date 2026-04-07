package flowguard

import "time"

// RateLimiterOption configures a RateLimiter.
type RateLimiterOption func(*RateLimiter)

// WithRateLimiterClock sets a custom clock — mainly useful for tests.
func WithRateLimiterClock(c Clock) RateLimiterOption {
	return func(rl *RateLimiter) {
		rl.clock = c
	}
}

// WithRateLimiterObserver attaches an observer for metrics/logging.
func WithRateLimiterObserver(o Observer) RateLimiterOption {
	return func(rl *RateLimiter) {
		rl.observer = o
	}
}

// Clock abstracts time so we can test without real sleeps.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

type defaultClock struct{}

func (defaultClock) Now() time.Time         { return time.Now() }
func (defaultClock) Sleep(d time.Duration)  { time.Sleep(d) }
