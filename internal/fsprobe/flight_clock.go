package fsprobe

import (
	"sync"
	"time"
)

// flightClock is the time source a bounded flight's bound runs on (#7456) —
// the deadline of a Stat or EvalSymlinks probe, and the per-step bound of a
// Paced judgement alike. Production uses the wall clock; a test installs a
// ManualClock so a lookup's cost is declared by the test — a mount whose
// every read answers, or one that stays out past the bound — and never
// measured against how the scheduler happened to run beside it. A flight
// loads it once, when it begins, the way every seam here loads in the
// caller.
type flightClock interface {
	Now() time.Time
	NewTimer(d time.Duration) flightTimer
}

// flightTimer is a bounded flight's deadline under its clock.
type flightTimer interface {
	C() <-chan time.Time
	Reset(d time.Duration) bool
	Stop() bool
}

// wallFlightClock is production's flightClock: the wall clock and its timers.
type wallFlightClock struct{}

func (wallFlightClock) Now() time.Time { return time.Now() }
func (wallFlightClock) NewTimer(d time.Duration) flightTimer {
	return wallFlightTimer{time.NewTimer(d)}
}

type wallFlightTimer struct{ *time.Timer }

func (t wallFlightTimer) C() <-chan time.Time { return t.Timer.C }

// flightClocks is the seam under a flight's clock, the wall clock in
// production.
var flightClocks = newSeam[flightClock](wallFlightClock{})

// ManualClock is the test flightClock SetFlightClockForTest installs: its
// time passes only when Advance moves it, and a timer fires when Advance
// carries now to or past its deadline. A stand-in for a slow mount declares
// each syscall's cost by advancing the clock, and a syscall that does not
// answer by advancing a whole bound and then holding, so the verdict a
// flight reaches is a fact the test set rather than a race it won. A probe
// whose work answers without ever stepping — a bounded Stat — answers
// whenever its work finishes, since nothing advances the clock while it
// runs.
type ManualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*manualFlightTimer]struct{}
}

func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{now: start, timers: map[*manualFlightTimer]struct{}{}}
}

func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *ManualClock) Since(t time.Time) time.Duration {
	return c.Now().Sub(t)
}

// Advance moves the clock d forward and fires every timer whose deadline the
// move reaches.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for timer := range c.timers {
		timer.fireDue()
	}
}

func (c *ManualClock) NewTimer(d time.Duration) flightTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &manualFlightTimer{clock: c, ch: make(chan time.Time, 1)}
	c.timers[timer] = struct{}{}
	timer.reset(d)
	return timer
}

type manualFlightTimer struct {
	clock    *ManualClock
	ch       chan time.Time
	deadline time.Time
	active   bool
}

func (t *manualFlightTimer) C() <-chan time.Time { return t.ch }

// reset arms the timer d past the clock's now, firing at once when that
// deadline is already reached. Callers hold the clock's lock.
func (t *manualFlightTimer) reset(d time.Duration) {
	t.deadline = t.clock.now.Add(d)
	t.active = true
	t.fireDue()
}

func (t *manualFlightTimer) fireDue() {
	if t.active && !t.deadline.After(t.clock.now) {
		t.active = false
		select {
		case t.ch <- t.clock.now:
		default:
		}
	}
}

func (t *manualFlightTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	// Drop a fired value still in the channel, as a real Reset requires its
	// caller to have drained.
	select {
	case <-t.ch:
	default:
	}
	t.reset(d)
	return true
}

func (t *manualFlightTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.active = false
	delete(t.clock.timers, t)
	return was
}
