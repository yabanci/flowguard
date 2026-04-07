package flowguard

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

// noopObserver is the default — does nothing.
type noopObserver struct{}

func (noopObserver) OnSuccess(time.Duration)            {}
func (noopObserver) OnFailure(error, time.Duration)     {}
func (noopObserver) OnRetry(int, error)                 {}
func (noopObserver) OnStateChange(State, State)         {}
func (noopObserver) OnRateLimited()                     {}

// State represents circuit breaker state.
type State int

const (
	StateClosed   State = iota
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
