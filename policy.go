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

// WithPolicyObserver attaches an observer to the policy itself.
func WithPolicyObserver(o Observer) PolicyOption {
	return func(p *Policy) { p.observer = o }
}

// Do runs fn through all configured components.
func (p *Policy) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	wrapped := fn

	if p.cb != nil {
		// when CB is present, buildCBWrapped handles RL internally
		// so that rate limit rejections don't count as CB failures
		wrapped = p.buildCBWrapped(fn)
	} else if p.rl != nil {
		// no CB — just wrap with rate limiter directly
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			if err := p.rl.Wait(ctx); err != nil {
				p.observer.OnRateLimited()
				return ErrRateLimited
			}
			return inner(ctx)
		}
	}

	// wrap with retry (outermost)
	if p.retry != nil {
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			return p.retry.Do(ctx, func(ctx context.Context) error {
				err := inner(ctx)
				// don't retry circuit breaker open errors — pointless
				if err == ErrCircuitOpen {
					return &permanentError{err}
				}
				return err
			})
		}
	}

	return unwrapPermanent(wrapped(ctx))
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
