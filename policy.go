package flowguard

import (
	"context"
)

// Policy composes rate limiter, circuit breaker, and retry into a single
// call wrapper. All components are optional — you can use just a limiter,
// or just retry, or any combination.
//
// Execution order: Retry → CircuitBreaker → RateLimiter → fn()
//
// Why this order:
//   - Retry wraps everything so it can retry on transient failures
//   - Circuit breaker is checked next — no point retrying if circuit is open
//   - Rate limiter goes last to control actual outgoing RPS
//   - Rate limit rejections do NOT count as circuit breaker failures
//     (that would be silly — your own limiter shouldn't trip your own breaker)
type Policy struct {
	rl       *RateLimiter
	cb       *CircuitBreaker
	retry    *Retry
	hedge    *Hedge
	bulkhead *Bulkhead
	fallback func(ctx context.Context, err error) error
	observer Observer
}

// PolicyOption configures a Policy.
type PolicyOption func(*Policy)

// NewPolicy creates a policy. Add components with WithPolicy* options.
func NewPolicy(opts ...PolicyOption) *Policy {
	p := &Policy{
		observer: noopObserver{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// WithPolicyRateLimiter adds a rate limiter to the policy.
func WithPolicyRateLimiter(rl *RateLimiter) PolicyOption {
	return func(p *Policy) { p.rl = rl }
}

// WithPolicyCircuitBreaker adds a circuit breaker to the policy.
func WithPolicyCircuitBreaker(cb *CircuitBreaker) PolicyOption {
	return func(p *Policy) { p.cb = cb }
}

// WithPolicyRetry adds retry logic to the policy.
func WithPolicyRetry(r *Retry) PolicyOption {
	return func(p *Policy) { p.retry = r }
}

// WithPolicyHedge adds hedged requests to the policy.
func WithPolicyHedge(h *Hedge) PolicyOption {
	return func(p *Policy) { p.hedge = h }
}

// WithPolicyBulkhead adds a bulkhead (concurrency limiter) to the policy.
func WithPolicyBulkhead(b *Bulkhead) PolicyOption {
	return func(p *Policy) { p.bulkhead = b }
}

// WithPolicyFallback sets a fallback function that runs when everything else fails.
// The fallback receives the original error so it can decide what to do.
func WithPolicyFallback(fn func(ctx context.Context, err error) error) PolicyOption {
	return func(p *Policy) { p.fallback = fn }
}

// WithPolicyObserver attaches an observer to the policy itself.
func WithPolicyObserver(o Observer) PolicyOption {
	return func(p *Policy) { p.observer = o }
}

// Do runs fn through all configured components.
//
// Execution order (outside → inside):
//
//	Fallback → Retry → Hedge → CircuitBreaker → Bulkhead → RateLimiter → fn()
func (p *Policy) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	wrapped := fn

	// bulkhead (innermost, right before the actual call)
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
				return ErrRateLimited
			}
			return inner(ctx)
		}
	}

	// hedged requests
	if p.hedge != nil {
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			return p.hedge.Do(ctx, inner)
		}
	}

	// retry
	if p.retry != nil {
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			return p.retry.Do(ctx, func(ctx context.Context) error {
				err := inner(ctx)
				if err == ErrCircuitOpen {
					return &permanentError{err}
				}
				return err
			})
		}
	}

	err := unwrapPermanent(wrapped(ctx))

	// fallback — last resort
	if err != nil && p.fallback != nil {
		return p.fallback(ctx, err)
	}

	return err
}

// buildCBWrapped builds the correct CB + RL wrapping.
// Separated out because the inline version was getting gnarly.
func (p *Policy) buildCBWrapped(fn func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		var rlErr error

		cbErr := p.cb.Do(ctx, func(ctx context.Context) error {
			// rate limit check
			if p.rl != nil {
				if err := p.rl.Wait(ctx); err != nil {
					p.observer.OnRateLimited()
					rlErr = ErrRateLimited
					return nil // don't tell CB about rate limit failures
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

// --- error wrappers ---

// permanentError wraps errors that retry should not retry
type permanentError struct{ inner error }

func (e *permanentError) Error() string { return e.inner.Error() }
func (e *permanentError) Unwrap() error { return e.inner }

func unwrapPermanent(err error) error {
	if p, ok := err.(*permanentError); ok {
		return p.inner
	}
	return err
}
