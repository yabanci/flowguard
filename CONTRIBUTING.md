# Contributing to flowguard

Thanks for your interest in contributing!

## Getting started

```bash
git clone https://github.com/yabanci/flowguard.git
cd flowguard
go test -race ./...
```

Requires Go 1.22+.

## Hard rules

- **Zero dependencies** — stdlib only. This is non-negotiable. If a change needs an external import, it belongs in examples or a separate module.
- **Zero allocations on the hot path** — check with `go test -bench . -benchmem ./...`. A new primitive must not regress existing benchmarks.
- **Race-free** — all changes must pass `go test -race -count=10 ./...`.
- **One PR per concern** — don't bundle unrelated fixes. Small, reviewable diffs.

## Development workflow

1. Fork the repo and create a branch: `feat/your-feature` or `fix/the-bug`
2. Make your changes with tests
3. Run the full check suite:

```bash
go vet ./...
go test -race -count=5 ./...
go test -bench=. -benchmem ./benchmarks/
golangci-lint run ./...
```

4. Open a pull request against `main`

## Adding a new primitive

1. Create a new package under the repo root: `yourprimitive/yourprimitive.go`
2. Implement the `observer.Observer` interface for metrics hooks
3. Accept a `clock.Clock` via `WithClock(c clock.Clock)` so tests don't need real timers
4. Add unit tests with table-driven cases and a concurrent stress test
5. Add an example in `examples/yourprimitive/main.go`
6. Add an entry to the `Policy` struct if it fits the composition model

## clock.Clock contract

The `Clock` interface (`Now`, `Sleep`, `After`) exists so tests can use `fakeclock` instead of real timers. **Never** call `time.Sleep`, `time.After`, or `time.NewTimer` directly in a primitive — always use the clock:

```go
// Blocking sleep — use for simple delays
r.clock.Sleep(delay)

// Context-cancellable sleep — required when ctx must be respected
select {
case <-ctx.Done():
    return ctx.Err()
case <-r.clock.After(delay):
}
```

Using a goroutine + `clock.Sleep` is a transient goroutine leak when the context is cancelled mid-sleep. Use `clock.After` instead.

## Policy execution order

The `Policy.Do` execution order is fixed:

```
Fallback → Retry → Hedge → CircuitBreaker → Bulkhead → RateLimiter → fn()
```

Bulkhead and rate-limiter rejections must **not** register as circuit breaker failures — they are capacity limits, not service errors. See `buildCBWrapped` in `policy.go`.

## Testing invariants

Invariant tests live in `invariant_test.go`. Add an invariant test for any property that must always hold concurrently, e.g. "bulkhead never exceeds max concurrency" or "CB never lets through more than halfOpenMaxCalls".

Leak tests live in `leak_test.go`. Add one if your primitive spawns goroutines.

## Observer interface

Every primitive must accept an observer and default to `observer.Noop{}`:

```go
type Observer interface {
    OnSuccess(latency time.Duration)
    OnFailure(err error, latency time.Duration)
    OnRetry(attempt int, err error)
    OnStateChange(from, to observer.State)
    OnRateLimited()
}
```

Never import a metrics library — the observer pattern keeps flowguard dependency-free.

## Code style

- Follow `gofmt` / `goimports`
- Keep functions short and focused
- Comments explain *why*, not *what*
- Godoc on all exported types and functions
- Conventional commits: `feat:`, `fix:`, `test:`, `docs:`, `refactor:`, `chore:`

## Reporting bugs

Open an issue with:
- What you expected
- What happened
- Minimal reproduction case (ideally a failing test)

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
