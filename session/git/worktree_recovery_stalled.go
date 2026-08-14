package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// BoundedLstat runs os.Lstat through the shared identity-probe deadline (#3278
// review). The destruction path's absence checks run while the caller holds
// the session operation lock, so a stalled FUSE/NFS mount must surface as a
// timeout — an unknown, fail-closed answer distinct from ENOENT — never as an
// unbounded syscall wedging the kill. Per-path flight with a timed-out latch,
// mirroring VerifyArchivedWorktreePointer: concurrent callers join one worker,
// and after a deadline retries are refused instead of stacking goroutines and
// blocked OS threads against the same stalled path until the stuck worker
// finally returns and unpublishes itself.
func BoundedLstat(path string) (os.FileInfo, error) {
	boundedLstatFlights.Lock()
	if active := boundedLstatFlights.byPath[path]; active != nil {
		if active.timedOut {
			boundedLstatFlights.Unlock()
			return nil, fmt.Errorf(
				"path check for %s is still running after an earlier deadline: %w",
				path, context.DeadlineExceeded,
			)
		}
		boundedLstatFlights.Unlock()
		return waitForBoundedLstat(path, active)
	}
	flight := &boundedLstatFlight{done: make(chan struct{})}
	boundedLstatFlights.byPath[path] = flight
	boundedLstatFlights.Unlock()
	go func() {
		flight.info, flight.err = os.Lstat(path)
		boundedLstatFlights.Lock()
		if boundedLstatFlights.byPath[path] == flight {
			delete(boundedLstatFlights.byPath, path)
		}
		close(flight.done)
		boundedLstatFlights.Unlock()
	}()
	return waitForBoundedLstat(path, flight)
}

type boundedLstatFlight struct {
	done     chan struct{}
	info     os.FileInfo
	err      error
	timedOut bool
}

var boundedLstatFlights = struct {
	sync.Mutex
	byPath map[string]*boundedLstatFlight
}{byPath: map[string]*boundedLstatFlight{}}

func waitForBoundedLstat(path string, flight *boundedLstatFlight) (os.FileInfo, error) {
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.info, flight.err
	case <-timer.C:
		boundedLstatFlights.Lock()
		if boundedLstatFlights.byPath[path] == flight {
			flight.timedOut = true
			boundedLstatFlights.Unlock()
			return nil, fmt.Errorf(
				"timed out after %s while checking path %s: %w",
				relocationIdentityTimeout, path, context.DeadlineExceeded,
			)
		}
		boundedLstatFlights.Unlock()
		<-flight.done
		return flight.info, flight.err
	}
}

// SettleStalledRelocationForAbsentPath clears an identity-unknown stalled
// record once its guarded pathname is conclusively absent (#3278 review). Such
// a record holds no directory identity — the probe that created it never
// answered — so when the pathname now answers ENOENT there is nothing left
// for the fence to protect, and retaining it would leave the row permanently
// undeletable: every claim wraps the same ENOENT in ErrRelocateStateUnknown
// and every kill and restore refuses. Identity-qualified records are
// deliberately excluded: they may carry an alternate candidate that the
// ordinary claim resolution must arbitrate.
func (g *GitWorktree) SettleStalledRelocationForAbsentPath() error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.activeRelocationClaim != nil || g.relocationRecovery == nil ||
		g.relocationRecovery.State != RelocationRecoveryStalled ||
		g.relocationRecovery.IdentityKnown {
		return fmt.Errorf("no identity-unknown stalled relocation record to settle")
	}
	if _, err := BoundedLstat(g.worktreePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stalled relocation path %s is not conclusively absent: %v", g.worktreePath, err)
	}
	g.relocationRecovery = nil
	return nil
}
