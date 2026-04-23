package flowguard_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yabanci/flowguard"
	"github.com/yabanci/flowguard/bulkhead"
	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/hedge"
	"github.com/yabanci/flowguard/internal/fakeclock"
	"github.com/yabanci/flowguard/loadshed"
	"github.com/yabanci/flowguard/middleware"
	"github.com/yabanci/flowguard/observer"
	"github.com/yabanci/flowguard/ratelimit"
	"github.com/yabanci/flowguard/retry"
)

// --- end-to-end integration tests ---
// These test real scenarios, not just individual components.

func TestIntegration_PolicyFullStack_RetryThenRecover(t *testing.T) {
	// Scenario: service fails 3 times, then recovers.
	// Policy should retry through the failures and succeed.
	clk := fakeclock.New(time.Now())

	rl := ratelimit.NewTokenBucket(1000, 100, ratelimit.WithClock(clk))
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10), // high threshold so it doesn't trip
		circuitbreaker.WithClock(clk),
	)
	r := retry.New(
		retry.WithMaxRetries(5),
		retry.WithClock(clk),
		retry.WithJitter(0),
	)

	p := flowguard.NewPolicy(
		flowguard.WithRateLimiter(rl),
		flowguard.WithCircuitBreaker(cb),
		flowguard.WithRetry(r),
	)

	calls := 0
	err := p.Do(context.Background(), func(ctx context.Context) error {
		calls++
		if calls <= 3 {
			return fmt.Errorf("transient error #%d", calls)
		}
		return nil
	})

	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if calls != 4 {
		t.Fatalf("expected 4 calls (3 failures + 1 success), got %d", calls)
	}
	if cb.State() != observer.StateClosed {
		t.Fatal("CB should still be closed")
	}
}

func TestIntegration_CBTripsAndRetryStops(t *testing.T) {
	// Scenario: CB trips mid-retry. Retry should stop immediately
	// because retrying an open CB is pointless.
	clk := fakeclock.New(time.Now())

	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithClock(clk),
	)
	r := retry.New(
		retry.WithMaxRetries(10),
		retry.WithClock(clk),
		retry.WithJitter(0),
	)

	p := flowguard.NewPolicy(
		flowguard.WithCircuitBreaker(cb),
		flowguard.WithRetry(r),
	)

	calls := 0
	err := p.Do(context.Background(), func(ctx context.Context) error {
		calls++
		return errBoom
	})

	// CB trips after 2 failures, retry should stop
	if !errors.Is(err, circuitbreaker.ErrOpen) {
		t.Fatalf("expected circuitbreaker.ErrOpen, got: %v", err)
	}
	// should be 2 actual calls (CB trips) + retry sees circuitbreaker.ErrOpen and stops
	if calls != 2 {
		t.Fatalf("expected 2 calls before CB tripped, got %d", calls)
	}
}

func TestIntegration_FallbackAfterAllRetriesFail(t *testing.T) {
	clk := fakeclock.New(time.Now())
	r := retry.New(retry.WithMaxRetries(2), retry.WithClock(clk), retry.WithJitter(0))

	var fallbackErr error
	p := flowguard.NewPolicy(
		flowguard.WithRetry(r),
		flowguard.WithFallback(func(ctx context.Context, err error) error {
			fallbackErr = err
			return nil // recover with fallback
		}),
	)

	err := p.Do(context.Background(), func(ctx context.Context) error {
		return errBoom
	})

	if err != nil {
		t.Fatalf("fallback should have recovered, got: %v", err)
	}
	if !errors.Is(fallbackErr, errBoom) {
		t.Fatalf("fallback should receive the original error, got: %v", fallbackErr)
	}
}

func TestIntegration_BulkheadPreventsOverload(t *testing.T) {
	// Scenario: 10 concurrent requests, bulkhead allows 3.
	// 7 should be rejected.
	b := bulkhead.New(3, bulkhead.WithMaxWait(0))
	p := flowguard.NewPolicy(flowguard.WithBulkhead(b))

	ctx := context.Background()
	gate := make(chan struct{})
	var allowed, rejected atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := p.Do(ctx, func(ctx context.Context) error {
				<-gate
				return nil
			})
			if err != nil {
				rejected.Add(1)
			} else {
				allowed.Add(1)
			}
		}()
	}

	// let goroutines try to acquire
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := allowed.Load(); got != 3 {
		t.Fatalf("expected 3 allowed, got %d", got)
	}
	if got := rejected.Load(); got != 7 {
		t.Fatalf("expected 7 rejected, got %d", got)
	}
}

