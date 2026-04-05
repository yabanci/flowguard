// Package flowguard provides resilience primitives for distributed systems:
// rate limiting, circuit breaking, retry, hedged requests, bulkhead,
// and load shedding.
//
// Each component works standalone or composed together via Policy.
// Zero external dependencies — just stdlib.
//
// Quick start:
//
//	p := flowguard.NewPolicy(
//	    flowguard.WithPolicyRateLimiter(flowguard.NewRateLimiter(10, 20)),
//	    flowguard.WithPolicyCircuitBreaker(flowguard.NewCircuitBreaker()),
//	    flowguard.WithPolicyRetry(flowguard.NewRetry()),
//	    flowguard.WithPolicyBulkhead(flowguard.NewBulkhead(50)),
//	)
//	err := p.Do(ctx, func(ctx context.Context) error {
//	    return callExternalService(ctx)
//	})
package flowguard
