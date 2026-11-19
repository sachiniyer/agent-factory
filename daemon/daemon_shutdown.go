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
// The join also never outlasts drainBudget, which the steps before it share. So
// handler joins that do finish now eat into this join's 3s: an HTTP drain that
// takes 1.5s leaves the join 2s, where it used to get the full 3s (#7393).
var drainJoinBudget = defaultDrainJoinBudget

// drainBudget bounds the whole drain, measured from when the stop was
// requested (#7416): the HTTP and control-plane handler joins, the
// web-listener close, and the background join all draw on this one deadline
// (#7393). Those handler joins used to be
// unbounded. On 11-15 one of them held a stop for 90s, until systemd's
// SIGKILL, and the final save, every deferred teardown and the exit all went
// with it.
//
// Sizing: the worst stop is the whole budget, plus one floor for every step
// after the first, plus the save's reserve. That is 3.5s + 3×100ms + 1s =
// 4.8s, inside the stopper's 5s grace (stopDaemonGrace), and the pins below
// fail the build if it outgrows it. The save is held to that 4.8s (saveBy,
// #7439), and the teardown's floor and the exit reserve take the last 200ms
// (daemon_teardown.go, #7416). A handler still running at the deadline is
// abandoned, and the save is skipped exactly as for a timed-out join: that
// handler passed admission, so it can still commit, and it may hold m.mu, an
// archive settlement or a repo flock that the save would wait on.
var drainBudget = defaultDrainBudget

// The drain's production values. The vars above are initialised from these and
// only tests reassign them, so the pins below hold over what ships, whatever a
// test helper sets.
const (
	defaultDrainJoinBudget = 3 * time.Second
	defaultDrainBudget     = 3500 * time.Millisecond
	// drainSaveReserve is the pessimistic time budgeted for the final save
	// (drainJoinBudget's sizing).
	drainSaveReserve = time.Second
	// drainPlaneSteps is how many steps before the background join draw on the
	// deadline: the HTTP drain, the control drain, the web-listener close. It
	// sizes drainPlanes' step array, so a fourth step does not compile until
	// this is raised, and raising it re-checks the pin below.
	drainPlaneSteps = 3
)

// Pins (#7393 review). Each fails to build when its relation breaks, because a
// negative constant cannot be converted to uint.
const (
	// The worst stop fits the stopper's grace: the first step spends the whole
	// budget, every later step (the other planes and the background join) gets
	// its floor, then the save runs.
	_ = uint(sigtermFallbackGrace - (defaultDrainBudget + drainPlaneSteps*drainStepFloor + drainSaveReserve))
	// The background join never claims more than the whole drain.
	_ = uint(defaultDrainBudget - defaultDrainJoinBudget)
)

// drainStepFloor is the least any drain step is given, even past the deadline,
// so a step that would finish at once is never reported as abandoned just
// because an earlier one spent the budget. Past the deadline it adds at most one
// floor per step still to run, which the pins above count.
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

// drainDeadline is when m's drain must be done: drainBudget after its stop
// began (#7416). A manager with no stop clock starts the clock now.
func drainDeadline(m *Manager) time.Time {
	return m.lifecycle.stopClock().began().Add(drainBudget)
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
// controlClosed is set via the supplied pointer so the deferred fallback drain
// in runDaemon can skip a redundant one.
//
// watchers is latched first, before anything here can block: its watch loops
// stop restarting and stop counting exits from this instant, not from the
// deferred teardown that runs only after this drain and the final save
// (#7392). nil is allowed.
//
// reserve is spent work the caller has already promised the stop's tail: a
// marked throwaway home reserves its teardown's known bound inside the stop
// deadline here by shrinking the drain's (#7620 item 4), so the synchronous
// teardown that follows is not squeezed out by the drain's own budget.
func drainDaemon(
	m *Manager,
	watchers *watcherSupervisor,
	closeHTTP func() error,
	closeControl func() error,
	controlClosed *bool,
	stopCh chan struct{},
	wg *sync.WaitGroup,
	reserve time.Duration,
) (joined bool) {
	// Every step from here to the save draws on one deadline (#7393), measured
	// from when the stop was requested (#7416). A step that cannot finish in
	// what is left is abandoned, named in the log, and costs the final save —
	// never the whole stop.
	deadline := drainDeadline(m).Add(-reserve)
	handlersJoined := drainPlanes(m, watchers, closeHTTP, closeControl, controlClosed, deadline)

	// Stop and join daemon-owned writers too. The lifecycle gate drainPlanes
	// closed also refuses any scheduler/watcher delivery that reaches the closed
	// transports during their deferred teardown.
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
		m.lifecycle.stopClock().abandon("the background join")
	}
	return joined && handlersJoined
}

