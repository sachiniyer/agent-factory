package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// withFrozenClock swaps the pause-poll clock (#1160) for a deterministic one so
// lease-expiry assertions never race real time. It returns a setter to advance
// the fake now, and registers cleanup.
func withFrozenClock(t *testing.T) func(d time.Duration) {
	t.Helper()
	// A fixed, non-zero base — time.Now()'s actual value is irrelevant to the
	// lease math, only deltas are.
	base := time.Unix(1_700_000_000, 0)
	cur := base
	prev := nowFunc
	nowFunc = func() time.Time { return cur }
	t.Cleanup(func() { nowFunc = prev })
	return func(d time.Duration) { cur = cur.Add(d) }
}

// TestRefreshStatuses_PausedInstanceSkipsProbe is the #1160 scope guard: while
// instance X's poll is paused (a TUI attached full-screen), a RefreshStatuses
// pass must NOT probe or transition X, while an unpaused sibling Y in the same
// manager IS refreshed as normal. Both carry a dead backend, so if the pause
// were ignored X would flip to Lost; it must stay Running.
func TestRefreshStatuses_PausedInstanceSkipsProbe(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// Dead backends: an unpaused refresh marks these Lost. seedDiskInstance
	// overwrites the per-repo file, so only the last seed persists — we assert
	// in-memory status, which holds both instances independently.
	registerStarted(t, manager, repoID, repoPath, "attached-x", deadTmuxBackend{session.NewFakeBackend()}, true, session.Running)
	registerStarted(t, manager, repoID, repoPath, "sibling-y", deadTmuxBackend{session.NewFakeBackend()}, true, session.Running)

	manager.PauseStatusPoll(repoID, "attached-x")

	manager.RefreshStatuses()

	x := manager.instances[daemonInstanceKey(repoID, "attached-x")]
	if got := x.GetStatus(); got != session.Running {
		t.Fatalf("paused instance status = %v, want Running untouched (its probe must be skipped)", got)
	}
	y := manager.instances[daemonInstanceKey(repoID, "sibling-y")]
	if got := y.GetStatus(); got != session.Lost {
		t.Fatalf("unpaused sibling status = %v, want Lost — the pause must affect ONLY the attached instance", got)
	}
}

// TestRefreshStatuses_PausedDeadInstanceNotMarkedLost guards the #1108
// interaction: a paused instance whose tmux is actually DEAD must not be marked
// Lost while paused — a paused instance is known-attached, not vanished. Real
// death is only surfaced once the pause is cleared/expired (see the lease test).
func TestRefreshStatuses_PausedDeadInstanceNotMarkedLost(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "dead-but-paused", deadTmuxBackend{session.NewFakeBackend()}, true, session.Running)

	manager.PauseStatusPoll(repoID, "dead-but-paused")

	manager.RefreshStatuses()

	inst := manager.instances[daemonInstanceKey(repoID, "dead-but-paused")]
	if got := inst.GetStatus(); got == session.Lost {
		t.Fatalf("paused instance was marked Lost (#1108 interaction) — a paused instance must never be marked Lost while paused")
	}
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("paused instance status = %v, want Running untouched", got)
	}
}

// TestRefreshStatuses_LeaseExpiryDetectsRealDeath is the crash-safety property:
// a pause is lease-bounded, so once statusPollLease elapses a crashed TUI that
// never sent Resume auto-resumes and REAL tmux death is detected — the dead
// instance is marked Lost on the next pass. The clock is advanced past the
// lease deterministically instead of sleeping.
func TestRefreshStatuses_LeaseExpiryDetectsRealDeath(t *testing.T) {
	advance := withFrozenClock(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "crashed-tui", deadTmuxBackend{session.NewFakeBackend()}, true, session.Running)

	manager.PauseStatusPoll(repoID, "crashed-tui")

	// Still within the lease: the dead instance is protected.
	if !manager.isPollPaused(repoID, "crashed-tui") {
		t.Fatal("instance should be paused immediately after PauseStatusPoll")
	}
	manager.RefreshStatuses()
	inst := manager.instances[daemonInstanceKey(repoID, "crashed-tui")]
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("within-lease status = %v, want Running untouched (still paused)", got)
	}

	// Advance past the lease: the crashed TUI never renewed, so the pause must
	// have lapsed and real death must now be detected.
	advance(statusPollLease + time.Second)
	if manager.isPollPaused(repoID, "crashed-tui") {
		t.Fatal("pause must auto-expire after statusPollLease — a crashed TUI can never permanently blind the daemon")
	}
	manager.RefreshStatuses()
	if got := inst.GetStatus(); got != session.Lost {
		t.Fatalf("post-lease status = %v, want Lost — real tmux death must be detected once the pause expires", got)
	}
}

// TestResumeStatusPoll_ClearsPauseImmediately proves a clean detach resumes
// polling without waiting out the lease: right after Resume, isPollPaused is
// false and a subsequent RefreshStatuses probes the (dead) instance and marks
// it Lost.
func TestResumeStatusPoll_ClearsPauseImmediately(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "detached", deadTmuxBackend{session.NewFakeBackend()}, true, session.Running)

	manager.PauseStatusPoll(repoID, "detached")
	if !manager.isPollPaused(repoID, "detached") {
		t.Fatal("instance should be paused after PauseStatusPoll")
	}

	manager.ResumeStatusPoll(repoID, "detached")
	if manager.isPollPaused(repoID, "detached") {
		t.Fatal("ResumeStatusPoll must clear the pause immediately, not wait out the lease")
	}

	manager.RefreshStatuses()
	inst := manager.instances[daemonInstanceKey(repoID, "detached")]
	if got := inst.GetStatus(); got != session.Lost {
		t.Fatalf("status after resume = %v, want Lost — polling must resume immediately on a clean detach", got)
	}
}

// TestControlServer_PauseResumeStatusPoll exercises the RPC handlers directly
// (the reflection-registered surface the TUI calls), proving they delegate to
// the manager: Pause makes isPollPaused true, Resume makes it false, both ack.
func TestControlServer_PauseResumeStatusPoll(t *testing.T) {
	manager, repoID, _ := newStatusTestManager(t)
	server := &controlServer{manager: manager}

	var pauseResp PauseStatusPollResponse
	if err := server.PauseStatusPoll(PauseStatusPollRequest{Title: "s", RepoID: repoID}, &pauseResp); err != nil {
		t.Fatalf("PauseStatusPoll RPC: %v", err)
	}
	if !pauseResp.OK {
		t.Fatal("PauseStatusPoll must ack OK")
	}
	if !manager.isPollPaused(repoID, "s") {
		t.Fatal("PauseStatusPoll RPC must pause the instance via the manager")
	}

	var resumeResp ResumeStatusPollResponse
	if err := server.ResumeStatusPoll(ResumeStatusPollRequest{Title: "s", RepoID: repoID}, &resumeResp); err != nil {
		t.Fatalf("ResumeStatusPoll RPC: %v", err)
	}
	if !resumeResp.OK {
		t.Fatal("ResumeStatusPoll must ack OK")
	}
	if manager.isPollPaused(repoID, "s") {
		t.Fatal("ResumeStatusPoll RPC must clear the pause via the manager")
	}
}
