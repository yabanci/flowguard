package flowguard

import (
	"context"
	"net/http"
)

// HTTPMiddleware wraps an http.Handler with a Policy. If the policy rejects
// the request (rate limited, circuit open, bulkhead full, load shed), it
// returns the appropriate HTTP status code without calling the handler.
//
// Usage:
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("/api", handler)
//	protected := flowguard.HTTPMiddleware(policy)(mux)
//	http.ListenAndServe(":8080", protected)
func HTTPMiddleware(p *Policy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			err := p.Do(r.Context(), func(ctx context.Context) error {
				next.ServeHTTP(w, r.WithContext(ctx))
				return nil
			})

			if err != nil {
				writeErrorResponse(w, err)
			}
		})
	}
}

// HTTPClientMiddleware wraps http.RoundTripper with a Policy.
// Useful for outgoing HTTP calls — retry, circuit break, rate limit.
//
// Usage:
//
//	client := &http.Client{
//	    Transport: flowguard.HTTPClientMiddleware(policy)(http.DefaultTransport),
//	}
func HTTPClientMiddleware(p *Policy) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		return &policyTransport{policy: p, next: next}
	}
}

type policyTransport struct {
	policy *Policy
	next   http.RoundTripper
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response

	err := t.policy.Do(req.Context(), func(ctx context.Context) error {
		var rerr error
		resp, rerr = t.next.RoundTrip(req.WithContext(ctx))
		if rerr != nil {
			return rerr
		}
		// treat 5xx as errors for circuit breaker purposes
		if resp.StatusCode >= 500 {
			return &serverError{code: resp.StatusCode}
		}
		return nil
	})

	if err != nil {
		// if we got a response but the policy wrapped the error, return both
		// so the caller can inspect the response body (e.g., for error details)
		if resp != nil {
			return resp, nil
		}
		return nil, err
	}
	return resp, nil
}

type serverError struct {
	code int
}

func (e *serverError) Error() string {
	return http.StatusText(e.code)
}

func writeErrorResponse(w http.ResponseWriter, err error) {
	switch err {
	case ErrRateLimited:
		http.Error(w, "Rate Limited", http.StatusTooManyRequests)
	case ErrCircuitOpen:
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	case ErrBulkheadFull:
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	case ErrLoadShed:
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	default:
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}
