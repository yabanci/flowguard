package flowguard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRetry_SuccessNoRetry(t *testing.T) {
	clk := newMockClock(time.Now())
	r := NewRetry(WithRetryClock(clk))

	calls := 0
	err := r.Do(context.Background(), func(ctx context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestRetry_SuccessAfterFailures(t *testing.T) {
	clk := newMockClock(time.Now())
	r := NewRetry(WithMaxRetries(3), WithRetryClock(clk))

	calls := 0
	err := r.Do(context.Background(), func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return errBoom
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestRetry_ExhaustedRetries(t *testing.T) {
	clk := newMockClock(time.Now())
	r := NewRetry(WithMaxRetries(2), WithRetryClock(clk))

	err := r.Do(context.Background(), func(ctx context.Context) error {
		return errBoom
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "after 2 retries") {
		t.Fatalf("unexpected error message: %v", err)
	}
	// should wrap the original error
	if !errors.Is(err, errBoom) {
		t.Fatal("expected wrapped errBoom")
	}
}

func TestRetry_BackoffGrows(t *testing.T) {
	clk := newMockClock(time.Now())
	r := NewRetry(
		WithMaxRetries(3),
		WithExponentialBackoff(100*time.Millisecond),
		WithJitter(0), // disable jitter for predictability
		WithRetryClock(clk),
	)

	start := clk.Now()
	r.Do(context.Background(), func(ctx context.Context) error {
		return errBoom
	})
	elapsed := clk.Now().Sub(start)

	// 100ms + 200ms + 400ms = 700ms total sleep
	expected := 700 * time.Millisecond
	if elapsed != expected {
		t.Fatalf("expected %v total backoff, got %v", expected, elapsed)
	}
}

func TestRetry_ConstantBackoff(t *testing.T) {
	clk := newMockClock(time.Now())
	r := NewRetry(
		WithMaxRetries(3),
		WithConstantBackoff(50*time.Millisecond),
		WithJitter(0),
		WithRetryClock(clk),
	)

	start := clk.Now()
	r.Do(context.Background(), func(ctx context.Context) error {
		return errBoom
	})
	elapsed := clk.Now().Sub(start)

	// 50ms * 3 = 150ms
	if elapsed != 150*time.Millisecond {
		t.Fatalf("expected 150ms, got %v", elapsed)
	}
}

func TestRetry_RetryIf(t *testing.T) {
	clk := newMockClock(time.Now())

	retryable := errors.New("retryable")
	permanent := errors.New("permanent")

	r := NewRetry(
		WithMaxRetries(5),
		WithRetryClock(clk),
		WithRetryIf(func(err error) bool {
			return errors.Is(err, retryable)
		}),
	)

	calls := 0
	err := r.Do(context.Background(), func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return retryable
		}
		return permanent // should not be retried
	})

	if !errors.Is(err, permanent) {
		t.Fatalf("expected permanent error, got: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (1 retry + stop on permanent), got %d", calls)
	}
}

func TestRetry_ContextCancelled(t *testing.T) {
	clk := newMockClock(time.Now())
	r := NewRetry(WithMaxRetries(10), WithRetryClock(clk))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := r.Do(ctx, func(ctx context.Context) error {
		return errBoom
	})
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRetry_ContextCancelledError(t *testing.T) {
	// if fn returns context.Canceled, don't retry it
	clk := newMockClock(time.Now())
	r := NewRetry(WithMaxRetries(5), WithRetryClock(clk))

	calls := 0
	err := r.Do(context.Background(), func(ctx context.Context) error {
		calls++
		return context.Canceled
	})
	if calls != 1 {
		t.Fatalf("should not have retried, got %d calls", calls)
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRetry_MaxBackoffCap(t *testing.T) {
	clk := newMockClock(time.Now())
	r := NewRetry(
		WithMaxRetries(5),
		WithExponentialBackoff(1*time.Second),
		WithMaxBackoff(3*time.Second),
		WithJitter(0),
		WithRetryClock(clk),
	)

	start := clk.Now()
	r.Do(context.Background(), func(ctx context.Context) error {
		return errBoom
	})
	elapsed := clk.Now().Sub(start)

	// delays: 1s, 2s, 3s(capped), 3s(capped), 3s(capped) = 12s
	if elapsed != 12*time.Second {
		t.Fatalf("expected 12s, got %v", elapsed)
	}
}
