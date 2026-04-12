// Basic example: circuit breaker wrapping an HTTP call.
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yabanci/flowguard"
)

func main() {
	cb := flowguard.NewCircuitBreaker(
		flowguard.WithFailureThreshold(3),
		flowguard.WithOpenTimeout(10*time.Second),
		flowguard.WithSuccessThreshold(2),
	)

	ctx := context.Background()

	for i := 0; i < 10; i++ {
		err := cb.Do(ctx, func(ctx context.Context) error {
			resp, err := http.Get("http://localhost:8080/health")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 500 {
				return fmt.Errorf("server error: %d", resp.StatusCode)
			}
			return nil
		})

		fmt.Printf("call %d: state=%s err=%v\n", i+1, cb.State(), err)
		time.Sleep(500 * time.Millisecond)
	}
}