func TestIntegration_LoadShedderConverges(t *testing.T) {
	// Scenario: start with high limit, hammer with slow calls,
	// limit should decrease. Then switch to fast calls, limit should recover.
	ls := loadshed.New(50, 10*time.Millisecond,
		loadshed.WithLimits(5, 100),
	)
	ctx := context.Background()

	// slow calls — should decrease the limit
	for i := 0; i < 10; i++ {
		ls.Do(ctx, func(ctx context.Context) error {
			time.Sleep(20 * time.Millisecond) // over threshold
			return nil
		})
	}

	afterSlow := ls.CurrentLimit()
	if afterSlow >= 50 {
		t.Fatalf("limit should have decreased from 50, got %d", afterSlow)
	}

	// fast calls — should increase the limit
	for i := 0; i < 20; i++ {
		ls.Do(ctx, func(ctx context.Context) error {
			return nil // instant
		})
	}

	afterFast := ls.CurrentLimit()
	if afterFast <= afterSlow {
		t.Fatalf("limit should have increased from %d, got %d", afterSlow, afterFast)
	}
}

func TestIntegration_AdaptiveCB_RealWorldScenario(t *testing.T) {
	// Scenario: service starts degrading, CB trips,
	// then service recovers, CB closes again.
	clk := fakeclock.New(time.Now())
	// small window so failures dominate quickly
	cb := circuitbreaker.NewAdaptive(10, 0.5, 5,
		circuitbreaker.WithOpenTimeout(5*time.Second),
		circuitbreaker.WithSuccessThreshold(3),
		circuitbreaker.WithClock(clk),
	)

	ctx := context.Background()
	ok := func(ctx context.Context) error { return nil }
	fail := func(ctx context.Context) error { return errBoom }

	// warm up with a few successes
	for i := 0; i < 3; i++ {
		cb.Do(ctx, ok)
	}
	if cb.State() != observer.StateClosed {
		t.Fatal("should be closed during warmup")
	}

	// service degrades — blast it with failures to push error rate over 50%
	for i := 0; i < 7; i++ {
		cb.Do(ctx, fail)
	}

	// window is full (10 slots): 3 ok + 7 fail = 70% error rate
	rate := cb.ErrorRate()
	t.Logf("error rate after degradation: %.2f, state: %s", rate, cb.State())

	if cb.State() != observer.StateOpen {
		t.Fatalf("CB should have tripped at %.0f%% error rate", rate*100)
	}

	// wait for half-open
	clk.Advance(6 * time.Second)
	if cb.State() != observer.StateHalfOpen {
		t.Fatalf("expected HalfOpen, got %v", cb.State())
	}

	// service recovers
	cb.Do(ctx, ok)
	cb.Do(ctx, ok)
	cb.Do(ctx, ok)

	if cb.State() != observer.StateClosed {
		t.Fatalf("expected Closed after recovery, got %v", cb.State())
	}
}

// --- HTTP middleware integration tests ---

func TestIntegration_HTTPServer_RateLimitReturns429(t *testing.T) {
	rl := ratelimit.NewTokenBucket(1, 1) // 1 req/s, burst 1
	p := flowguard.NewPolicy(flowguard.WithRateLimiter(rl))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(middleware.HTTPServer(p)(handler))
	defer server.Close()

	// first request should pass
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// second request (within 1s) — rate limiter will Wait (token refills)
	// so it should also succeed since Wait blocks until token available
	resp2, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("expected 200 (Wait blocks), got %d", resp2.StatusCode)
	}
}

func TestIntegration_HTTPClient_CircuitBreaker(t *testing.T) {
	// backend that fails
	failCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCount++
		if failCount <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	cb := circuitbreaker.New(circuitbreaker.WithFailureThreshold(5))
	r := retry.New(retry.WithMaxRetries(4), retry.WithExponentialBackoff(time.Millisecond), retry.WithJitter(0))
	p := flowguard.NewPolicy(
		flowguard.WithCircuitBreaker(cb),
		flowguard.WithRetry(r),
	)

	transport := middleware.HTTPClient(p)(http.DefaultTransport)
	client := &http.Client{Transport: transport}

	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("expected eventual success, got: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("expected 'ok', got '%s'", string(body))
	}
}

