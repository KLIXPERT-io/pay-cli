package payloadtest

import (
	"sync"
	"time"
)

// Epoch is the instant every test clock starts at. A fixed, obviously
// synthetic timestamp keeps golden files stable and makes an accidental
// time.Now() leak visible at a glance.
var Epoch = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// Clock is a deterministic, monotonically advancing clock.
//
// Every call to Now advances it by Step, so a command that measures its own
// duration produces the same number on every run instead of a flaky 0-3 ms.
type Clock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

// NewClock returns a clock at Epoch advancing by step on each read. A zero step
// makes the clock frozen.
func NewClock(step time.Duration) *Clock {
	return &Clock{now: Epoch, step: step}
}

// Frozen is the clock every golden test uses: it never moves, so meta.duration_ms
// is always 0.
func Frozen() *Clock { return NewClock(0) }

// Now returns the current instant and advances the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now
	c.now = c.now.Add(c.step)
	return t
}

// Peek returns the current instant without advancing.
func (c *Clock) Peek() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set pins the clock to an absolute instant.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Func is the injectable form App.Now takes.
func (c *Clock) Func() func() time.Time { return c.Now }
