package flowguard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPMiddleware_PassThrough(t *testing.T) {
	p := NewPolicy() // no components = pass through

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	wrapped := HTTPMiddleware(p)(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHTTPMiddleware_RateLimited(t *testing.T) {
	clk := newMockClock(time.Now())
	rl := NewRateLimiter(100, 1, WithRateLimiterClock(clk)) // burst of 1

	p := NewPolicy(WithPolicyRateLimiter(rl))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := HTTPMiddleware(p)(handler)

	// first request uses the token
	req1 := httptest.NewRequest("GET", "/", nil)
	rec1 := httptest.NewRecorder()
	wrapped.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", rec1.Code)
	}

	// refill so we don't block forever — advance time
	clk.Advance(time.Second)

	req2 := httptest.NewRequest("GET", "/", nil)
	rec2 := httptest.NewRecorder()
	wrapped.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("second request should pass after refill, got %d", rec2.Code)
	}
}

func TestHTTPMiddleware_CircuitOpen(t *testing.T) {
	clk := newMockClock(time.Now())
	cb := NewCircuitBreaker(
		WithFailureThreshold(1),
		WithCircuitBreakerClock(clk),
	)
	p := NewPolicy(WithPolicyCircuitBreaker(cb))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := HTTPMiddleware(p)(handler)

	// trip the CB directly
	cb.Do(context.Background(), func(_ context.Context) error { return errBoom })

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when circuit is open, got %d", rec.Code)
	}
}