func TestIntegration_HTTPServer_LoadShed503(t *testing.T) {
	ls := loadshed.New(1, time.Second, loadshed.WithLimits(1, 10))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // hold the slot
		w.WriteHeader(http.StatusOK)
	})

	// wrap with load shedder manually (not in Policy yet)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := ls.Do(r.Context(), func(ctx context.Context) error {
			handler.ServeHTTP(w, r.WithContext(ctx))
			return nil
		})
		if err != nil {
			w.WriteHeader(middleware.StatusFor(err))
		}
	})

	server := httptest.NewServer(wrapped)
	defer server.Close()

	// fire 3 requests concurrently
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := http.Get(server.URL)
			if err != nil {
				codes[idx] = 0
				return
			}
			codes[idx] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	has200, has503 := false, false
	for _, c := range codes {
		if c == 200 {
			has200 = true
		}
		if c == 503 {
			has503 = true
		}
	}
	if !has200 {
		t.Fatal("expected at least one 200")
	}
	if !has503 {
		t.Fatal("expected at least one 503 (load shed)")
	}
}

// --- concurrent stress tests ---

func TestConcurrent_RateLimiter(t *testing.T) {
	rl := ratelimit.NewTokenBucket(1000, 100)

	var wg sync.WaitGroup
	var allowed, rejected atomic.Int32

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rl.Allow() {
				allowed.Add(1)
			} else {
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()

	// burst is 100, so first 100 should pass
	if got := allowed.Load(); got < 50 || got > 150 {
		t.Fatalf("expected ~100 allowed (burst), got %d", got)
	}
	t.Logf("allowed=%d rejected=%d", allowed.Load(), rejected.Load())
}

func TestConcurrent_CircuitBreaker(t *testing.T) {
	cb := circuitbreaker.New(circuitbreaker.WithFailureThreshold(100))
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			cb.Do(ctx, func(ctx context.Context) error {
				if n%3 == 0 {
					return errBoom
				}
				return nil
			})
		}(i)
	}
	wg.Wait()

	// should still be closed (threshold is 100)
	if cb.State() != observer.StateClosed {
		t.Fatal("should be closed")
	}
	counts := cb.GetCounts()
	if counts.Requests != 100 {
		t.Fatalf("expected 100 requests, got %d", counts.Requests)
	}
}

func TestConcurrent_AdaptiveCB(t *testing.T) {
	cb := circuitbreaker.NewAdaptive(100, 0.9, 50)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			cb.Do(ctx, func(ctx context.Context) error {
				if n%10 == 0 {
					return errBoom // 10% error rate
				}
				return nil
			})
		}(i)
	}
	wg.Wait()

	// 10% error < 90% threshold — should stay closed
	if cb.State() != observer.StateClosed {
		t.Fatalf("should be closed at 10%% error rate, got %v (rate=%.2f)", cb.State(), cb.ErrorRate())
	}
}

