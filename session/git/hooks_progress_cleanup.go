package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

type hookProgressCleanup struct {
	journal  string
	receipt  string
	progress *hookProgress
}

type hookProgressCleanupFlight struct {
	done chan struct{}
	err  error
}

var hookProgressCleanupFlights = struct {
	sync.Mutex
	byPath map[string]*hookProgressCleanupFlight
}{byPath: make(map[string]*hookProgressCleanupFlight)}

// Cleanup happens only after the progress lock has published a non-resumable
// retired name. Start every independent deletion together and give the batch
// one identity-probe budget; a stalled receipt neither holds .progress nor
// charges one timeout per artifact. A later pass joins an unfinished flight.
func cleanupHookProgressArtifacts(cleanups []hookProgressCleanup) error {
	if len(cleanups) == 0 {
		return nil
	}
	flights := make([]*hookProgressCleanupFlight, 0, len(cleanups))
	for _, cleanup := range cleanups {
		flights = append(flights, startHookProgressCleanup(cleanup))
	}
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	var result error
	for _, flight := range flights {
		select {
		case <-flight.done:
			result = errors.Join(result, flight.err)
		case <-timer.C:
			return errors.Join(result, fmt.Errorf("hook receipt cleanup exceeded %s: %w", relocationIdentityTimeout, context.DeadlineExceeded))
		}
	}
	return result
}

func startHookProgressCleanup(cleanup hookProgressCleanup) *hookProgressCleanupFlight {
	key := cleanup.journal
	if key == "" {
		key = cleanup.receipt
	}
	hookProgressCleanupFlights.Lock()
	if active := hookProgressCleanupFlights.byPath[key]; active != nil {
		hookProgressCleanupFlights.Unlock()
		return active
	}
	flight := &hookProgressCleanupFlight{done: make(chan struct{})}
	hookProgressCleanupFlights.byPath[key] = flight
	removeProgress := hookProgressRemove
	removeAll := os.RemoveAll
	hookProgressCleanupFlights.Unlock()
	go func() {
		switch {
		case cleanup.progress != nil:
			flight.err = removeProgress(cleanup.journal, cleanup.progress)
		case cleanup.receipt != "":
			_, flight.err = withInactiveHookProgressLease(cleanup.receipt, func() error { return removeAll(cleanup.receipt) })
		default:
			flight.err = os.Remove(cleanup.journal)
			if os.IsNotExist(flight.err) {
				flight.err = nil
			}
		}
		hookProgressCleanupFlights.Lock()
		if hookProgressCleanupFlights.byPath[key] == flight {
			delete(hookProgressCleanupFlights.byPath, key)
		}
		close(flight.done)
		hookProgressCleanupFlights.Unlock()
	}()
	return flight
}
