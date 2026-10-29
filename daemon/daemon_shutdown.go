package daemon

import (
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// drainJoinBudget bounds how long drainDaemon waits for joined goroutines (the
// poll pass, a deferred orphan sweep, the update driver's release lookup, a root
// create, a mutation's targeted persist) after the first checkpoint is durable.
// It sits well inside stopDaemonGrace (5s): the stopper SIGKILLs at the grace, so
// a goroutine that can block longer than that must cost at most its own
// in-flight work, never the final save (#5846).
var drainJoinBudget = 2 * time.Second

// checkpointBeforeJoin writes the durable checkpoint that precedes the join
// (#5846). It refreshes rows already on disk from the live instances and never
// adds, removes or re-orders membership (session.Storage.CheckpointLiveRows):
// goroutines that are still joining, a root-agent replacement above all, may
// commit while it runs, and a whole-list write from a stale snapshot would put the
// replaced row back. It does not seal archive admission either. It is skipped when
// the home directory is gone (see homeDeleted). A var so a test can observe the
// ordering.
var checkpointBeforeJoin = func(m *Manager) error {
	if m.homeDeleted.Load() {
		return nil
	}
	return m.checkpointLiveRows()
}

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

// markHomeDeleted records that the AF home is gone, so no checkpoint may be
// written: saving would recreate a skeleton of the deleted home.
func (m *Manager) markHomeDeleted() { m.homeDeleted.Store(true) }

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

	// Durable checkpoint BEFORE anything is joined (#5846). Admission is closed and
	// both control planes are joined, so nothing user-driven is in flight: this
	// captures every mutation made up to now. The joins below can then be bounded
	// without ever putting the checkpoint behind a goroutine that cannot be
	// cancelled. Ordering argument: a join that completes is followed by the
	// final save (daemon.go), which is a superset of this one; a join that does
	// not complete is abandoned and this checkpoint stands, losing at most that
	// goroutine's in-flight work, which is never durable until its own persist.
	if err := checkpointBeforeJoin(m); err != nil {
		log.ErrorLog.Printf("failed to write the pre-drain checkpoint: %v", err)
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
		log.WarningLog.Printf("shutdown join exceeded %s; keeping the pre-drain checkpoint and skipping the final save so a wedged goroutine cannot hold exit past the SIGKILL grace (#5846)", drainJoinBudget)
	}
	return joined
}

// checkpointLiveRows is the pre-drain checkpoint's body. Membership is
// re-validated under the repo file lock against the manager's current instance
// map; m.mu is only TRY-locked there, because a writer that holds m.mu may be
// waiting on that same file lock, and a skipped row is safe (the final save or the
// row's own targeted persist covers it) where a lock-order deadlock inside the
// stop grace is not.
func (m *Manager) checkpointLiveRows() error {
	// m.mu is only TRY-locked, here and below (see above).
	tryMu := func(fn func()) bool {
		for attempt := 0; attempt < 20; attempt++ {
			if m.mu.TryLock() {
				defer m.mu.Unlock()
				fn()
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}
	isCurrent := func(inst *session.Instance) bool {
		current := false
		tryMu(func() {
			for _, cur := range m.instances {
				if cur == inst {
					current = true
					return
				}
			}
		})
		return current
	}

	// Owed stand-down discharges (#4730) must reach this checkpoint: the join
	// timeout skips the final save and its FlushOwedSettlements, so this can be
	// the last durable write, and persisting a marker memory already discharged
	// would re-arm a teardown the worker stood down. They are applied to the rows
	// being written rather than flushed through FlushOwedSettlements, because that
	// path takes the repo start lock, which CreateSession holds across
	// provisioning and the prompt send: nothing before the join may wait on a
	// lock an in-flight repository operation can hold for an unbounded time. When
	// the owed set cannot be read without blocking, nothing is written: a stale
	// marker is worse than no checkpoint, and the final save covers a clean join.
	type owedRow struct{ id, title string }
	owed := make(map[owedRow]bool)
	if !tryMu(func() {
		for _, entry := range m.dischargeOwed {
			owed[owedRow{entry.instance.ID, entry.instance.Title}] = true
		}
	}) {
		log.WarningLog.Printf("pre-drain checkpoint skipped: the owed-discharge set could not be read without blocking (#5846)")
		return nil
	}
	adjust := func(data *session.InstanceData) {
		if owed[owedRow{data.ID, data.Title}] {
			data.PendingOnComplete = nil
		}
	}
	return m.storage.CheckpointLiveRows(m.InstancesSnapshot(), isCurrent, adjust)
}
