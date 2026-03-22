package flowguard

import "errors"

var (
	// ErrCircuitOpen is returned when you try to call through an open circuit breaker.
	ErrCircuitOpen = errors.New("flowguard: circuit breaker is open")

	// ErrTooManyRequests — circuit breaker is half-open and already has max concurrent probes.
	ErrTooManyRequests = errors.New("flowguard: too many requests in half-open state")

	// ErrRateLimited — the rate limiter rejected the request (non-blocking path).
	ErrRateLimited = errors.New("flowguard: rate limited")
)
