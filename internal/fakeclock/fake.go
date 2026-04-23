// Package fakeclock provides a controllable clock for tests. Lives in
// internal/ so only flowguard's own tests can import it — external users
// should implement their own clock.Clock if they need fakes.
package fakeclock

import (
	"sync"
	"time"
)

// Clock is a clock.Clock whose time only advances when the test calls
// Sleep or Advance. It is safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// New returns a fake clock anchored at t.
func New(t time.Time) *Clock { return &Clock{now: t} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Advance moves the clock forward by d without the Sleep semantics.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
