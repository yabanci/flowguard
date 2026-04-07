package flowguard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests use real HTTP servers, real network I/O, real time.
// No mocks. If these pass, the library works.

func TestE2E_UnstableServer_PolicyRecovers(t *testing.T) {
	// Real HTTP server that fails the first 3 requests, then works
	var reqCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		if n <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "error #%d", n)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "success")
	}))
	defer server.Close()

	policy := NewPolicy(
		WithPolicyCircuitBreaker(NewCircuitBreaker(WithFailureThreshold(10))),
		WithPolicyRetry(NewRetry(
			WithMaxRetries(5),
			WithConstantBackoff(10*time.Millisecond),
			WithJitter(0),
		)),
	)

	var body string
	err := policy.Do(context.Background(), func(ctx context.Context) error {
		resp, err := http.Get(server.URL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d: %s", resp.StatusCode, b)
		}
		body = string(b)
		return nil
	})

	if err != nil {
		t.Fatalf("policy should have recovered after retries, got: %v", err)
	}
	if body != "success" {
		t.Fatalf("expected 'success', got '%s'", body)
	}
	if got := reqCount.Load(); got != 4 {
		t.Fatalf("expected 4 requests (3 fail + 1 success), got %d", got)
	}
	t.Logf("server recovered after %d requests", reqCount.Load())
}

func TestE2E_RateLimiter_ActuallyLimitsRPS(t *testing.T) {
	// Verify the rate limiter actually limits real requests per second
	var handled atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rl := NewRateLimiter(20, 5) // 20/s, burst 5
	policy := NewPolicy(WithPolicyRateLimiter(rl))

	start := time.Now()
	for i := 0; i < 15; i++ {
		err := policy.Do(context.Background(), func(ctx context.Context) error {
			resp, err := http.Get(server.URL)
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		})
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// 15 requests at 20/s with burst 5: first 5 instant, remaining 10 at 20/s = ~500ms
	if elapsed < 400*time.Millisecond {
		t.Fatalf("rate limiter didn't throttle: 15 requests completed in %v (expected >400ms)", elapsed)
	}
	if handled.Load() != 15 {
		t.Fatalf("expected 15 handled, got %d", handled.Load())
	}
	t.Logf("15 requests in %v (rate limited at 20/s)", elapsed.Round(time.Millisecond))
}

func TestE2E_CircuitBreaker_ProtectsFromDeadServer(t *testing.T) {
	// Server that always 500s
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cb := NewCircuitBreaker(
		WithFailureThreshold(3),
		WithOpenTimeout(time.Second),
	)

	ctx := context.Background()

	// first 3 calls hit the server
	for i := 0; i < 3; i++ {
		cb.Do(ctx, func(ctx context.Context) error {
			resp, err := http.Get(server.URL)
			if err != nil {
				return err
			}
			resp.Body.Close()
			return fmt.Errorf("server error: %d", resp.StatusCode)
		})
	}

	if cb.State() != StateOpen {
		t.Fatal("CB should be open after 3 failures")
	}

	// next call should be rejected WITHOUT hitting the server
	start := time.Now()
	err := cb.Do(ctx, func(ctx context.Context) error {
		t.Fatal("fn should not be called when circuit is open")
		return nil
	})
	rejectionTime := time.Since(start)

	if err != ErrCircuitOpen {
		t.Fatalf("expected ErrCircuitOpen, got: %v", err)
	}
	if rejectionTime > 5*time.Millisecond {
		t.Fatalf("rejection took %v — should be instant", rejectionTime)
	}
	t.Logf("circuit open, rejection in %v (instant, no network call)", rejectionTime)
}

func TestE2E_Bulkhead_LimitsConcurrentRequests(t *testing.T) {
	// Server that tracks concurrent connections
	var concurrent, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := concurrent.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond) // simulate work
		concurrent.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bh := NewBulkhead(3, WithMaxWaitDuration(2*time.Second))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bh.Do(context.Background(), func(ctx context.Context) error {
				resp, err := http.Get(server.URL)
				if err != nil {
					return err
				}
				resp.Body.Close()
				return nil
			})
		}()
	}
	wg.Wait()

	if p := peak.Load(); p > 3 {
		t.Fatalf("peak concurrent requests was %d, bulkhead should limit to 3", p)
	}
	t.Logf("peak concurrency: %d (max allowed: 3)", peak.Load())
}

