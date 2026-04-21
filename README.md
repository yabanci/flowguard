# flowguard

[![Go Reference](https://pkg.go.dev/badge/github.com/yabanci/flowguard.svg)](https://pkg.go.dev/github.com/yabanci/flowguard)
[![Go Report Card](https://goreportcard.com/badge/github.com/yabanci/flowguard)](https://goreportcard.com/report/github.com/yabanci/flowguard)
[![CI](https://github.com/yabanci/flowguard/actions/workflows/ci.yml/badge.svg)](https://github.com/yabanci/flowguard/actions/workflows/ci.yml)

Resilience primitives for Go: rate limiting, circuit breaking, retry, hedged requests, bulkhead, load shedding — in one dependency-free module.

## Features

- **Rate limiting** — token bucket, sliding window, and adaptive AIMD
- **Circuit breaker** — classic (consecutive failures) and adaptive (sliding window error rate)
- **Retry** — exponential/constant backoff, full jitter, retry predicates
- **Hedged requests** — tail latency optimization (Google's "Tail at Scale" pattern)
- **Bulkhead** — concurrency limiter to isolate failures
- **Load shedding** — adaptive server-side backpressure (Netflix-style)
- **Policy** — compose any combination into a single call wrapper
- **Fallback** — define what happens when everything fails
- **HTTP middleware** — drop-in server and client middleware
- **Zero dependencies** — stdlib only
- **Zero allocations** on the hot path
- **Observable** — plug in your own metrics via the `Observer` interface

## Install

```bash
go get github.com/yabanci/flowguard
```

Requires Go 1.21+.

## Quick Start

```go
policy := flowguard.NewPolicy(
    flowguard.WithPolicyRateLimiter(flowguard.NewRateLimiter(10, 20)),
    flowguard.WithPolicyCircuitBreaker(flowguard.NewCircuitBreaker()),
    flowguard.WithPolicyRetry(flowguard.NewRetry()),
    flowguard.WithPolicyBulkhead(flowguard.NewBulkhead(50)),
    flowguard.WithPolicyFallback(func(ctx context.Context, err error) error {
        return cachedResponse() // serve stale data
    }),
)

err := policy.Do(ctx, func(ctx context.Context) error {
    return callExternalService(ctx)
})
```

## Rate Limiter

Three strategies, same interface:

```go
// Token bucket — classic, good default
rl := flowguard.NewRateLimiter(rate, burst)

// Sliding window — fixed memory, good for API quotas
rl := flowguard.NewSlidingWindowLimiter(100, time.Minute)

// AIMD — adapts to backpressure, like TCP congestion control
rl := flowguard.NewAIMDLimiter(10, 2, 50)
```

```go
rl.Allow()              // non-blocking, returns bool
rl.Wait(ctx)            // blocks until token available or ctx cancelled
rl.Reserve()            // how long until next token (without consuming)
```

### AIMD (Additive Increase, Multiplicative Decrease)

Adapts the rate limit based on success/failure signals — same algorithm TCP uses for congestion control.

```go
rl := flowguard.NewAIMDLimiter(10, 2, 50)
rl.OnSuccess()  // limit goes up by 1
rl.OnFailure()  // limit halves
```

Great for auto-tuning against APIs that don't publish their rate limits.

## Circuit Breaker

Two modes: classic (consecutive failures) and adaptive (error rate over a sliding window).

```go
// Classic — trips after 5 consecutive failures
cb := flowguard.NewCircuitBreaker(
    flowguard.WithFailureThreshold(5),
    flowguard.WithOpenTimeout(30 * time.Second),
)

// Adaptive — trips when error rate exceeds 50% over last 100 calls
cb := flowguard.NewAdaptiveCircuitBreaker(100, 0.5, 10)
```

The adaptive mode is way more robust — a single success doesn't reset the failure counter. It tracks the actual error rate over a sliding window and trips when things are genuinely broken.

```go
cb.ErrorRate()  // current error rate (0.0-1.0)
cb.State()      // Closed, Open, or HalfOpen
```

## Hedged Requests

If the primary call is slow, fire a second one after a delay and take whichever finishes first. Dramatically reduces tail latency.

```go
h := flowguard.NewHedge(50 * time.Millisecond) // hedge after P95 latency

err := h.Do(ctx, func(ctx context.Context) error {
    return callService(ctx) // must be idempotent!
})
```

If primary fails immediately, the hedge fires right away instead of waiting. Use `WithMaxHedges(2)` for up to 3 total attempts.

## Bulkhead

Limits concurrent access to prevent one slow dependency from eating all goroutines.

```go
b := flowguard.NewBulkhead(50,
    flowguard.WithMaxWaitDuration(100 * time.Millisecond),
)

err := b.Do(ctx, func(ctx context.Context) error {
    return callSlowService(ctx)
})
// err is ErrBulkheadFull if all slots taken
```

## Load Shedding

Server-side adaptive concurrency limiting. Uses AIMD to find the right concurrency level automatically — increases on success, halves on latency spikes or errors.

```go
ls := flowguard.NewLoadShedder(
    20,                  // initial limit
    100*time.Millisecond, // latency threshold
    flowguard.WithLoadShedLimits(5, 200),
)

err := ls.Do(ctx, func(ctx context.Context) error {
    return handleRequest(ctx)
})
// err is ErrLoadShed if server is overloaded
```

## HTTP Middleware

Drop-in protection for HTTP servers and clients:

```go
// Server middleware
policy := flowguard.NewPolicy(
    flowguard.WithPolicyRateLimiter(flowguard.NewRateLimiter(100, 200)),
    flowguard.WithPolicyBulkhead(flowguard.NewBulkhead(50)),
)
protected := flowguard.HTTPMiddleware(policy)(mux)
http.ListenAndServe(":8080", protected)
```

```go
// Client middleware
client := &http.Client{
    Transport: flowguard.HTTPClientMiddleware(policy)(http.DefaultTransport),
}
```

The server middleware maps errors to HTTP status codes: `429` for rate limiting, `503` for circuit open / bulkhead full / load shed.

## Retry

```go
r := flowguard.NewRetry(
    flowguard.WithMaxRetries(3),
    flowguard.WithExponentialBackoff(100 * time.Millisecond),
    flowguard.WithMaxBackoff(30 * time.Second),
    flowguard.WithJitter(0.2),
    flowguard.WithRetryIf(func(err error) bool {
        return isTransient(err)
    }),
)
```

## Observability

Implement the `Observer` interface to hook into your metrics:

```go
type Observer interface {
    OnSuccess(latency time.Duration)
    OnFailure(err error, latency time.Duration)
    OnRetry(attempt int, err error)
    OnStateChange(from, to State)
    OnRateLimited()
}
```

## Benchmarks

Measured on Apple M1, Go 1.21:

| Benchmark | ns/op | allocs/op |
|---|---|---|
| TokenBucket.Allow | ~60 | 0 |
| CircuitBreaker.Do | ~100 | 0 |
| Policy.Do (full stack) | ~165 | 0 |
| RateLimiter (parallel) | ~162 | 0 |
| SlidingWindow.Allow | ~50 | 0 |

## Design Decisions

**Why this ordering?** `Fallback → Retry → Hedge → CircuitBreaker → Bulkhead → RateLimiter → fn()`. Each layer adds protection without interfering with the others. Rate limit rejections don't trip the circuit breaker. Circuit open errors aren't retried. Fallback only fires after everything else fails.

**Why adaptive circuit breaking?** Consecutive failure counts are fragile — one success resets the counter even if 90% of calls are failing. The sliding window tracks actual error rate, which is what you care about in production.

**Why hedged requests?** Google showed that hedging after P95 latency reduces P99 by 4-5x with only ~5% extra load. Most Go libraries don't offer this.

**Why load shedding?** Static concurrency limits require tuning. The AIMD-based shedder finds the right limit automatically — same idea as your rate limiter, applied server-side.

**Why no dependencies?** These are infrastructure primitives. `sync.Mutex` + `time.Now()` + `chan struct{}` is all you need.

## License

MIT — see [LICENSE](LICENSE).
