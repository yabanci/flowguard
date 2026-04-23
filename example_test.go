package flowguard_test

import (
	"context"
	"fmt"
	"time"

	"github.com/yabanci/flowguard"
	"github.com/yabanci/flowguard/bulkhead"
	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/ratelimit"
	"github.com/yabanci/flowguard/retry"
)

func ExampleNewPolicy() {
	policy := flowguard.NewPolicy(
		flowguard.WithRateLimiter(ratelimit.NewTokenBucket(100, 10)),
		flowguard.WithCircuitBreaker(circuitbreaker.New()),
		flowguard.WithRetry(retry.New(
			retry.WithMaxRetries(2),
			retry.WithConstantBackoff(time.Millisecond),
		)),
		flowguard.WithFallback(func(ctx context.Context, err error) error {
			fmt.Println("fallback called")
			return nil
		}),
	)

	err := policy.Do(context.Background(), func(ctx context.Context) error {
		return nil
	})
	fmt.Println("err:", err)
	// Output:
	// err: <nil>
}

func Example_tokenBucket() {
	rl := ratelimit.NewTokenBucket(10, 3)

	for i := 0; i < 5; i++ {
		if rl.Allow() {
			fmt.Printf("request %d: allowed\n", i)
		} else {
			fmt.Printf("request %d: rejected\n", i)
		}
	}
	// Output:
	// request 0: allowed
	// request 1: allowed
	// request 2: allowed
	// request 3: rejected
	// request 4: rejected
}

func Example_circuitBreaker() {
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithOpenTimeout(time.Second),
	)

	ctx := context.Background()

	_ = cb.Do(ctx, func(ctx context.Context) error { return fmt.Errorf("fail") })
	_ = cb.Do(ctx, func(ctx context.Context) error { return fmt.Errorf("fail") })

	fmt.Println("state:", cb.State())

	err := cb.Do(ctx, func(ctx context.Context) error { return nil })
	fmt.Println("error:", err)

	// Output:
	// state: open
	// error: flowguard/circuitbreaker: open
}

func Example_retry() {
	r := retry.New(
		retry.WithMaxRetries(3),
		retry.WithConstantBackoff(time.Millisecond),
		retry.WithJitter(0),
	)

	attempt := 0
	err := r.Do(context.Background(), func(ctx context.Context) error {
		attempt++
		if attempt < 3 {
			return fmt.Errorf("transient")
		}
		return nil
	})

	fmt.Println("succeeded after", attempt, "attempts, err:", err)
	// Output:
	// succeeded after 3 attempts, err: <nil>
}

func Example_bulkhead() {
	b := bulkhead.New(2, bulkhead.WithMaxWait(0))

	for i := 0; i < 3; i++ {
		err := b.Do(context.Background(), func(ctx context.Context) error {
			return nil
		})
		if err != nil {
			fmt.Println("rejected:", err)
		} else {
			fmt.Println("allowed")
		}
	}
	// Output:
	// allowed
	// allowed
	// allowed
}

func Example_aimd() {
	rl := ratelimit.NewAIMD(5, 1, 20)

	fmt.Println("initial:", rl.CurrentLimit())

	rl.OnSuccess()
	rl.OnSuccess()
	fmt.Println("after 2 successes:", rl.CurrentLimit())

	rl.OnFailure()
	fmt.Println("after failure:", rl.CurrentLimit())

	// Output:
	// initial: 5
	// after 2 successes: 7
	// after failure: 3
}

func Example_adaptiveCircuitBreaker() {
	cb := circuitbreaker.NewAdaptive(10, 0.5, 5)

	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_ = cb.Do(ctx, func(ctx context.Context) error { return nil })
	}
	for i := 0; i < 2; i++ {
		_ = cb.Do(ctx, func(ctx context.Context) error { return fmt.Errorf("fail") })
	}

	fmt.Printf("error rate: %.0f%%\n", cb.ErrorRate()*100)
	fmt.Println("state:", cb.State())

	// Output:
	// error rate: 40%
	// state: closed
}