// drainPlanes closes mutation admission and drains both externally reachable
// control planes by deadline: the HTTP drain, the control drain, then the
// web-listener close. It is every stop's plane drain (#7416). drainDaemon runs
// it on the graceful path, and runDaemon defers it for every other exit — a
// signal or Shutdown during the restore or upgrade probation, a failed restore
// — which used to call both closes directly, with no bound. handlersJoined is
// false when a step was abandoned; the step and what was still running are
// named in the log.
//
// Closing only the listeners is insufficient: an ArchiveSession (or any other
// writer) that already passed admission can still mutate memory and block its
// targeted persist behind SaveInstances' repo flock. The transport cleanup
// functions wait for those dispatched handlers, so nothing user-driven can
// begin or remain in flight across the checkpoint.
func drainPlanes(
	m *Manager,
	watchers *watcherSupervisor,
	closeHTTP func() error,
	closeControl func() error,
	controlClosed *bool,
	deadline time.Time,
) (handlersJoined bool) {
	// The watchers stand down first of all. Stopping the daemon's unit also
	// stops every watch's bound scope, and a drain that hangs past that would
	// otherwise keep restarting watch commands into systemd's refusals and
	// spend their crash-loop budget on its own stop (#7392).
	watchers.beginShutdown()
	// Advisory work (host probes) is abandoned before anything else: it must not be
	// able to hold the drain, or the final checkpoint below, past the stopper's
	// SIGKILL (#5839).
	m.advisory.Cancel()
	// An ordinary stop; an upgrade hand-off has already marked itself (#7440).
	m.lifecycle.markStopping()

	handlersJoined = true
	step := func(what string, run func() error) {
		var err error
		if !joinWithin(drainRemaining(deadline), func() { err = run() }) {
			handlersJoined = false
			m.lifecycle.stopClock().abandon(what)
			log.WarningLog.Printf("shutdown: %s did not finish within the %s drain budget; abandoning it and skipping the final save, "+
				"since an abandoned handler can still commit — still running: %s (#7393)",
				what, drainBudget, m.lifecycle.inFlight().describe(time.Now()))
			return
		}
		if err != nil {
			log.WarningLog.Printf("failed to %s: %v", what, err)
		}
	}
	*controlClosed = true
	var closeWebListeners func() error
	if m.webListeners != nil {
		closeWebListeners = m.webListeners.close
	}
	// The array's length is drainPlaneSteps, which the budget pins count, so a
	// step added here does not compile until the pins have been re-checked. A
	// step that is absent on this daemon has no run and is skipped.
	steps := [drainPlaneSteps]struct {
		what string
		run  func() error
	}{
		{"drain daemon HTTP server", closeHTTP},
		{"drain daemon control server", closeControl},
		// A control-socket ApplyConfig admitted before quiescing can rebind a
		// TCP listener after closeHTTP's sync.Once-guarded double close. Close
		// once more after every control handler has returned so no replacement
		// listener outlives shutdown.
		{"close web listeners after control drain", closeWebListeners},
	}
	for _, st := range steps {
		if st.run != nil {
			step(st.what, st.run)
		}
	}
	return handlersJoined
}

// checkpointAfterDrain takes the daemon's terminal checkpoint when, and only
// when, the drain joined everything. A drain that abandoned a handler (#7393)
// or timed out its join (#5846) writes nothing. An admitted writer may still
// commit, so the save would race it, or queue behind the lock it holds past
// the SIGKILL grace.
//
// What a skipped save leaves on disk is still whole. Every real mutation was
// already made durable by its own targeted persist, and every instances.json
// write, those and an abandoned writer's late one alike, goes through
// SaveRepoInstances to config.AtomicWriteFile (temp file, fsync, rename). The
// file is therefore the old version or the new one, never torn.
// config.TestEveryConfigWriterGoesThroughAtomicWriteFile pins that path.
//
// The save itself is bounded too (#7439). It runs until saveDeadline, the
// pinned point after the drain's worst case plus the save's reserve, whatever
// it is waiting on: an archive that never settles, a contended repo flock, a
// slow disk. A save still running then is abandoned, named, and entered in the
// stop's ledger, so the teardown still starts on time and the process exits
// holding the home. The same atomic rename means an abandoned save, like a
// SIGKILLed one, leaves each repo's file old or new: FlushOwedSettlements'
// retries go through persistInstanceData, and the save through
// SaveRepoInstances, both to config.AtomicWriteFile.
func checkpointAfterDrain(m *Manager, joined bool) {
	if !joined {
		return
	}
	// Read the seam here, on the main flow: an abandoned save's goroutine must
	// not race a test restoring it.
	save := finalSave
	var err error
	if !joinWithin(max(time.Until(saveDeadline(m)), drainStepFloor), func() { err = save(m) }) {
		m.lifecycle.stopClock().abandon("the final save")
		log.WarningLog.Printf("shutdown: the final save did not finish by %s after the stop began; abandoning it, so the "+
			"teardown still starts on time. Each repo's instances.json is its old or its new version, never torn (#7439)",
			saveBy().Round(time.Millisecond))
		return
	}
	if err != nil {
		log.ErrorLog.Printf("failed to save instances when terminating daemon: %v", err)
	}
}

// finalSave is the daemon's terminal checkpoint. A seam so a test can hold it.
var finalSave = func(m *Manager) error { return m.SaveInstancesForShutdown() }

// saveBy is how long after the stop began the final save must be done: the
// drain's worst case (its whole budget, then a floor for each later step) plus
// the save's reserve. It is the point the pins in this file and in
// daemon_teardown.go place the save's end at, so the teardown's floor and the
// exit reserve still fit inside the stopper's grace after it.
func saveBy() time.Duration {
	return drainBudget + drainPlaneSteps*drainStepFloor + drainSaveReserve
}

// saveDeadline is when m's final save must be done (#7439).
func saveDeadline(m *Manager) time.Time {
	return m.lifecycle.stopClock().began().Add(saveBy())
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
