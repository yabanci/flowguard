package flowguard

import (
	"context"
	"sync"
	"testing"
	"time"
)

// mockClock lets us control time in tests without real sleeps.
type mockClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMockClock(t time.Time) *mockClock {
	return &mockClock{now: t}
}

func (c *mockClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mockClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *mockClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestTokenBucket_Allow(t *testing.T) {
	clk := newMockClock(time.Now())
	rl := NewRateLimiter(10, 3, WithRateLimiterClock(clk))

	// should allow burst
	for i := 0; i < 3; i++ {
		if !rl.Allow() {
			t.Fatalf("expected Allow() to be true on call %d", i+1)
		}
	}

	// burst exhausted
	if rl.Allow() {
		t.Fatal("expected Allow() to be false after burst exhausted")
	}

	// advance time, should refill
	clk.Advance(200 * time.Millisecond) // 10/s * 0.2s = 2 tokens
	if !rl.Allow() {
		t.Fatal("expected Allow() to be true after refill")
	}
	if !rl.Allow() {
		t.Fatal("expected second Allow() to be true")
	}
	if rl.Allow() {
		t.Fatal("should be exhausted again")
	}
}

func TestTokenBucket_Wait(t *testing.T) {
	clk := newMockClock(time.Now())
	rl := NewRateLimiter(10, 1, WithRateLimiterClock(clk))

	ctx := context.Background()

	// first call consumes the one token
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// next Wait should block and then succeed after clock advances
	done := make(chan error, 1)
	go func() {
		done <- rl.Wait(ctx)
	}()

	// give the goroutine a moment to start (ugh, but mock clock Sleep will advance)
	time.Sleep(10 * time.Millisecond)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error from Wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait didn't return in time — mock clock should have advanced via Sleep")
	}
}

func TestTokenBucket_WaitCancelled(t *testing.T) {
	clk := newMockClock(time.Now())
	rl := NewRateLimiter(1, 1, WithRateLimiterClock(clk))

	// drain the token
	rl.Allow()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := rl.Wait(ctx)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestTokenBucket_Reserve(t *testing.T) {
	clk := newMockClock(time.Now())
	rl := NewRateLimiter(10, 2, WithRateLimiterClock(clk))

	// full bucket — reserve should be 0
	if d := rl.Reserve(); d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}

	// drain tokens
	rl.Allow()
	rl.Allow()

	d := rl.Reserve()
	if d <= 0 {
		t.Fatal("expected positive duration when bucket is empty")
	}
}

func TestSlidingWindow_Basic(t *testing.T) {
	clk := newMockClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	rl := NewSlidingWindowLimiter(5, time.Second, WithRateLimiterClock(clk))

	// should allow up to limit
	for i := 0; i < 5; i++ {
		if !rl.Allow() {
			t.Fatalf("expected allow on call %d", i+1)
		}
	}

	// 6th should fail
	if rl.Allow() {
		t.Fatal("should reject after limit")
	}
}

func TestSlidingWindow_Expiry(t *testing.T) {
	clk := newMockClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	rl := NewSlidingWindowLimiter(3, time.Second, WithRateLimiterClock(clk))

	// fill up
	for i := 0; i < 3; i++ {
		rl.Allow()
	}
	if rl.Allow() {
		t.Fatal("should be at limit")
	}

	// advance past the full window
	clk.Advance(1100 * time.Millisecond)

	// old counts should be gone
	if !rl.Allow() {
		t.Fatal("expected allow after window passed")
	}
}

func TestAIMD_Basic(t *testing.T) {
	rl := NewAIMDLimiter(5, 1, 20)

	// should allow 5 requests
	for i := 0; i < 5; i++ {
		if !rl.Allow() {
			t.Fatalf("expected allow on call %d", i+1)
		}
	}
	if rl.Allow() {
		t.Fatal("should reject at limit")
	}
}

func TestAIMD_Increase(t *testing.T) {
	rl := NewAIMDLimiter(3, 1, 10)

	rl.OnSuccess() // limit → 4
	rl.OnSuccess() // limit → 5
	if got := rl.CurrentLimit(); got != 5 {
		t.Fatalf("expected limit 5, got %d", got)
	}

	// should be able to use all 5
	for i := 0; i < 5; i++ {
		if !rl.Allow() {
			t.Fatalf("expected allow on call %d", i+1)
		}
	}
	if rl.Allow() {
		t.Fatal("should reject")
	}
}

func TestAIMD_Decrease(t *testing.T) {
	rl := NewAIMDLimiter(10, 2, 20)

	rl.OnFailure() // limit → 5
	if got := rl.CurrentLimit(); got != 5 {
		t.Fatalf("expected 5, got %d", got)
	}

	rl.OnFailure() // limit → 2 (min)
	if got := rl.CurrentLimit(); got != 2 {
		t.Fatalf("expected 2 (min), got %d", got)
	}

	rl.OnFailure() // should stay at min
	if got := rl.CurrentLimit(); got != 2 {
		t.Fatalf("expected 2 (min floor), got %d", got)
	}
}

func TestAIMD_MaxCap(t *testing.T) {
	rl := NewAIMDLimiter(9, 1, 10)

	rl.OnSuccess() // → 10
	rl.OnSuccess() // should stay at 10 (max)

	if got := rl.CurrentLimit(); got != 10 {
		t.Fatalf("expected 10 (max), got %d", got)
	}
}
