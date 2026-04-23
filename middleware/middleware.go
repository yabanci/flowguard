// Package middleware provides drop-in net/http server and client
// wrappers for a flowguard.Policy.
package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/yabanci/flowguard"
	"github.com/yabanci/flowguard/bulkhead"
	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/loadshed"
	"github.com/yabanci/flowguard/ratelimit"
)

// HTTPServer wraps an http.Handler with a Policy. If the policy rejects
// the request it returns the appropriate HTTP status code without
// calling the handler.
//
// Usage:
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("/api", handler)
//	protected := middleware.HTTPServer(policy)(mux)
//	http.ListenAndServe(":8080", protected)
func HTTPServer(p *flowguard.Policy) func(http.Handler) http.Handler {
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

// HTTPClient wraps an http.RoundTripper with a Policy. Useful for
// outgoing HTTP calls — retry, circuit break, rate limit.
//
// Usage:
//
//	client := &http.Client{
//	    Transport: middleware.HTTPClient(policy)(http.DefaultTransport),
//	}
func HTTPClient(p *flowguard.Policy) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		return &policyTransport{policy: p, next: next}
	}
}

type policyTransport struct {
	policy *flowguard.Policy
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
		if resp.StatusCode >= 500 {
			return &serverError{code: resp.StatusCode}
		}
		return nil
	})

	if err != nil {
		// policy rejected or fn returned error — drain and close body to
		// avoid connection leaks if we got a 5xx back from the server.
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		return nil, err
	}
	return resp, nil
}

type serverError struct{ code int }

func (e *serverError) Error() string { return http.StatusText(e.code) }

// StatusFor maps a flowguard error to the HTTP status code that the
// default middleware would emit. Exposed so callers writing their own
// middleware can reuse the same mapping.
func StatusFor(err error) int {
	switch {
	case errors.Is(err, ratelimit.ErrLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, circuitbreaker.ErrOpen), errors.Is(err, circuitbreaker.ErrTooManyRequests):
		return http.StatusServiceUnavailable
	case errors.Is(err, bulkhead.ErrFull), errors.Is(err, loadshed.ErrShed):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeErrorResponse(w http.ResponseWriter, err error) {
	http.Error(w, http.StatusText(StatusFor(err)), StatusFor(err))
}
