package flowguard

import (
	"context"

	"github.com/yabanci/flowguard/bulkhead"
	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/hedge"
	"github.com/yabanci/flowguard/observer"
	"github.com/yabanci/flowguard/ratelimit"
	"github.com/yabanci/flowguard/retry"
)

// Policy composes the flowguard primitives into a single call wrapper.
// All components are optional — pass only the ones you want.
//
// Execution order (outside → inside):
//
//	Fallback → Retry → Hedge → CircuitBreaker → Bulkhead → RateLimiter → fn
//
// Rate-limit rejections intentionally do NOT count as circuit breaker
// failures — your own limiter shouldn't trip your own breaker.
type Policy struct {
	rl       *ratelimit.Limiter
	cb       *circuitbreaker.Breaker
	retry    *retry.Retry
	hedge    *hedge.Hedge
	bulkhead *bulkhead.Bulkhead
	fallback func(ctx context.Context, err error) error
	observer observer.Observer
}

// Option configures a Policy.
type Option func(*Policy)

// NewPolicy creates a policy. Add components with the With* options.
func NewPolicy(opts ...Option) *Policy {
	p := &Policy{observer: observer.Noop{}}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// WithRateLimiter attaches a rate limiter.
func WithRateLimiter(rl *ratelimit.Limiter) Option {
	return func(p *Policy) { p.rl = rl }
}

// WithCircuitBreaker attaches a circuit breaker.
func WithCircuitBreaker(cb *circuitbreaker.Breaker) Option {
	return func(p *Policy) { p.cb = cb }
}

// WithRetry attaches a retry.
func WithRetry(r *retry.Retry) Option {
	return func(p *Policy) { p.retry = r }
}

// WithHedge attaches a hedger.
func WithHedge(h *hedge.Hedge) Option {
	return func(p *Policy) { p.hedge = h }
}

// WithBulkhead attaches a bulkhead.
func WithBulkhead(b *bulkhead.Bulkhead) Option {
	return func(p *Policy) { p.bulkhead = b }
}

// WithFallback sets a fallback that runs when everything else fails.
// The fallback receives the original error so it can decide what to do
// (return a cached response, a default value, or the error itself).
func WithFallback(fn func(ctx context.Context, err error) error) Option {
	return func(p *Policy) { p.fallback = fn }
}

// WithObserver attaches an observer to the policy itself.
func WithObserver(o observer.Observer) Option {
	return func(p *Policy) { p.observer = o }
}

// Do runs fn through every configured component.
func (p *Policy) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	wrapped := fn

	if p.bulkhead != nil {
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			return p.bulkhead.Do(ctx, inner)
		}
	}

	if p.cb != nil {
		wrapped = p.buildCBWrapped(fn)
	} else if p.rl != nil {
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			if err := p.rl.Wait(ctx); err != nil {
				p.observer.OnRateLimited()
				return ratelimit.ErrLimited
			}
			return inner(ctx)
		}
	}

	if p.hedge != nil {
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			return p.hedge.Do(ctx, inner)
		}
	}

	if p.retry != nil {
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			return p.retry.Do(ctx, func(ctx context.Context) error {
				err := inner(ctx)
				if err == circuitbreaker.ErrOpen {
					return retry.Permanent(err)
				}
				return err
			})
		}
	}

	err := retry.Unwrap(wrapped(ctx))

	if err != nil && p.fallback != nil {
		return p.fallback(ctx, err)
	}

	return err
}

// buildCBWrapped wires the rate limiter inside the circuit breaker so
// that rate-limit rejections do NOT register as breaker failures.
func (p *Policy) buildCBWrapped(fn func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		var rlErr error

		cbErr := p.cb.Do(ctx, func(ctx context.Context) error {
			if p.rl != nil {
				if err := p.rl.Wait(ctx); err != nil {
					p.observer.OnRateLimited()
					rlErr = ratelimit.ErrLimited
					return nil
				}
			}
			return fn(ctx)
		})

		if cbErr != nil {
			return cbErr
		}
		return rlErr
	}
}