func TestE2E_Hedge_ReducesTailLatency(t *testing.T) {
	// Server where ~20% of requests are slow (200ms), rest are fast (5ms)
	var reqCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		if n%5 == 1 { // first request of each batch is slow
			time.Sleep(200 * time.Millisecond)
		} else {
			time.Sleep(5 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	h := NewHedge(30 * time.Millisecond) // hedge after 30ms

	var latencies []time.Duration
	for i := 0; i < 10; i++ {
		start := time.Now()
		err := h.Do(context.Background(), func(ctx context.Context) error {
			req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		})
		lat := time.Since(start)
		latencies = append(latencies, lat)
		if err != nil {
			t.Logf("request %d: error %v (%v)", i, err, lat)
		} else {
			t.Logf("request %d: ok (%v)", i, lat.Round(time.Millisecond))
		}
	}

	// with hedging, no request should take >100ms even if primary is slow
	// (hedge fires at 30ms and the fast path takes ~5ms)
	for i, lat := range latencies {
		if lat > 150*time.Millisecond {
			t.Errorf("request %d took %v — hedging should have kicked in", i, lat)
		}
	}
}

func TestE2E_LoadShedder_ProtectsUnderBurst(t *testing.T) {
	var handled atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
		time.Sleep(30 * time.Millisecond) // simulate work
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ls := NewLoadShedder(5, 50*time.Millisecond, WithLoadShedLimits(2, 20))

	var ok, shed atomic.Int32
	var wg sync.WaitGroup

	// burst 20 concurrent requests through a limit of 5
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := ls.Do(context.Background(), func(ctx context.Context) error {
				resp, err := http.Get(server.URL)
				if err != nil {
					return err
				}
				resp.Body.Close()
				return nil
			})
			if err == ErrLoadShed {
				shed.Add(1)
			} else if err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()

	t.Logf("20 burst: ok=%d shed=%d server_handled=%d limit=%d",
		ok.Load(), shed.Load(), handled.Load(), ls.CurrentLimit())

	if shed.Load() == 0 {
		t.Fatal("load shedder should have rejected some requests under burst")
	}
	if ok.Load() == 0 {
		t.Fatal("at least some requests should have succeeded")
	}
	// server should have handled fewer than 20 (some were shed)
	if handled.Load() >= 20 {
		t.Fatal("load shedder didn't protect the server — all 20 got through")
	}
}

func TestE2E_Fallback_ServesStaleData(t *testing.T) {
	// Server that's completely down
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	policy := NewPolicy(
		WithPolicyCircuitBreaker(NewCircuitBreaker(WithFailureThreshold(2))),
		WithPolicyRetry(NewRetry(
			WithMaxRetries(1),
			WithConstantBackoff(time.Millisecond),
		)),
		WithPolicyFallback(func(ctx context.Context, err error) error {
			// serve cached/stale data instead of failing
			return nil
		}),
	)

	// should succeed via fallback even though server is down
	err := policy.Do(context.Background(), func(ctx context.Context) error {
		resp, err := http.Get(server.URL)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	})

	if err != nil {
		t.Fatalf("fallback should have saved us, got: %v", err)
	}
}

func TestE2E_FullStack_RealWorldScenario(t *testing.T) {
	// Simulate a real microservice call pattern:
	// - Server is flaky (30% errors)
	// - Rate limited to 50/s
	// - Circuit breaker with adaptive error rate
	// - Retry with backoff
	// - Bulkhead limits concurrency to 5
	// - Fallback returns cached response

	var serverCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := serverCalls.Add(1)
		if n%3 == 0 { // 33% error rate
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data")
	}))
	defer server.Close()

	policy := NewPolicy(
		WithPolicyRateLimiter(NewRateLimiter(50, 10)),
		WithPolicyCircuitBreaker(NewAdaptiveCircuitBreaker(50, 0.6, 10)),
		WithPolicyRetry(NewRetry(
			WithMaxRetries(2),
			WithExponentialBackoff(5*time.Millisecond),
			WithMaxBackoff(50*time.Millisecond),
		)),
		WithPolicyBulkhead(NewBulkhead(5, WithMaxWaitDuration(500*time.Millisecond))),
		WithPolicyFallback(func(ctx context.Context, err error) error {
			return nil // cached fallback
		}),
	)

	var ok, fallbacks atomic.Int32
	var wg sync.WaitGroup

	// 10 goroutines, 5 requests each
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				var gotData bool
				err := policy.Do(context.Background(), func(ctx context.Context) error {
					req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return err
					}
					defer resp.Body.Close()
					if resp.StatusCode != 200 {
						return fmt.Errorf("status %d", resp.StatusCode)
					}
					gotData = true
					return nil
				})

				if err == nil {
					if gotData {
						ok.Add(1)
					} else {
						fallbacks.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()

	total := ok.Load() + fallbacks.Load()
	t.Logf("50 requests: ok=%d fallbacks=%d server_calls=%d",
		ok.Load(), fallbacks.Load(), serverCalls.Load())

	if total != 50 {
		t.Fatalf("expected all 50 to succeed (via data or fallback), got %d", total)
	}
}
