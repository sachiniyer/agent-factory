package fsprobe

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// pacedFlight is one Paced run: a probe flight whose bound restarts at every
// step its work reports.
type pacedFlight struct {
	done     chan struct{}
	res      any
	err      error
	timedOut bool // guarded by pacedFlights
	// lastStep is when work last reported a step, as the offset since the
	// flight began on its clock. An atomic can hold the offset but not the
	// instant, and a time.Unix rebuild of a stored stamp would carry no
	// monotonic reading — the bound would run on the wall clock, where a
	// step back during a stall lengthens the wait by the step (#7403).
	// Rebuilt against started, the instant keeps whatever monotonic reading
	// the clock has.
	lastStep atomic.Int64
	// clock is the time source the bound runs on, and started when the
	// flight began on it — both fixed when the flight begins (#7456). The
	// flight's start is its first step, which lastStep's zero value already
	// records.
	clock   flightClock
	started time.Time
}

// pacedFlights is Paced's registry, keyed by question and key.
var pacedFlights = struct {
	sync.Mutex
	byKey map[string]*pacedFlight
}{byKey: map[string]*pacedFlight{}}

// Paced runs work in one flight whose bound is per step, not per judgement
// (#7383). work calls step before each syscall it makes, and the caller gives
// up only when a whole bound passes with neither the next step nor the
// answer. A judgement that makes many syscalls, such as a prefix compare of
// two large files read in chunks, therefore reaches its answer whatever its
// total time, as long as each syscall answers within the bound. A syscall
// that does not answer gives the ErrProbeTimedOut + context.DeadlineExceeded
// non-answer every probe here gives, and a retry while the stuck one is still
// running is refused the same way.
//
// work owns everything it opens for its whole run, so a file it opened stays
// open across the steps that read it; it must close all of it before it
// returns, since an abandoned flight returns after its caller has gone. stat
// is the regularFileStat seam loaded before the flight's goroutine starts, as
// openPath's is loaded in ReadFileWith's caller (#7539): a flight that
// outlives its test stats files with the seam it began with. As
// with Bounded, question and key name what work decides and about what, only
// a caller asking the same question of the same key joins a flight, and an
// answer of a type other than T is an error, never a panic.
func Paced[T any](question, key string, work func(step func(), stat RegularStatFunc) (T, error)) (T, error) {
	var zero T
	got, err := runPaced(question+"\x00"+key, question, key, func(step func(), stat RegularStatFunc) (any, error) {
		return work(step, stat)
	})
	if err != nil {
		return zero, err
	}
	answer, ok := got.(T)
	if !ok {
		return zero, fmt.Errorf("%s of %s answered with %T, not %T", question, key, got, zero)
	}
	return answer, nil
}

func runPaced(id, question, key string, work func(step func(), stat RegularStatFunc) (any, error)) (any, error) {
	pacedFlights.Lock()
	if active := pacedFlights.byKey[id]; active != nil {
		if active.timedOut {
			pacedFlights.Unlock()
			return nil, fmt.Errorf("%s of %s is still running after an earlier deadline: %w: %w",
				question, key, ErrProbeTimedOut, context.DeadlineExceeded)
		}
		pacedFlights.Unlock()
		return waitPaced(id, question, key, active)
	}
	flight := &pacedFlight{done: make(chan struct{}), clock: flightClocks.load()}
	stat := regularFileStat.load()
	flight.started = flight.clock.Now()
	pacedFlights.byKey[id] = flight
	pacedFlights.Unlock()
	go func() {
		res, err := work(func() {
			flight.lastStep.Store(int64(flight.clock.Now().Sub(flight.started)))
		}, stat)
		pacedFlights.Lock()
		flight.res, flight.err = res, err
		if pacedFlights.byKey[id] == flight {
			delete(pacedFlights.byKey, id)
		}
		close(flight.done)
		pacedFlights.Unlock()
	}()
	return waitPaced(id, question, key, flight)
}

// waitPaced waits for flight's answer, giving up once a whole bound has
// passed since its last step.
func waitPaced(id, question, key string, flight *pacedFlight) (any, error) {
	timeout := probeTimeout.load()
	// The bound is measured from the flight's last step, so the deadline is
	// the absolute instant lastStep + timeout: arming a fixed instant can't
	// overshoot however the work's steps and advances interleave with the
	// arm (#7456). lastStep is an offset from started, so the instant it
	// rebuilds keeps the monotonic reading started carries — a time.Unix
	// rebuild would drop it and put the wait back on the wall clock (#7403).
	// Each staleness read takes the clock before lastStep so a step that
	// lands mid-read makes the flight look fresher, never staler.
	lastStepAt := func() time.Time {
		return flight.started.Add(time.Duration(flight.lastStep.Load()))
	}
	timer := flight.clock.NewTimer(lastStepAt().Add(timeout))
	defer timer.Stop()
	for {
		select {
		case <-flight.done:
			return flight.res, flight.err
		case <-timer.C():
			if since := flight.clock.Now().Sub(lastStepAt()); since < timeout {
				timer.Reset(lastStepAt().Add(timeout))
				continue
			}
			pacedFlights.Lock()
			if pacedFlights.byKey[id] == flight {
				flight.timedOut = true
				pacedFlights.Unlock()
				return nil, fmt.Errorf("%s of %s did not answer within %s of its last step — the filesystem did not answer: %w: %w",
					question, key, timeout, ErrProbeTimedOut, context.DeadlineExceeded)
			}
			pacedFlights.Unlock()
			<-flight.done
			return flight.res, flight.err
		}
	}
}
