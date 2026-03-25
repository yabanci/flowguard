package benchmarks

import (
	"context"
	"testing"
	"time"

	"github.com/yabanci/flowguard"
)

func BenchmarkTokenBucketAllow(b *testing.B) {
	rl := flowguard.NewRateLimiter(1e9, 1e9) // basically unlimited
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rl.Allow()
	}
}

func BenchmarkCircuitBreakerDo(b *testing.B) {
	cb := flowguard.NewCircuitBreaker()
	ctx := context.Background()
	fn := func(ctx context.Context) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cb.Do(ctx, fn)
	}
}

func BenchmarkPolicyDo(b *testing.B) {
	rl := flowguard.NewRateLimiter(1e9, 1e9)
	cb := flowguard.NewCircuitBreaker()
	r := flowguard.NewRetry()

	p := flowguard.NewPolicy(
		flowguard.WithPolicyRateLimiter(rl),
		flowguard.WithPolicyCircuitBreaker(cb),
		flowguard.WithPolicyRetry(r),
	)

	ctx := context.Background()
	fn := func(ctx context.Context) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Do(ctx, fn)
	}
}

func BenchmarkRateLimiterParallel(b *testing.B) {
	rl := flowguard.NewRateLimiter(1e9, 1e9)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rl.Allow()
		}
	})
}

func BenchmarkCircuitBreakerParallel(b *testing.B) {
	cb := flowguard.NewCircuitBreaker()
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
	rl := flowguard.NewSlidingWindowLimiter(1e9, time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rl.Allow()
	}
}
