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
import (
    "github.com/yabanci/flowguard"
    "github.com/yabanci/flowguard/bulkhead"
    "github.com/yabanci/flowguard/circuitbreaker"
    "github.com/yabanci/flowguard/ratelimit"
    "github.com/yabanci/flowguard/retry"
)

policy := flowguard.NewPolicy(
    flowguard.WithRateLimiter(ratelimit.NewTokenBucket(10, 20)),
    flowguard.WithCircuitBreaker(circuitbreaker.New()),
    flowguard.WithRetry(retry.New()),
    flowguard.WithBulkhead(bulkhead.New(50)),
    flowguard.WithFallback(func(ctx context.Context, err error) error {
        return cachedResponse() // serve stale data
    }),
)

err := policy.Do(ctx, func(ctx context.Context) error {
    return callExternalService(ctx)
})
```

The root `flowguard` package only owns `Policy`. Every primitive lives in
its own subpackage and is usable standalone: `ratelimit`, `circuitbreaker`,
`retry`, `hedge`, `bulkhead`, `loadshed`, `middleware`, plus `observer`
and `clock` for the shared interfaces.

## Rate Limiter

Three strategies, same `*ratelimit.Limiter` type:

```go
// Token bucket — classic, good default
rl := ratelimit.NewTokenBucket(rate, burst)

// Sliding window — fixed memory, good for API quotas
rl := ratelimit.NewSlidingWindow(100, time.Minute)

// AIMD — adapts to backpressure, like TCP congestion control
rl := ratelimit.NewAIMD(10, 2, 50)
```

```go
rl.Allow()              // non-blocking, returns bool
rl.Wait(ctx)            // blocks until token available or ctx cancelled
rl.Reserve()            // how long until next token (without consuming)
```

### AIMD (Additive Increase, Multiplicative Decrease)

Adapts the rate limit based on success/failure signals — same algorithm TCP uses for congestion control.

```go
rl := ratelimit.NewAIMD(10, 2, 50)
rl.OnSuccess()  // limit goes up by 1
rl.OnFailure()  // limit halves
```

Great for auto-tuning against APIs that don't publish their rate limits.

## Circuit Breaker

Two modes: classic (consecutive failures) and adaptive (error rate over a sliding window).

```go
// Classic — trips after 5 consecutive failures
cb := circuitbreaker.New(
    circuitbreaker.WithFailureThreshold(5),
    circuitbreaker.WithOpenTimeout(30 * time.Second),
)

// Adaptive — trips when error rate exceeds 50% over last 100 calls
cb := circuitbreaker.NewAdaptive(100, 0.5, 10)
```

The adaptive mode is way more robust — a single success doesn't reset the failure counter. It tracks the actual error rate over a sliding window and trips when things are genuinely broken.

```go
cb.ErrorRate()  // current error rate (0.0-1.0)
cb.State()      // Closed, Open, or HalfOpen
```

## Hedged Requests

If the primary call is slow, fire a second one after a delay and take whichever finishes first. Dramatically reduces tail latency.

```go
h := hedge.New(50 * time.Millisecond) // hedge after P95 latency

err := h.Do(ctx, func(ctx context.Context) error {
    return callService(ctx) // must be idempotent!
})
```

If primary fails immediately, the hedge fires right away instead of waiting. Use `hedge.WithMaxHedges(2)` for up to 3 total attempts.

## Bulkhead

Limits concurrent access to prevent one slow dependency from eating all goroutines.

```go
b := bulkhead.New(50,
    bulkhead.WithMaxWait(100 * time.Millisecond),
)

err := b.Do(ctx, func(ctx context.Context) error {
    return callSlowService(ctx)
})
// err is bulkhead.ErrFull if all slots taken
```

## Load Shedding

Server-side adaptive concurrency limiting. Uses AIMD to find the right concurrency level automatically — increases on success, halves on latency spikes or errors.

```go
ls := loadshed.New(
    20,                  // initial limit
    100*time.Millisecond, // latency threshold
    loadshed.WithLimits(5, 200),
)

err := ls.Do(ctx, func(ctx context.Context) error {
    return handleRequest(ctx)
})
// err is loadshed.ErrShed if server is overloaded
```

## HTTP Middleware

Drop-in protection for HTTP servers and clients:

```go
// Server middleware
policy := flowguard.NewPolicy(
    flowguard.WithRateLimiter(ratelimit.NewTokenBucket(100, 200)),
    flowguard.WithBulkhead(bulkhead.New(50)),
)
protected := middleware.HTTPServer(policy)(mux)
http.ListenAndServe(":8080", protected)
```

```go
// Client middleware
client := &http.Client{
    Transport: middleware.HTTPClient(policy)(http.DefaultTransport),
}
```

The server middleware maps errors to HTTP status codes: `429` for rate limiting, `503` for circuit open / bulkhead full / load shed. `middleware.StatusFor(err)` exposes the mapping if you roll your own handler.

## Retry

```go
r := retry.New(
    retry.WithMaxRetries(3),
    retry.WithExponentialBackoff(100 * time.Millisecond),
    retry.WithMaxBackoff(30 * time.Second),
    retry.WithJitter(0.2),
    retry.WithRetryIf(func(err error) bool {
        return isTransient(err)
    }),
)
```

Wrap an error with `retry.Permanent(err)` to stop retrying on it.

## Observability

Implement `observer.Observer` to hook into your metrics:

```go
import "github.com/yabanci/flowguard/observer"

type Observer interface {
    OnSuccess(latency time.Duration)
    OnFailure(err error, latency time.Duration)
    OnRetry(attempt int, err error)
    OnStateChange(from, to observer.State)
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

## Changelog

### v0.3.0 (upcoming)

**Bug fixes:**
- `retry`: replaced sleep goroutine with `clock.After(d)` — eliminates transient goroutine leak per retry attempt on context cancellation
- `ratelimit.Wait`: same `clock.After` fix — no goroutine per sleep cycle
- `ratelimit.NewAIMD`: clamps `min ≥ 1` — prevents permanent deadlock when `aimdCurrent` reaches 0 via halving
- `loadshed.WithLimits`: clamps `min ≥ 1` — same class of bug
- `policy.Do`: `buildCBWrapped` now receives `wrapped` (with bulkhead applied) instead of bare `fn` — bulkhead was silently discarded when both CB and BH were configured
- `policy.Do`: bulkhead `ErrFull` is now treated the same as rate-limit rejections — does not count as a CB failure

**API additions:**
- `clock.Clock` interface gains `After(d time.Duration) <-chan time.Time` — enables context-cancellable sleeping without goroutines in dependent packages

### v0.2.0

Initial public release. All primitives stable.

---

## License

MIT — see [LICENSE](LICENSE).
