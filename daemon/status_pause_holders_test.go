package daemon

import (
	"testing"

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
