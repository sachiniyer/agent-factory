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
// A timer takes an absolute deadline on its clock, never a duration: the
// deadline a flight is bounded by — its start plus the bound, or its last
// step plus the bound — is a fixed instant, so arming it can never overshoot
// no matter how the work's advances interleave with the arm. A timer whose
// deadline the clock has already passed fires at once.
type flightClock interface {
	Now() time.Time
	NewTimer(deadline time.Time) flightTimer
}

// flightTimer is a bounded flight's deadline under its clock.
type flightTimer interface {
	C() <-chan time.Time
	Reset(deadline time.Time) bool
	Stop() bool
}

// wallFlightClock is production's flightClock: the wall clock and its timers.
type wallFlightClock struct{}

func (wallFlightClock) Now() time.Time { return time.Now() }
func (wallFlightClock) NewTimer(deadline time.Time) flightTimer {
	return wallFlightTimer{time.NewTimer(time.Until(deadline))}
}

type wallFlightTimer struct{ *time.Timer }

func (t wallFlightTimer) C() <-chan time.Time { return t.Timer.C }
func (t wallFlightTimer) Reset(deadline time.Time) bool {
	return t.Timer.Reset(time.Until(deadline))
}

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

func (c *ManualClock) NewTimer(deadline time.Time) flightTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &manualFlightTimer{clock: c, ch: make(chan time.Time, 1)}
	c.timers[timer] = struct{}{}
	timer.reset(deadline)
	return timer
}

type manualFlightTimer struct {
	clock    *ManualClock
	ch       chan time.Time
	deadline time.Time
	active   bool
}

func (t *manualFlightTimer) C() <-chan time.Time { return t.ch }

// reset arms the timer for the deadline, firing at once when the clock has
// already reached it — arming can never overshoot a clock that has moved
// past the deadline before the timer registered (#7456). Callers hold the
// clock's lock.
func (t *manualFlightTimer) reset(deadline time.Time) {
	t.deadline = deadline
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

func (t *manualFlightTimer) Reset(deadline time.Time) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	// Drop a fired value still in the channel, as a real Reset requires its
	// caller to have drained.
	select {
	case <-t.ch:
	default:
	}
	t.reset(deadline)
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
