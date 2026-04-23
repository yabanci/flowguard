package benchmarks

import (
	"context"
	"testing"
	"time"

	"github.com/yabanci/flowguard"
	"github.com/yabanci/flowguard/bulkhead"
	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/loadshed"
	"github.com/yabanci/flowguard/ratelimit"
	"github.com/yabanci/flowguard/retry"
)

func BenchmarkTokenBucketAllow(b *testing.B) {
	rl := ratelimit.NewTokenBucket(1e9, 1e9) // basically unlimited
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rl.Allow()
	}
}

func BenchmarkCircuitBreakerDo(b *testing.B) {
	cb := circuitbreaker.New()
	ctx := context.Background()
	fn := func(ctx context.Context) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cb.Do(ctx, fn)
	}
}

func BenchmarkPolicyDo(b *testing.B) {
	rl := ratelimit.NewTokenBucket(1e9, 1e9)
	cb := circuitbreaker.New()
	r := retry.New()

	p := flowguard.NewPolicy(
		flowguard.WithRateLimiter(rl),
		flowguard.WithCircuitBreaker(cb),
		flowguard.WithRetry(r),
	)

	ctx := context.Background()
	fn := func(ctx context.Context) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Do(ctx, fn)
	}
}

func BenchmarkRateLimiterParallel(b *testing.B) {
	rl := ratelimit.NewTokenBucket(1e9, 1e9)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rl.Allow()
		}
	})
}

func BenchmarkCircuitBreakerParallel(b *testing.B) {
	cb := circuitbreaker.New()
	ctx := context.Background()
	fn := func(ctx context.Context) error { return nil }

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			cb.Do(ctx, fn)
		}
	})
}

// just to get a baseline for how fast a function call + context is
func BenchmarkBaseline(b *testing.B) {
	ctx := context.Background()
	fn := func(ctx context.Context) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fn(ctx)
	}
}

func BenchmarkSlidingWindowAllow(b *testing.B) {
	rl := ratelimit.NewSlidingWindow(1e9, time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rl.Allow()
	}
}

func BenchmarkBulkheadDo(b *testing.B) {
	bh := bulkhead.New(1e6)
	ctx := context.Background()
	fn := func(ctx context.Context) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bh.Do(ctx, fn)
	}
}

func BenchmarkLoadShedderDo(b *testing.B) {
	ls := loadshed.New(1e6, time.Hour) // won't trigger decrease
	ctx := context.Background()
	fn := func(ctx context.Context) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ls.Do(ctx, fn)
	}
}

func BenchmarkAdaptiveCBDo(b *testing.B) {
	cb := circuitbreaker.NewAdaptive(1000, 0.9, 100)
	ctx := context.Background()
	fn := func(ctx context.Context) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cb.Do(ctx, fn)
	}
}
