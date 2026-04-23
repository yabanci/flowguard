// Load shedding example: adaptive concurrency limiting for server-side protection.
package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/yabanci/flowguard/loadshed"
)

func main() {
	ls := loadshed.New(
		20,                  // initial concurrency limit
		50*time.Millisecond, // latency threshold
		loadshed.WithLimits(5, 100),
	)

	// simulate bursts of traffic
	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		shed := 0
		ok := 0

		// send 30 concurrent requests
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := ls.Do(context.Background(), func(ctx context.Context) error {
					// simulate work — sometimes slow
					time.Sleep(time.Duration(10+rand.Intn(80)) * time.Millisecond)
					return nil
				})
				if err != nil {
					shed++
				} else {
					ok++
				}
			}()
		}
		wg.Wait()

		fmt.Printf("round %d: ok=%d shed=%d limit=%d\n",
			round, ok, shed, ls.CurrentLimit())
		time.Sleep(100 * time.Millisecond)
	}
}
