package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// TestPauseStatusPoll_ReleaseByOneHolderKeepsTheOthersPause is #3027: the lease is
// a mutual-exclusion primitive with a single slot, so two overlapping holders share
// one entry and the FIRST release drops it for both. The second holder then
// believes it holds a pause that is no longer in effect — worse than never having
// taken one, because it is a false guarantee: the poll it thinks is suppressed
// resumes underneath its long operation, and deferWhileAttached (daemon/delivery.go)
// stops holding automated deliveries for a pane someone is still typing in.
//
// Reachable today with no browser involved: two TUIs attached to the same session,
// the first to detach calls ResumeStatusPoll unconditionally (app/home_attach.go
// runStatusPollResume) and revokes the second's claim.
func TestPauseStatusPoll_ReleaseByOneHolderKeepsTheOthersPause(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "shared", session.NewFakeBackend(), true, session.Running)
	inst := manager.instances[daemonInstanceKey(repoID, "shared")]

	manager.PauseStatusPollFor(repoID, "shared", inst.ID, "tui-a")
	manager.PauseStatusPollFor(repoID, "shared", inst.ID, "tui-b")
	if !manager.isPollPaused(repoID, "shared", inst.ID) {
		t.Fatal("two holders took the pause; it must be in effect")
	}

	// The first holder detaches. The second is still attached and still typing.
	manager.ResumeStatusPollFor(repoID, "shared", inst.ID, "tui-a")

	if !manager.isPollPaused(repoID, "shared", inst.ID) {
		t.Fatal("one holder released and the pause was lifted for BOTH; the remaining holder now has a false guarantee")
	}

	// Only when the last holder leaves does the pause lift.
	manager.ResumeStatusPollFor(repoID, "shared", inst.ID, "tui-b")
	if manager.isPollPaused(repoID, "shared", inst.ID) {
		t.Fatal("the last holder released; the pause must lift")
	}
}

// A holder releasing twice must not lift a pause another holder still holds — the
// release has to be idempotent per holder, not a decrement that can be replayed.
// A TUI whose detach path retries a best-effort RPC does exactly this.
func TestResumeStatusPoll_DoubleReleaseByOneHolderIsIdempotent(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "shared", session.NewFakeBackend(), true, session.Running)
	inst := manager.instances[daemonInstanceKey(repoID, "shared")]

	manager.PauseStatusPollFor(repoID, "shared", inst.ID, "tui-a")
	manager.PauseStatusPollFor(repoID, "shared", inst.ID, "tui-b")
	manager.ResumeStatusPollFor(repoID, "shared", inst.ID, "tui-a")
	manager.ResumeStatusPollFor(repoID, "shared", inst.ID, "tui-a")

	if !manager.isPollPaused(repoID, "shared", inst.ID) {
		t.Fatal("a repeated release by one holder revoked another holder's claim")
	}
}

// A heartbeat renewal is an OVERWRITE of that holder's expiry, not a second
// acquisition. This is why the lease is keyed by holder rather than counted: the
// attached TUI renews once a second, so a count would climb for the life of the
// attach and never reach zero on detach.
func TestPauseStatusPoll_RenewalIsNotASecondAcquisition(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "shared", session.NewFakeBackend(), true, session.Running)
	inst := manager.instances[daemonInstanceKey(repoID, "shared")]

	for i := 0; i < 5; i++ { // five heartbeats from ONE holder
		manager.PauseStatusPollFor(repoID, "shared", inst.ID, "tui-a")
	}
	manager.ResumeStatusPollFor(repoID, "shared", inst.ID, "tui-a")

	if manager.isPollPaused(repoID, "shared", inst.ID) {
		t.Fatal("one holder renewed five times and released once; the pause must lift")
	}
}