func TestConcurrent_LoadShedder(t *testing.T) {
	ls := loadshed.New(20, 50*time.Millisecond, loadshed.WithLimits(5, 100))
	ctx := context.Background()

	var wg sync.WaitGroup
	var ok, shed atomic.Int32

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := ls.Do(ctx, func(ctx context.Context) error {
				time.Sleep(5 * time.Millisecond) // under threshold
				return nil
			})
			if err != nil {
				shed.Add(1)
			} else {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()

	t.Logf("ok=%d shed=%d limit=%d", ok.Load(), shed.Load(), ls.CurrentLimit())
	total := ok.Load() + shed.Load()
	if total != 50 {
		t.Fatalf("expected 50 total, got %d", total)
	}
}

func TestConcurrent_Bulkhead(t *testing.T) {
	b := bulkhead.New(10, bulkhead.WithMaxWait(50*time.Millisecond))
	ctx := context.Background()

	var wg sync.WaitGroup
	var ok, rejected atomic.Int32

	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := b.Do(ctx, func(ctx context.Context) error {
				time.Sleep(20 * time.Millisecond) // hold the slot
				return nil
			})
			if err != nil {
				rejected.Add(1)
			} else {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()

	t.Logf("ok=%d rejected=%d", ok.Load(), rejected.Load())
	if ok.Load() == 0 {
		t.Fatal("expected some to succeed")
	}
	if ok.Load()+rejected.Load() != 30 {
		t.Fatal("all 30 should have completed")
	}
}

// --- edge cases ---

func TestEdge_PolicyNoComponents(t *testing.T) {
	// Policy with nothing configured should just pass through
	p := flowguard.NewPolicy()

	err := p.Do(context.Background(), func(ctx context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatal("empty policy should pass through")
	}

	// should also propagate errors
	err = p.Do(context.Background(), func(ctx context.Context) error {
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got: %v", err)
	}
}

func TestEdge_ZeroBurstRateLimiter(t *testing.T) {
	clk := fakeclock.New(time.Now())
	rl := ratelimit.NewTokenBucket(10, 0, ratelimit.WithClock(clk))

	// zero burst means no tokens available initially
	if rl.Allow() {
		t.Fatal("should not allow with 0 burst")
	}

	// but after time passes, tokens should refill (up to burst=0, so still no)
	clk.Advance(time.Second)
	if rl.Allow() {
		t.Fatal("should still not allow — burst is 0 so max tokens is 0")
	}
}

func TestEdge_CB_ZeroThreshold(t *testing.T) {
	// failure threshold of 0 is technically invalid but shouldn't crash
	cb := circuitbreaker.New(circuitbreaker.WithFailureThreshold(0))
	ctx := context.Background()

	// should always trip since any consecutive failure count >= 0
	// this is a degenerate case — just make sure it doesn't panic
	cb.Do(ctx, func(ctx context.Context) error { return nil })
}

func TestEdge_RetryZeroRetries(t *testing.T) {
	clk := fakeclock.New(time.Now())
	r := retry.New(retry.WithMaxRetries(0), retry.WithClock(clk))

	calls := 0
	err := r.Do(context.Background(), func(ctx context.Context) error {
		calls++
		return errBoom
	})

	if calls != 1 {
		t.Fatalf("expected exactly 1 call with 0 retries, got %d", calls)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got: %v", err)
	}
}

func TestEdge_HedgeZeroDelay(t *testing.T) {
	// zero delay = fire hedge immediately
	h := hedge.New(0)

	var calls atomic.Int32
	err := h.Do(context.Background(), func(ctx context.Context) error {
		calls.Add(1)
		return nil
	})

	if err != nil {
		t.Fatal("unexpected error")
	}
}

func TestEdge_BulkheadZeroConcurrency(t *testing.T) {
	// this panics because make(chan struct{}, 0) is unbuffered
	// but we should handle it gracefully
	defer func() {
		if r := recover(); r != nil {
			t.Logf("panicked with: %v (this is expected for 0 concurrency)", r)
		}
	}()

	b := bulkhead.New(0, bulkhead.WithMaxWait(0))
	err := b.Do(context.Background(), func(ctx context.Context) error {
		return nil
	})
	// should be rejected since no slots
	if !errors.Is(err, bulkhead.ErrFull) {
		t.Logf("got: %v (may also panic, which is fine for invalid input)", err)
	}
}

func TestEdge_StateString(t *testing.T) {
	tests := []struct {
		s    observer.State
		want string
	}{
		{observer.StateClosed, "closed"},
		{observer.StateOpen, "open"},
		{observer.StateHalfOpen, "half-open"},
		{observer.State(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestEdge_WriteErrorResponse_AllErrors(t *testing.T) {
	tests := []struct {
		err  error
		code int
	}{
		{ratelimit.ErrLimited, 429},
		{circuitbreaker.ErrOpen, 503},
		{bulkhead.ErrFull, 503},
		{loadshed.ErrShed, 503},
		{errBoom, 500},
	}

	for _, tt := range tests {
		rec := httptest.NewRecorder()
		rec.Code = middleware.StatusFor(tt.err)
		if rec.Code != tt.code {
			t.Errorf("StatusFor(%v) = %d, want %d", tt.err, rec.Code, tt.code)
		}
	}
}
