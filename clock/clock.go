// Package clock abstracts time so flowguard primitives can be tested
// without real sleeps. Production code uses Real(); tests inject a
// fake via internal/fakeclock.
package clock

import "time"

// Clock abstracts time so we can test without real sleeps.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// Real returns a Clock backed by the standard library.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time        { return time.Now() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }
