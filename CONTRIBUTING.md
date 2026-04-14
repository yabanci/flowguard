# Contributing to flowguard

Thanks for your interest in contributing!

## Getting Started

```bash
git clone https://github.com/yabanci/flowguard.git
cd flowguard
go test ./... -race
```

Requires Go 1.21+.

## Development Workflow

1. Fork the repo and create a feature branch from `main`
2. Make your changes
3. Run the checks:

```bash
go vet ./...
go test ./... -race -count=1
go test -bench=. ./benchmarks/ -benchmem
```

4. Open a pull request against `main`

## Guidelines

- **Zero dependencies** — stdlib only. This is a hard rule.
- **Zero allocations on hot path** — run benchmarks before and after your change.
- **Thread-safe** — all public types must be safe for concurrent use.
- **Tests** — new features need tests. Bug fixes need a regression test.
- **No breaking changes** — if you need to change a public API, open an issue first.
- **Conventional commits** — `feat:`, `fix:`, `test:`, `docs:`, `refactor:`, `chore:`

## What We're Looking For

- Bug fixes with reproduction tests
- Performance improvements (with benchmark proof)
- New backoff strategies for Retry
- Better documentation and examples
- Edge case tests

## Code Style

- Follow `gofmt` / `goimports`
- Keep functions short
- Comments should explain *why*, not *what*
- Godoc on all public types and functions

## Reporting Issues

Open an issue with:
- What you expected
- What happened
- Minimal reproduction (if a bug)

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
