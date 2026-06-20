// Package sys holds the injectable environment seams that doc depends on but
// that must be controllable in tests: the wall clock and the identifier
// generator. Spec 2061 doc 19 calls for a Clock and an IDGenerator interface so
// that TTL expiry, ObjectId minting, and any timestamp-bearing record can be
// driven deterministically by a test rather than by real time and real
// randomness. Production wiring uses the system implementations; tests inject
// manual ones.
package sys

import (
	"sync"
	"time"
)

// Clock is the seam over the wall clock. Every component that needs the current
// time takes a Clock rather than calling time.Now directly, so that a test can
// freeze or advance time at will. Implementations must be safe for concurrent
// use.
type Clock interface {
	// Now returns the current time. The returned time should be in UTC for
	// on-disk timestamps to be location-independent.
	Now() time.Time
}

// SystemClock is the production Clock backed by time.Now. The zero value is
// ready to use.
type SystemClock struct{}

// Now returns time.Now in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// ManualClock is a Clock whose time only advances when a test tells it to. It is
// safe for concurrent use. The zero value starts at the Unix epoch; use
// NewManualClock to start at a chosen instant.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock returns a ManualClock pinned at start.
func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{now: start.UTC()}
}

// Now returns the clock's current frozen time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d and returns the new time.
func (c *ManualClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// Set pins the clock at t.
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}
