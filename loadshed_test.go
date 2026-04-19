package flowguard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadShedder_Basic(t *testing.T) {
	ls := NewLoadShedder(10, 100*time.Millisecond)

	err := ls.Do(context.Background(), func(ctx context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadShedder_ShedsAtLimit(t *testing.T) {
	ls := NewLoadShedder(3, time.Second)
	ctx := context.Background()

	gate := make(chan struct{})
	var running atomic.Int32

	// fill all 3 slots
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ls.Do(ctx, func(ctx context.Context) error {
				running.Add(1)
				<-gate
				return nil
			})
		}()
	}

	// wait for all to be in-flight
	for running.Load() < 3 {
		time.Sleep(time.Millisecond)
	}

	// 4th request should be shed
	err := ls.Do(ctx, func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrLoadShed) {
		t.Fatalf("expected ErrLoadShed, got %v", err)
	}

	close(gate)
	wg.Wait()
}

func TestLoadShedder_DecreasesOnSlowCalls(t *testing.T) {
	ls := NewLoadShedder(20, 10*time.Millisecond,
		WithLoadShedLimits(2, 100),
	)

	ctx := context.Background()

	// make a slow call to trigger decrease
	ls.Do(ctx, func(ctx context.Context) error {
		time.Sleep(50 * time.Millisecond) // over the 10ms threshold
		return nil
	})

	// limit should have halved: 20 → 10
	if got := ls.CurrentLimit(); got != 10 {
		t.Fatalf("expected limit 10 after slow call, got %d", got)
	}
}

func TestLoadShedder_IncreasesOnSuccess(t *testing.T) {
	ls := NewLoadShedder(10, time.Second)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		ls.Do(ctx, func(ctx context.Context) error { return nil })
	}

	// limit should have gone up by 5
	if got := ls.CurrentLimit(); got != 15 {
		t.Fatalf("expected limit 15, got %d", got)
	}
}

func TestLoadShedder_DecreasesOnError(t *testing.T) {
	ls := NewLoadShedder(20, time.Second,
		WithLoadShedLimits(5, 100),
	)
	ctx := context.Background()

	ls.Do(ctx, func(ctx context.Context) error { return errBoom })

	if got := ls.CurrentLimit(); got != 10 {
		t.Fatalf("expected limit 10 after error, got %d", got)
	}
}

func TestLoadShedder_RespectsMinMax(t *testing.T) {
	ls := NewLoadShedder(6, time.Second,
		WithLoadShedLimits(5, 8),
	)
	ctx := context.Background()

	// decrease past min
	ls.Do(ctx, func(ctx context.Context) error { return errBoom }) // 6 → 3, clamped to 5
	if got := ls.CurrentLimit(); got != 5 {
		t.Fatalf("expected min 5, got %d", got)
	}

	// increase past max
	ls2 := NewLoadShedder(7, time.Second,
		WithLoadShedLimits(1, 8),
	)
	for i := 0; i < 5; i++ {
		ls2.Do(ctx, func(ctx context.Context) error { return nil })
	}
	if got := ls2.CurrentLimit(); got != 8 {
		t.Fatalf("expected max 8, got %d", got)
	}
}

func TestLoadShedder_Inflight(t *testing.T) {
	ls := NewLoadShedder(10, time.Second)
	ctx := context.Background()

	if ls.Inflight() != 0 {
		t.Fatal("should start at 0")
	}

	gate := make(chan struct{})
	go ls.Do(ctx, func(ctx context.Context) error {
		<-gate
		return nil
	})
	time.Sleep(5 * time.Millisecond)

	if ls.Inflight() != 1 {
		t.Fatalf("expected 1 inflight, got %d", ls.Inflight())
	}

	close(gate)
	time.Sleep(5 * time.Millisecond)

	if ls.Inflight() != 0 {
		t.Fatalf("expected 0 after done, got %d", ls.Inflight())
	}
}
