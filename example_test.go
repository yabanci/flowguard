package flowguard_test

import (
	"context"
	"fmt"
	"time"

	"github.com/yabanci/flowguard"
)

func ExampleNewRateLimiter() {
	rl := flowguard.NewRateLimiter(10, 3) // 10/s, burst 3

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

func ExampleNewCircuitBreaker() {
	cb := flowguard.NewCircuitBreaker(
		flowguard.WithFailureThreshold(2),
		flowguard.WithOpenTimeout(time.Second),
	)

	ctx := context.Background()

	// two failures trip the breaker
	cb.Do(ctx, func(ctx context.Context) error { return fmt.Errorf("fail") })
	cb.Do(ctx, func(ctx context.Context) error { return fmt.Errorf("fail") })

	fmt.Println("state:", cb.State())

	// calls are now rejected
	err := cb.Do(ctx, func(ctx context.Context) error { return nil })
	fmt.Println("error:", err)

	// Output:
	// state: open
	// error: flowguard: circuit breaker is open
}

func ExampleNewRetry() {
	r := flowguard.NewRetry(
		flowguard.WithMaxRetries(3),
		flowguard.WithConstantBackoff(time.Millisecond),
		flowguard.WithJitter(0),
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

func ExampleNewPolicy() {
	policy := flowguard.NewPolicy(
		flowguard.WithPolicyRateLimiter(flowguard.NewRateLimiter(100, 10)),
		flowguard.WithPolicyCircuitBreaker(flowguard.NewCircuitBreaker()),
		flowguard.WithPolicyRetry(flowguard.NewRetry(
			flowguard.WithMaxRetries(2),
			flowguard.WithConstantBackoff(time.Millisecond),
		)),
		flowguard.WithPolicyFallback(func(ctx context.Context, err error) error {
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

func ExampleNewBulkhead() {
	b := flowguard.NewBulkhead(2, flowguard.WithMaxWaitDuration(0))

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

func ExampleNewAIMDLimiter() {
	rl := flowguard.NewAIMDLimiter(5, 1, 20)

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

func ExampleNewAdaptiveCircuitBreaker() {
	cb := flowguard.NewAdaptiveCircuitBreaker(
		10,  // window size
		0.5, // error threshold (50%)
		5,   // min samples before checking
	)

	ctx := context.Background()

	// 3 successes + 2 failures = 40% error rate (below 50%)
	for i := 0; i < 3; i++ {
		cb.Do(ctx, func(ctx context.Context) error { return nil })
	}
	for i := 0; i < 2; i++ {
		cb.Do(ctx, func(ctx context.Context) error { return fmt.Errorf("fail") })
	}

	fmt.Printf("error rate: %.0f%%\n", cb.ErrorRate()*100)
	fmt.Println("state:", cb.State())

	// Output:
	// error rate: 40%
	// state: closed
}
