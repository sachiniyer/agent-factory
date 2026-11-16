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
//
// The join also never outlasts drainBudget, which the steps before it share.
var drainJoinBudget = 3 * time.Second

// drainBudget bounds the whole drain from its first step: the HTTP and
// control-plane handler joins, the web-listener close, and the background join
// all draw on this one deadline (#7393). Those handler joins used to be
// unbounded. On 11-15 one of them held a stop for 90s, until systemd's
// SIGKILL, and the final save, every deferred teardown and the exit all went
// with it.
//
// Sizing: it leaves the save's pessimistic 1s (see drainJoinBudget) inside the
// stopper's 5s grace (stopDaemonGrace). A handler still running at the deadline
// is abandoned, and the save is skipped exactly as for a timed-out join: that
// handler passed admission, so it can still commit, and it may hold m.mu, an
// archive settlement or a repo flock that the save would wait on.
var drainBudget = 4 * time.Second

// drainStepFloor is the least any drain step is given, even past the deadline,
// so a step that would finish at once is never reported as abandoned just
// because an earlier one spent the budget. Past the deadline it adds at most one
// floor per step still to run (three: control, web listeners, the join), which
// the save's 1s reserve absorbs.
const drainStepFloor = 100 * time.Millisecond

// drainRemaining is what is left of the drain's deadline, never below the floor.
func drainRemaining(deadline time.Time) time.Duration {
	return max(time.Until(deadline), drainStepFloor)
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

// drainDaemon quiesces the daemon's externally reachable control planes and
// daemon-owned writers before the terminal checkpoint. It closes mutation
// admission, drains the HTTP and control servers (joining all dispatched
// handlers), re-closes web listeners to cover the ApplyConfig rebind window,
// then stops the background goroutines and joins every background writer.
// Every one of those steps shares drainBudget, so no handler and no writer can
// hold a stop until the stopper's SIGKILL. joined is false when any step was
// abandoned, and the caller then skips the final save (#5846, #7393).
//
// httpClosed and controlClosed are set via the supplied pointers so the
// deferred fallback-closers in runDaemon can skip a redundant close.
//
// watchers is latched first, before anything here can block: its watch loops
// stop restarting and stop counting exits from this instant, not from the
// deferred watchers.Stop that runs only after this drain and the final save
// (#7392). nil is allowed.
func drainDaemon(
	m *Manager,
	watchers *watcherSupervisor,
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
	//
	// The watchers stand down first of all. Stopping the daemon's unit also
	// stops every watch's bound scope, and a drain that hangs past that would
	// otherwise keep restarting watch commands into systemd's refusals and
	// spend their crash-loop budget on its own stop (#7392).
	watchers.beginShutdown()
	m.advisory.Cancel()
	m.lifecycle.markQuiescing()

	// Every step from here to the save draws on one deadline (#7393). A step
	// that cannot finish in what is left is abandoned, named in the log, and
	// costs the final save — never the whole stop.
	deadline := time.Now().Add(drainBudget)
	handlersJoined := true
	step := func(what string, run func() error) {
		var err error
		if !joinWithin(drainRemaining(deadline), func() { err = run() }) {
			handlersJoined = false
			log.WarningLog.Printf("shutdown: %s did not finish within the %s drain budget; abandoning it and skipping the final save, "+
				"since an abandoned handler can still commit — still running: %s (#7393)",
				what, drainBudget, m.lifecycle.inFlight().describe(time.Now()))
			return
		}
		if err != nil {
			log.WarningLog.Printf("failed to %s: %v", what, err)
		}
	}
	if closeHTTP != nil {
		*httpClosed = true
		step("drain daemon HTTP server", closeHTTP)
	}
	*controlClosed = true
	step("drain daemon control server", closeControl)
	// A control-socket ApplyConfig admitted before quiescing can rebind a TCP
	// listener after closeHTTP's sync.Once-guarded double close. Close once
	// more after every control handler has returned so no replacement listener
	// outlives shutdown.
	if m.webListeners != nil {
		step("close web listeners after control drain", m.webListeners.close)
	}

	// Stop and join daemon-owned writers too. The lifecycle gate above also
	// refuses any scheduler/watcher delivery that reaches the closed transports
	// during their deferred teardown.
	close(stopCh)
	joinBudget := min(drainJoinBudget, drainRemaining(deadline))
	joined = joinWithin(joinBudget, func() {
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
			joinBudget, m.skippedOnJoinTimeout())
	}
	return joined && handlersJoined
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
