// Hedged requests example: fire a backup request if the primary is slow.
// Dramatically reduces tail latency for idempotent operations.
package main

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/yabanci/flowguard/hedge"
)

func main() {
	// hedge after 50ms — roughly our P95 latency
	h := hedge.New(50 * time.Millisecond)

	for i := 0; i < 10; i++ {
		start := time.Now()
		err := h.Do(context.Background(), func(ctx context.Context) error {
			// simulate variable latency — sometimes fast, sometimes slow
			latency := time.Duration(10+rand.Intn(200)) * time.Millisecond
			select {
			case <-time.After(latency):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})

		elapsed := time.Since(start)
		if err != nil {
			fmt.Printf("[%d] error: %v (%v)\n", i, err, elapsed)
		} else {
			fmt.Printf("[%d] ok (%v)\n", i, elapsed.Round(time.Millisecond))
		}
	}
}
