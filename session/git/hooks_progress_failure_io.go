package git

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type hookEntryRecoveryWriteFlight struct {
	done     chan struct{}
	err      error
	timedOut bool
}

var hookEntryRecoveryWriteFlights = struct {
	sync.Mutex
	byReceipt map[string]*hookEntryRecoveryWriteFlight
}{byReceipt: make(map[string]*hookEntryRecoveryWriteFlight)}

// A receipt transition is one transaction: private-directory creation, marker
// writes, exclusive publication, and failed-publication cleanup all share one
// deadline. The runner can hold its journal lease while it waits here, so an
// inconclusive write must return at the identity-probe bound and keep the
// ordered suffix pending. The per-receipt flight prevents retries from piling
// up workers on the same stalled mount; its worker owns no home-wide lock.
func boundedHookEntryRecoveryWrite(ctx context.Context, receipt string, write func() error) error {
	hookEntryRecoveryWriteFlights.Lock()
	if active := hookEntryRecoveryWriteFlights.byReceipt[receipt]; active != nil {
		if active.timedOut {
			hookEntryRecoveryWriteFlights.Unlock()
			return hookEntryRecoveryWriteTimeout(receipt)
		}
		hookEntryRecoveryWriteFlights.Unlock()
		return waitForHookEntryRecoveryWrite(ctx, receipt, active)
	}
	flight := &hookEntryRecoveryWriteFlight{done: make(chan struct{})}
	hookEntryRecoveryWriteFlights.byReceipt[receipt] = flight
	hookEntryRecoveryWriteFlights.Unlock()
	go func() {
		flight.err = write()
		hookEntryRecoveryWriteFlights.Lock()
		if hookEntryRecoveryWriteFlights.byReceipt[receipt] == flight {
			delete(hookEntryRecoveryWriteFlights.byReceipt, receipt)
		}
		close(flight.done)
		hookEntryRecoveryWriteFlights.Unlock()
	}()
	return waitForHookEntryRecoveryWrite(ctx, receipt, flight)
}

func waitForHookEntryRecoveryWrite(ctx context.Context, receipt string, flight *hookEntryRecoveryWriteFlight) error {
	timeout := relocationIdentityTimeout
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		hookEntryRecoveryWriteFlights.Lock()
		if hookEntryRecoveryWriteFlights.byReceipt[receipt] == flight {
			flight.timedOut = true
			hookEntryRecoveryWriteFlights.Unlock()
			return hookEntryRecoveryWriteTimeout(receipt)
		}
		hookEntryRecoveryWriteFlights.Unlock()
		<-flight.done
		return flight.err
	}
}

func hookEntryRecoveryWriteTimeout(receipt string) error {
	return fmt.Errorf("timed out after %s while recording terminal hook receipt %s: %w", relocationIdentityTimeout, receipt, context.DeadlineExceeded)
}
