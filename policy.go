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

func WithPolicyRateLimiter(rl *RateLimiter) PolicyOption {
	return func(p *Policy) { p.rl = rl }
}

func WithPolicyCircuitBreaker(cb *CircuitBreaker) PolicyOption {
	return func(p *Policy) { p.cb = cb }
}

func WithPolicyRetry(r *Retry) PolicyOption {
	return func(p *Policy) { p.retry = r }
}

func WithPolicyObserver(o Observer) PolicyOption {
	return func(p *Policy) { p.observer = o }
}

// Do runs fn through all configured components.
func (p *Policy) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	wrapped := fn

	// wrap with rate limiter (innermost)
	if p.rl != nil {
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			if err := p.rl.Wait(ctx); err != nil {
				p.observer.OnRateLimited()
				return &rateLimitError{err}
			}
			return inner(ctx)
		}
	}

	// wrap with circuit breaker
	if p.cb != nil {
		inner := wrapped
		wrapped = func(ctx context.Context) error {
			return p.cb.Do(ctx, func(ctx context.Context) error {
				err := inner(ctx)
				// unwrap rate limit errors so CB doesn't count them as failures
				if isRateLimitErr(err) {
					// still return the error to caller, but CB saw nil
					// this is a bit of a hack but it works
					return nil
				}
				return err
			})
		}

		// hmm actually the above eats the rate limit error. let me fix that.
		// we need to thread the rl error back out without CB seeing it
		cbInner := wrapped
		wrapped = func(ctx context.Context) error {
			var rlErr error
			err := cbInner(ctx)
			if err != nil {
				return err
			}
			// check if rate limiter failed — we stashed it
			if rlErr != nil {
				return rlErr
			}
			return nil
		}

		// OK this got too convoluted. Let me redo this properly.
		wrapped = p.buildCBWrapped(fn)
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

// --- helpers to prevent RL errors from tripping CB ---

type rateLimitError struct{ inner error }

func (e *rateLimitError) Error() string { return e.inner.Error() }
func (e *rateLimitError) Unwrap() error { return e.inner }

func isRateLimitErr(err error) bool {
	_, ok := err.(*rateLimitError)
	return ok
}

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
