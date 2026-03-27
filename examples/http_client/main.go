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
)

func main() {
	rl := flowguard.NewRateLimiter(5, 10) // 5 req/s, burst of 10
	cb := flowguard.NewCircuitBreaker(
		flowguard.WithFailureThreshold(3),
		flowguard.WithOpenTimeout(15*time.Second),
	)
	retry := flowguard.NewRetry(
		flowguard.WithMaxRetries(3),
		flowguard.WithExponentialBackoff(200*time.Millisecond),
		flowguard.WithMaxBackoff(5*time.Second),
	)

	policy := flowguard.NewPolicy(
		flowguard.WithPolicyRateLimiter(rl),
		flowguard.WithPolicyCircuitBreaker(cb),
		flowguard.WithPolicyRetry(retry),
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