// The legacy shared holder keeps working: a client that sends no holder id behaves
// exactly as it did before #3027, so an old TUI is not broken by the change.
func TestPauseStatusPoll_LegacyHolderStillPausesAndResumes(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "legacy", session.NewFakeBackend(), true, session.Running)
	inst := manager.instances[daemonInstanceKey(repoID, "legacy")]

	manager.PauseStatusPoll(repoID, "legacy", inst.ID)
	if !manager.isPollPaused(repoID, "legacy", inst.ID) {
		t.Fatal("the legacy path must still pause")
	}
	manager.ResumeStatusPoll(repoID, "legacy", inst.ID)
	if manager.isPollPaused(repoID, "legacy", inst.ID) {
		t.Fatal("the legacy path must still resume")
	}
}

// The task-run backstop entry belongs to the PAUSE, not to any one holder: it is
// armed while a session is paused and bounds how long that pause may hide a task
// run's completion from the concurrency cap (#1892). Disarming it while another
// holder still has the session paused would be the same one-slot mistake this
// change fixes, one map over — so it is cleared only when the last holder leaves.
func TestResumeStatusPoll_BackstopSurvivesUntilTheLastHolderLeaves(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "shared", session.NewFakeBackend(), true, session.Running)
	inst := manager.instances[daemonInstanceKey(repoID, "shared")]
	probeKey := stableSessionKey(repoID, inst)

	manager.PauseStatusPollFor(repoID, "shared", inst.ID, "tui-a")
	manager.PauseStatusPollFor(repoID, "shared", inst.ID, "tui-b")
	manager.pausedMu.Lock()
	manager.taskRunProbeDue[probeKey] = nowFunc().Add(taskRunPollBackstop)
	manager.pausedMu.Unlock()

	manager.ResumeStatusPollFor(repoID, "shared", inst.ID, "tui-a")
	manager.pausedMu.Lock()
	_, armed := manager.taskRunProbeDue[probeKey]
	manager.pausedMu.Unlock()
	if !armed {
		t.Fatal("one holder left but the session is still paused; disarming the backstop leaves that pause unbounded")
	}

	manager.ResumeStatusPollFor(repoID, "shared", inst.ID, "tui-b")
	manager.pausedMu.Lock()
	_, stillArmed := manager.taskRunProbeDue[probeKey]
	manager.pausedMu.Unlock()
	if stillArmed {
		t.Fatal("the last holder left; the backstop entry must be reclaimed with the pause")
	}
}

// An expired holder must not keep a pause alive for a holder that never released,
// and must not block the last-holder cleanup either — the lease bound is what makes
// a crashed client safe (#1160), and per-holder keying must not weaken it.
func TestPauseStatusPoll_AnExpiredHolderDoesNotHoldThePause(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "shared", session.NewFakeBackend(), true, session.Running)
	inst := manager.instances[daemonInstanceKey(repoID, "shared")]

	base := nowFunc()
	manager.PauseStatusPollFor(repoID, "shared", inst.ID, "crashed")
	restore := nowFunc
	nowFunc = func() time.Time { return base.Add(statusPollLease + time.Second) }
	defer func() { nowFunc = restore }()

	if manager.isPollPaused(repoID, "shared", inst.ID) {
		t.Fatal("the only holder's lease expired; a crashed client must not blind the daemon")
	}
}

