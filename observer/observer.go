// Package observer defines the Observer interface every flowguard
// primitive uses to emit events, plus the State enum for circuit
// breaker transitions. Implement Observer to hook in Prometheus,
// OpenTelemetry, or any other metrics system.
package observer

import "time"

// Observer gets notified about events. Implement this to hook in
// your metrics system (prometheus, otel, whatever).
// All methods must be safe for concurrent use.
type Observer interface {
	OnSuccess(latency time.Duration)
	OnFailure(err error, latency time.Duration)
	OnRetry(attempt int, err error)
	OnStateChange(from, to State)
	OnRateLimited()
}

// Noop is the default observer that does nothing.
// Exposed so users can embed it and only override the methods they need.
type Noop struct{}

func (Noop) OnSuccess(time.Duration)        {}
func (Noop) OnFailure(error, time.Duration) {}
func (Noop) OnRetry(int, error)             {}
func (Noop) OnStateChange(State, State)     {}
func (Noop) OnRateLimited()                 {}

// State represents circuit breaker state.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}
