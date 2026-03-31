package flowguard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBulkhead_Basic(t *testing.T) {
	b := NewBulkhead(2)
	ctx := context.Background()

	err := b.Do(ctx, func(ctx context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBulkhead_ConcurrencyLimit(t *testing.T) {
	b := NewBulkhead(2, WithMaxWaitDuration(0))
	ctx := context.Background()

	gate := make(chan struct{})
	var running atomic.Int32

	// fill both slots
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Do(ctx, func(ctx context.Context) error {
				running.Add(1)
				<-gate
				return nil
			})
		}()
	}

	// wait for both goroutines to be inside fn
	for running.Load() < 2 {
		time.Sleep(time.Millisecond)
	}

	// third call should be rejected immediately
	err := b.Do(ctx, func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrBulkheadFull) {
		t.Fatalf("expected ErrBulkheadFull, got %v", err)
	}

	close(gate)
	wg.Wait()
}

func TestBulkhead_WaitForSlot(t *testing.T) {
	b := NewBulkhead(1, WithMaxWaitDuration(500*time.Millisecond))
	ctx := context.Background()

	gate := make(chan struct{})

	// occupy the slot
	go b.Do(ctx, func(ctx context.Context) error {
		<-gate
		return nil
	})
	time.Sleep(5 * time.Millisecond) // let it acquire

	// this one should wait and then succeed when slot is freed
	done := make(chan error, 1)
	go func() {
		done <- b.Do(ctx, func(ctx context.Context) error { return nil })
	}()

	// free the slot
	time.Sleep(10 * time.Millisecond)
	close(gate)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected success after wait, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bulkhead")
	}
}

func TestBulkhead_WaitTimeout(t *testing.T) {
	b := NewBulkhead(1, WithMaxWaitDuration(10*time.Millisecond))
	ctx := context.Background()

	gate := make(chan struct{})
	defer close(gate)

	// occupy the slot
	go b.Do(ctx, func(ctx context.Context) error {
		<-gate
		return nil
	})
	time.Sleep(5 * time.Millisecond)

	// this should time out
	err := b.Do(ctx, func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrBulkheadFull) {
		t.Fatalf("expected ErrBulkheadFull after timeout, got %v", err)
	}
}

func TestBulkhead_ContextCancelled(t *testing.T) {
	b := NewBulkhead(1, WithMaxWaitDuration(5*time.Second))

	gate := make(chan struct{})
	defer close(gate)

	ctx := context.Background()
	go b.Do(ctx, func(ctx context.Context) error {
		<-gate
		return nil
	})
	time.Sleep(5 * time.Millisecond)

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	err := b.Do(cancelCtx, func(ctx context.Context) error { return nil })
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestBulkhead_ActiveCount(t *testing.T) {
	b := NewBulkhead(5)
	ctx := context.Background()

	if b.ActiveCount() != 0 {
		t.Fatal("should start at 0")
	}

	gate := make(chan struct{})
	for i := 0; i < 3; i++ {
		go b.Do(ctx, func(ctx context.Context) error {
			<-gate
			return nil
		})
	}
	time.Sleep(10 * time.Millisecond)

	if got := b.ActiveCount(); got != 3 {
		t.Fatalf("expected 3 active, got %d", got)
	}

	close(gate)
	time.Sleep(10 * time.Millisecond)

	if got := b.ActiveCount(); got != 0 {
		t.Fatalf("expected 0 after done, got %d", got)
	}
}
