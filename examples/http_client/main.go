// Full stack example: Policy with rate limiter + circuit breaker + retry
// wrapping an HTTP client.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yabanci/flowguard"
	"github.com/yabanci/flowguard/circuitbreaker"
	"github.com/yabanci/flowguard/ratelimit"
	"github.com/yabanci/flowguard/retry"
)

func main() {
	rl := ratelimit.NewTokenBucket(5, 10) // 5 req/s, burst of 10
	cb := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(3),
		circuitbreaker.WithOpenTimeout(15*time.Second),
	)
	retry := retry.New(
		retry.WithMaxRetries(3),
		retry.WithExponentialBackoff(200*time.Millisecond),
		retry.WithMaxBackoff(5*time.Second),
	)

	policy := flowguard.NewPolicy(
		flowguard.WithRateLimiter(rl),
		flowguard.WithCircuitBreaker(cb),
		flowguard.WithRetry(retry),
	)

	ctx := context.Background()

	// simulate a bunch of requests
	for i := 0; i < 20; i++ {
		var body string
		err := policy.Do(ctx, func(ctx context.Context) error {
			req, err := http.NewRequestWithContext(ctx, "GET", "https://httpbin.org/get", nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode >= 500 {
				return fmt.Errorf("server error: %d", resp.StatusCode)
			}
			b, _ := io.ReadAll(resp.Body)
			body = string(b[:min(len(b), 80)]) // just first 80 chars
			return nil
		})

		if err != nil {
			fmt.Printf("[%d] ERROR: %v (cb=%s)\n", i, err, cb.State())
		} else {
			fmt.Printf("[%d] OK: %s...\n", i, body)
		}
	}
}
