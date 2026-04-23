package flowguard_test

import (
	"errors"
	"sync"
	"time"

	"github.com/yabanci/flowguard/observer"
)

var errBoom = errors.New("boom")

// testObserver records events for assertion across root integration tests.
type testObserver struct {
	mu           sync.Mutex
	successes    int
	failures     int
	retries      int
	stateChanges [][2]observer.State
	rateLimited  int
}

func (o *testObserver) OnSuccess(time.Duration) {
	o.mu.Lock()
	o.successes++
	o.mu.Unlock()
}

func (o *testObserver) OnFailure(error, time.Duration) {
	o.mu.Lock()
	o.failures++
	o.mu.Unlock()
}

func (o *testObserver) OnRetry(_ int, _ error) {
	o.mu.Lock()
	o.retries++
	o.mu.Unlock()
}

func (o *testObserver) OnStateChange(from, to observer.State) {
	o.mu.Lock()
	o.stateChanges = append(o.stateChanges, [2]observer.State{from, to})
	o.mu.Unlock()
}

func (o *testObserver) OnRateLimited() {
	o.mu.Lock()
	o.rateLimited++
	o.mu.Unlock()
}
