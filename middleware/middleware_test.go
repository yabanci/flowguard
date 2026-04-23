package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yabanci/flowguard"
	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/internal/fakeclock"
	"github.com/yabanci/flowguard/middleware"
	"github.com/yabanci/flowguard/ratelimit"
)

var errBoom = errors.New("boom")

func TestHTTPServer_PassThrough(t *testing.T) {
	p := flowguard.NewPolicy()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	wrapped := middleware.HTTPServer(p)(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHTTPServer_RateLimited(t *testing.T) {
	clk := fakeclock.New(time.Now())
	rl := ratelimit.NewTokenBucket(100, 1, ratelimit.WithClock(clk))

	p := flowguard.NewPolicy(flowguard.WithRateLimiter(rl))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := middleware.HTTPServer(p)(handler)

	req1 := httptest.NewRequest("GET", "/", nil)
	rec1 := httptest.NewRecorder()
	wrapped.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", rec1.Code)
	}

	clk.Advance(time.Second)

	req2 := httptest.NewRequest("GET", "/", nil)
	rec2 := httptest.NewRecorder()
	wrapped.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("second request should pass after refill, got %d", rec2.Code)
	}
}

func TestHTTPServer_CircuitOpen(t *testing.T) {
	clk := fakeclock.New(time.Now())
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithClock(clk),
	)
	p := flowguard.NewPolicy(flowguard.WithCircuitBreaker(cb))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := middleware.HTTPServer(p)(handler)

	_ = cb.Do(context.Background(), func(_ context.Context) error { return errBoom })

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when circuit is open, got %d", rec.Code)
	}
}
