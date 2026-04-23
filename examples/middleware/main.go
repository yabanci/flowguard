// HTTP middleware example: protect an HTTP server with flowguard.
package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/yabanci/flowguard"
	"github.com/yabanci/flowguard/loadshed"
	"github.com/yabanci/flowguard/middleware"
	"github.com/yabanci/flowguard/ratelimit"
)

func main() {
	// server-side policy: rate limit + load shed
	rl := ratelimit.NewTokenBucket(100, 200) // 100 req/s, burst 200
	ls := loadshed.New(50, 100*time.Millisecond)

	// wrap load shedder in a policy (it's not a Policy component, so we use
	// a separate middleware layer — TODO: maybe add to Policy later)
	_ = ls

	policy := flowguard.NewPolicy(
		flowguard.WithRateLimiter(rl),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		// simulate some work
		time.Sleep(10 * time.Millisecond)
		fmt.Fprintf(w, `{"status": "ok"}`)
	})

	protected := middleware.HTTPServer(policy)(mux)

	fmt.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", protected))
}
