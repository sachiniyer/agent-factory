package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// drainJoinBudget bounds how long drainDaemon waits for joined goroutines (the
// poll pass, a deferred orphan sweep, the update driver's release lookup, a root
// create, a mutation's targeted persist). It sits well inside stopDaemonGrace
// (5s): the stopper SIGKILLs at the grace, so a goroutine that can block longer
// than that costs its own in-flight work and no longer the whole grace. A join
// that times out writes nothing (see skippedOnJoinTimeout) (#5846).
//
// Sizing (#5846): the final save is a per-repo flock plus one JSON write, measured
// at ~0.7ms for 10 sessions, ~6ms for 100 and ~28ms for 500 on a warm local disk
// (SaveInstancesForShutdown, 5 repos). Budgeting a pessimistic 1s for it (a slow or
// network disk, a contended flock) and keeping 1s of the 5s grace for the HTTP and
// control-plane joins that run before this join and for process exit, the join gets
// 3s. A join that finishes between this budget and roughly grace minus the save
// used to be saved on master and now is not; that window is about 1s wide.
var drainJoinBudget = 3 * time.Second

// joinWithin runs join and reports whether it returned inside d. A join that does
// not return is abandoned, not cancelled: its goroutine keeps running until the
// process exits.
func joinWithin(d time.Duration, join func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		join()
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// drainDaemon quiesces the daemon's externally reachable control planes and
// daemon-owned writers before the terminal checkpoint. It closes mutation
// admission, drains the HTTP and control servers (joining all dispatched
// handlers), re-closes web listeners to cover the ApplyConfig rebind window,
// then stops the background goroutines and joins every background writer.
//
// httpClosed and controlClosed are set via the supplied pointers so the
// deferred fallback-closers in runDaemon can skip a redundant close.
func drainDaemon(
	m *Manager,
	closeHTTP func() error,
	closeControl func() error,
	httpClosed *bool,
	controlClosed *bool,
	stopCh chan struct{},
	wg *sync.WaitGroup,
) (joined bool) {
	// Close mutation admission, then close and JOIN both externally reachable
	// control planes before taking the terminal snapshot. Closing only their
	// listeners is insufficient: an ArchiveSession (or any other writer) that
	// already passed admission can still mutate memory and block its targeted
	// persist behind SaveInstances' repo flock. The transport cleanup functions
	// wait for those dispatched handlers, so nothing user-driven can begin or
	// remain in flight across the checkpoint.
	// Advisory work (host probes) is abandoned before anything else: it must not be
	// able to hold the drain, or the final checkpoint below, past the stopper's
	// SIGKILL (#5839).
	m.advisory.Cancel()
	m.lifecycle.markQuiescing()
	if closeHTTP != nil {
		*httpClosed = true
		if err := closeHTTP(); err != nil {
			log.WarningLog.Printf("failed to drain daemon HTTP server: %v", err)
		}
	}
	*controlClosed = true
	if err := closeControl(); err != nil {
		log.WarningLog.Printf("failed to drain daemon control server: %v", err)
	}
	// A control-socket ApplyConfig admitted before quiescing can rebind a TCP
	// listener after closeHTTP's sync.Once-guarded double close. Close once
	// more after every control handler has returned so no replacement listener
	// outlives shutdown.
	if m.webListeners != nil {
		if err := m.webListeners.close(); err != nil {
			log.WarningLog.Printf("failed to close web listeners after control drain: %v", err)
		}
	}

	// Stop and join daemon-owned writers too. The lifecycle gate above also
	// refuses any scheduler/watcher delivery that reaches the closed transports
	// during their deferred teardown.
	close(stopCh)
	joined = joinWithin(drainJoinBudget, func() {
		wg.Wait()
		// The poll loop is out, so no further root-agent create can be launched; wait
		// for one that already is (#3721). JOINED, never cancelled — a create torn
		// down mid-provision is the half-created session the always-ensure loop has
		// no way to reconcile — and BEFORE the final SaveInstances, so a create that
		// lands late is persisted rather than overwritten by a save that predates it.
		m.abandonRootProgramDriftInspectionsForShutdown()
		m.waitRootAgentCreatesForShutdown()
		// RPCs and the poll are gone, and root creates are joined. No detached durable
		// writer may now be admitted; let pre-destructive and permanently stalled work
		// stand down, and join any mutation already admitted through its targeted persist.
		m.stopAndWaitBackgroundMutationsForShutdown()
	})
	if !joined {
		log.WarningLog.Printf("shutdown join exceeded %s; skipping the final save so a wedged goroutine cannot hold exit past the SIGKILL grace, and writing nothing: %s (#5846)",
			drainJoinBudget, m.skippedOnJoinTimeout())
	}
	return joined
}

// skippedOnJoinTimeout names what a timed-out join leaves undurable, for the log.
//
// There is deliberately no checkpoint on this path. Any durable write taken
// while a joined goroutine can still commit (the poll pass, a settlement, a
// stand-down discharge, a create holding the repo start lock, RefreshInstances
// holding m.mu) races that writer: the only lock every disk write shares is the
// repo file lock, and in-memory rows are not under it. Targeted persists already
// made every real mutation durable; what a timeout loses is the wedged
// goroutine's in-flight work, stale status the next poll recomputes, and persists
// that had ALREADY failed and were waiting on a retry, which is exactly what a
// SIGKILL leaves on master. m.mu is only try-locked: this must not block.
func (m *Manager) skippedOnJoinTimeout() string {
	if !m.mu.TryLock() {
		return "the owed-retry counts could not be read without blocking"
	}
	defer m.mu.Unlock()
	return fmt.Sprintf("%d owed settlement retr%s and %d owed stand-down discharge%s stay as the disk has them",
		len(m.settleOwed), plural(len(m.settleOwed), "y", "ies"),
		len(m.dischargeOwed), plural(len(m.dischargeOwed), "", "s"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
