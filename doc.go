// Package flowguard provides resilience primitives for distributed
// systems: rate limiting, circuit breaking, retry, hedged requests,
// bulkhead, and load shedding.
//
// The root package exposes only Policy, which composes the primitives
// living in subpackages (ratelimit, circuitbreaker, retry, hedge,
// bulkhead, loadshed, middleware, observer, clock). Each subpackage
// also works standalone.
//
// Zero external dependencies — just stdlib.
//
// Quick start:
//
//	import (
//	    "github.com/yabanci/flowguard"
//	    "github.com/yabanci/flowguard/circuitbreaker"
//	    "github.com/yabanci/flowguard/ratelimit"
//	    "github.com/yabanci/flowguard/retry"
//	)
//
//	p := flowguard.NewPolicy(
//	    flowguard.WithRateLimiter(ratelimit.NewTokenBucket(10, 20)),
//	    flowguard.WithCircuitBreaker(circuitbreaker.New()),
//	    flowguard.WithRetry(retry.New()),
//	)
//	err := p.Do(ctx, func(ctx context.Context) error {
//	    return callExternalService(ctx)
//	})
package flowguard
