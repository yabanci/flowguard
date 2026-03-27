# flowguard

[![Go Reference](https://pkg.go.dev/badge/github.com/yabanci/flowguard.svg)](https://pkg.go.dev/github.com/yabanci/flowguard)
[![Go Report Card](https://goreportcard.com/badge/github.com/yabanci/flowguard)](https://goreportcard.com/report/github.com/yabanci/flowguard)

Resilience primitives for Go: rate limiting, circuit breaking, and retry — in one dependency-free module.

## Features

- **Token bucket**, **sliding window**, and **AIMD** rate limiters
- **Circuit breaker** with configurable thresholds and custom trip functions
- **Retry** with exponential backoff, jitter, and retry predicates
- **Policy** — compose all three into a single call wrapper
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
)

err := policy.Do(ctx, func(ctx context.Context) error {
    resp, err := http.Get("https://api.example.com/data")
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode >= 500 {
        return fmt.Errorf("server error: %d", resp.StatusCode)
    }
    // process response...
    return nil
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

This is the one you won't find in other Go libraries. It adapts the rate limit based on success/failure signals — same algorithm TCP uses for congestion control.

```go
rl := flowguard.NewAIMDLimiter(10, 2, 50)

// on success: limit goes up by 1
rl.OnSuccess()

// on failure (e.g., 429 from server): limit halves
rl.OnFailure()
```

Great for auto-tuning against APIs that don't publish their rate limits.

## Circuit Breaker

```go
cb := flowguard.NewCircuitBreaker(
    flowguard.WithFailureThreshold(5),
    flowguard.WithOpenTimeout(30 * time.Second),
    flowguard.WithSuccessThreshold(2),
)

err := cb.Do(ctx, func(ctx context.Context) error {
    return callExternalService(ctx)
})

// err is ErrCircuitOpen if breaker is tripped
```

Custom trip function for more complex logic:

```go
cb := flowguard.NewCircuitBreaker(
    flowguard.WithTripFunc(func(c flowguard.Counts) bool {
        // trip when >50% failure rate over 10+ requests
        return c.Requests >= 10 && c.TotalFailures*2 > c.Requests
    }),
)
```

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

err := r.Do(ctx, func(ctx context.Context) error {
    return callFlaky(ctx)
})
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

Attach to any component:

```go
cb := flowguard.NewCircuitBreaker(
    flowguard.WithCircuitBreakerObserver(myPrometheusObserver),
)
```

## Benchmarks

Measured on Apple M1, Go 1.21:

| Benchmark | ns/op | allocs/op |
|---|---|---|
| TokenBucket.Allow | ~60 | 0 |
| CircuitBreaker.Do | ~100 | 0 |
| Policy.Do (all three) | ~165 | 0 |
| RateLimiter (parallel) | ~162 | 0 |
| SlidingWindow.Allow | ~50 | 0 |

## Design Decisions

**Why Retry → CB → RL ordering?** Retry wraps everything so transient failures get retried. Circuit breaker is checked before the rate limiter — no point acquiring a rate limit token if the circuit is open. Rate limiter is innermost because it controls actual outgoing traffic.

**Why rate limit rejections don't trip the circuit breaker?** Your own rate limiter rejecting a call isn't a downstream failure. If it counted as one, high traffic would trip your breaker — the opposite of what you want.

**Why AIMD?** Most Go rate limiters are static. But if you're calling an API that rate-limits you with 429s, you want the limit to adapt. AIMD gives you TCP-like congestion control: slow ramp up, fast backoff.

**Why no dependencies?** Rate limiting and circuit breaking are infrastructure-level primitives. Adding a dependency tree for something that's fundamentally `sync.Mutex` + `time.Now()` doesn't make sense.

## License

MIT
