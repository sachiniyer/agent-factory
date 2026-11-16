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
	// lastStep is when work last reported a step, in Unix nanoseconds.
	lastStep atomic.Int64
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
// returns, since an abandoned flight returns after its caller has gone. As
// with Bounded, question and key name what work decides and about what, only
// a caller asking the same question of the same key joins a flight, and an
// answer of a type other than T is an error, never a panic.
func Paced[T any](question, key string, work func(step func()) (T, error)) (T, error) {
	var zero T
	got, err := runPaced(question+"\x00"+key, question, key, func(step func()) (any, error) { return work(step) })
	if err != nil {
		return zero, err
	}
	answer, ok := got.(T)
	if !ok {
		return zero, fmt.Errorf("%s of %s answered with %T, not %T", question, key, got, zero)
	}
	return answer, nil
}

func runPaced(id, question, key string, work func(step func()) (any, error)) (any, error) {
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
	flight := &pacedFlight{done: make(chan struct{})}
	flight.lastStep.Store(time.Now().UnixNano())
	pacedFlights.byKey[id] = flight
	pacedFlights.Unlock()
	go func() {
		res, err := work(func() { flight.lastStep.Store(time.Now().UnixNano()) })
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
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-flight.done:
			return flight.res, flight.err
		case <-timer.C:
			if since := time.Since(time.Unix(0, flight.lastStep.Load())); since < timeout {
				timer.Reset(timeout - since)
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
