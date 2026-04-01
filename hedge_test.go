package flowguard

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestHedge_FastPrimary(t *testing.T) {
	h := NewHedge(100 * time.Millisecond)

	var calls atomic.Int32
	err := h.Do(context.Background(), func(ctx context.Context) error {
		calls.Add(1)
		return nil // fast success
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// should not have launched any hedges
	time.Sleep(200 * time.Millisecond) // wait to be sure
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 call (no hedge), got %d", got)
	}
}

func TestHedge_SlowPrimaryHedgeWins(t *testing.T) {
	h := NewHedge(50 * time.Millisecond)

	var calls atomic.Int32
	err := h.Do(context.Background(), func(ctx context.Context) error {
		n := calls.Add(1)
		if n == 1 {
			// primary is slow
			time.Sleep(500 * time.Millisecond)
			return ctx.Err() // will be cancelled
		}
		// hedge is fast
		return nil
	})

	if err != nil {
		t.Fatalf("expected success from hedge, got: %v", err)
	}
}

func TestHedge_AllFail(t *testing.T) {
	h := NewHedge(10 * time.Millisecond)

	err := h.Do(context.Background(), func(ctx context.Context) error {
		return errBoom
	})

	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got: %v", err)
	}
}

func TestHedge_ContextCancelled(t *testing.T) {
	h := NewHedge(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := h.Do(ctx, func(ctx context.Context) error {
		return ctx.Err()
	})

	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestHedge_MultipleHedges(t *testing.T) {
	h := NewHedge(10*time.Millisecond, WithMaxHedges(2))

	var calls atomic.Int32
	err := h.Do(context.Background(), func(ctx context.Context) error {
		n := calls.Add(1)
		if n <= 2 {
			// first two calls are slow
			time.Sleep(200 * time.Millisecond)
			return ctx.Err()
		}
		// third call succeeds
		return nil
	})

	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	// should have launched 3 total (1 primary + 2 hedges)
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 calls, got %d", got)
	}
}

func TestHedge_PrimaryFailsHedgeSucceeds(t *testing.T) {
	h := NewHedge(10 * time.Millisecond)

	var calls atomic.Int32
	err := h.Do(context.Background(), func(ctx context.Context) error {
		n := calls.Add(1)
		if n == 1 {
			return errBoom // primary fails immediately
		}
		return nil // hedge succeeds
	})

	if err != nil {
		t.Fatalf("expected hedge success, got: %v", err)
	}
}
