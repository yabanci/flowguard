// Adaptive rate limiting with AIMD — simulates a server that starts
// returning 429 under load, and the limiter backs off automatically.
package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/yabanci/flowguard"
)

func main() {
	rl := flowguard.NewAIMDLimiter(
		10, // initial limit
		2,  // min
		50, // max
	)

	// fake server: starts rejecting at ~15 concurrent requests
	var inflight atomic.Int32

	callServer := func(ctx context.Context) error {
		n := inflight.Add(1)
		defer inflight.Add(-1)

		// simulate latency
		time.Sleep(time.Duration(10+rand.Intn(20)) * time.Millisecond)

		// server overloaded?
		if n > 15 {
			return fmt.Errorf("429 too many requests")
		}
		return nil
	}

	ctx := context.Background()

	// run for a few seconds, printing the current limit
	for i := 0; i < 100; i++ {
		if !rl.Allow() {
			// we're at the limit, signal failure
			rl.OnFailure()
			fmt.Printf("tick %3d: rate limited (limit=%d)\n", i, rl.CurrentLimit())
			time.Sleep(50 * time.Millisecond)
			continue
		}

		err := callServer(ctx)
		if err != nil {
			rl.OnFailure()
			fmt.Printf("tick %3d: %v (limit=%d)\n", i, err, rl.CurrentLimit())
		} else {
			rl.OnSuccess()
			fmt.Printf("tick %3d: ok (limit=%d)\n", i, rl.CurrentLimit())
		}

		time.Sleep(20 * time.Millisecond)
	}

	fmt.Printf("\nfinal limit: %d\n", rl.CurrentLimit())
}