// #3028 review: a session can be held under TWO namespaces at once — an ID-bearing
// client keys the lease by stable id, a legacy one by title, and isPollPaused treats
// either as an active pause. Judging the backstop by "this key is now empty" would
// disarm it while the other namespace still holds the session, and the next paused
// refresh would rearm it from scratch, delaying task-run completion (and the
// concurrency slot it releases) by another full taskRunPollBackstop.
func TestResumeStatusPoll_BackstopSurvivesAHolderInTheOtherNamespace(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "shared", session.NewFakeBackend(), true, session.Running)
	inst := manager.instances[daemonInstanceKey(repoID, "shared")]
	probeKey := stableSessionKey(repoID, inst)

	// An ID-bearing client and a legacy ID-less one, holding the same session under
	// different keys.
	manager.PauseStatusPollFor(repoID, "shared", inst.ID, "modern")
	manager.PauseStatusPollFor(repoID, "shared", "", "legacy")
	manager.pausedMu.Lock()
	manager.taskRunProbeDue[probeKey] = nowFunc().Add(taskRunPollBackstop)
	manager.pausedMu.Unlock()

	// The ID-bearing one leaves. Its key is now empty, but the session is still
	// paused by the legacy holder.
	manager.ResumeStatusPollFor(repoID, "shared", inst.ID, "modern")

	if !manager.isPollPaused(repoID, "shared", inst.ID) {
		t.Fatal("the legacy holder still holds the pause")
	}
	manager.pausedMu.Lock()
	_, armed := manager.taskRunProbeDue[probeKey]
	manager.pausedMu.Unlock()
	if !armed {
		t.Fatal("the backstop was disarmed while the session is still paused, leaving that pause unbounded")
	}

	// Only when the last namespace releases does the backstop go with it.
	manager.ResumeStatusPollFor(repoID, "shared", "", "legacy")
	manager.pausedMu.Lock()
	_, stillArmed := manager.taskRunProbeDue[probeKey]
	manager.pausedMu.Unlock()
	if stillArmed {
		t.Fatal("no holder remains; the backstop entry must be reclaimed with the pause")
	}
}

// The reverse ordering of the test above: a LEGACY client (id="") resumes FIRST
// while an ID-bearing client still holds the pause under the ID-keyed lease. The
// cleanup predicate isPollPaused uses covers BOTH namespaces, so the backstop
// must stay armed until the ID-bearing holder also leaves — but the legacy resume
// only clears the title namespace, and a backstop check that judged it by "this
// caller's key is now empty" would miss the still-held ID-keyed lease, delete the
// backstop mid-pause, and delay task-run completion (#1892) detection by another
// full taskRunPollBackstop when the next paused refresh rearmed it from scratch.
//
// The cross-namespace case is exercised above for the ID-bearing-resumes-first
// ordering; this covers the order the original commit's review missed. See #3028.
func TestResumeStatusPoll_BackstopSurvivesAHolderInTheOtherNamespaceWhenLegacyResumesFirst(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "shared", session.NewFakeBackend(), true, session.Running)
	inst := manager.instances[daemonInstanceKey(repoID, "shared")]
	probeKey := stableSessionKey(repoID, inst)

	// An ID-bearing client and a legacy ID-less one, holding the same session under
	// different keys.
	manager.PauseStatusPollFor(repoID, "shared", inst.ID, "modern")
	manager.PauseStatusPollFor(repoID, "shared", "", "legacy")
	manager.pausedMu.Lock()
	manager.taskRunProbeDue[probeKey] = nowFunc().Add(taskRunPollBackstop)
	manager.pausedMu.Unlock()

	// The legacy one leaves FIRST. Its title key is now empty, but the session is
	// still paused by the ID-bearing holder.
	manager.ResumeStatusPollFor(repoID, "shared", "", "legacy")

	if !manager.isPollPaused(repoID, "shared", inst.ID) {
		t.Fatal("the ID-bearing holder still holds the pause")
	}
	manager.pausedMu.Lock()
	_, armed := manager.taskRunProbeDue[probeKey]
	manager.pausedMu.Unlock()
	if !armed {
		t.Fatal("the backstop was disarmed while the session is still paused by the ID-bearing holder, leaving that pause unbounded")
	}

	// Only when the last namespace releases does the backstop go with it.
	manager.ResumeStatusPollFor(repoID, "shared", inst.ID, "modern")
	manager.pausedMu.Lock()
	_, stillArmed := manager.taskRunProbeDue[probeKey]
	manager.pausedMu.Unlock()
	if stillArmed {
		t.Fatal("no holder remains; the backstop entry must be reclaimed with the pause")
	}
}
