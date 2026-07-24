package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// registerTaskRun seeds a started, task-run-active instance — the only shape
// that arms taskRunProbeDue. taskRunActive is derived from TaskID (#1892), so a
// non-empty TaskID is what distinguishes this from registerStarted's user
// session. The instance still carries a stable ID (remoteLossKey's basis), which
// is exactly the key the backstop timer is stored under.
func registerTaskRun(t *testing.T, m *Manager, repoID, repoPath, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: title, Path: repoPath, Program: "claude", TaskID: "task-" + title,
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// probeDueLen reads taskRunProbeDue's size under its guarding lock, so the
// assertions never race the poll goroutine's writes.
func probeDueLen(m *Manager) int {
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	return len(m.taskRunProbeDue)
}

// TestResumeStatusPoll_FreesTaskRunProbeDueEntry is the #2015 fail-first: a clean
// detach must FREE the backstop-timer entry an attach armed. The map is written
// by taskRunBackstopDue under remoteLossKey (the stable instance ID), but the
// buggy ResumeStatusPoll deletes under daemonInstanceKey(repoID,title) — keys
// that never coincide for an ID-bearing session — so the delete is a no-op and
// the entry leaks. The map must return to its pre-attach baseline of 0.
func TestResumeStatusPoll_FreesTaskRunProbeDueEntry(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskRun(t, manager, repoID, repoPath, "attached-run")

	if inst.ID == "" {
		t.Fatal("precondition: a task-run instance must carry a stable ID (remoteLossKey's basis)")
	}
	if !inst.TaskRunActive() {
		t.Fatal("precondition: instance must be task-run-active to arm the backstop")
	}
	if n := probeDueLen(manager); n != 0 {
		t.Fatalf("baseline taskRunProbeDue size = %d, want 0", n)
	}

	// Attach: pause the poll, then one refresh ARMS the backstop timer (the first
	// paused tick only arms; see taskRunBackstopDue).
	manager.PauseStatusPoll(repoID, "attached-run", "")
	manager.RefreshStatuses()
	if n := probeDueLen(manager); n != 1 {
		t.Fatalf("after arming, taskRunProbeDue size = %d, want 1 (a paused task run must arm its backstop)", n)
	}

	// Detach: a clean ResumeStatusPoll must reclaim the armed entry.
	manager.ResumeStatusPoll(repoID, "attached-run", "")
	if n := probeDueLen(manager); n != 0 {
		t.Fatalf("after clean detach, taskRunProbeDue size = %d, want 0 — the entry leaked (#2015: cleanup keyed by the wrong function)", n)
	}
}

// TestRefreshStatuses_SweepsLapsedTaskRunProbeDue guards the crashed-TUI path: a
// TUI that never sends Resume leaves the pause to lapse on its lease. There is no
// Resume to reclaim the armed backstop entry, so the poll's own GC must — once
// the lease has expired the entry is dead weight and the map must shrink back to
// 0 on the next pass.
func TestRefreshStatuses_SweepsLapsedTaskRunProbeDue(t *testing.T) {
	advance := withFrozenClock(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	registerTaskRun(t, manager, repoID, repoPath, "crashed-attach")

	manager.PauseStatusPoll(repoID, "crashed-attach", "")
	manager.RefreshStatuses() // arms
	if n := probeDueLen(manager); n != 1 {
		t.Fatalf("after arming, taskRunProbeDue size = %d, want 1", n)
	}

	// The lease lapses with no Resume; the next poll must reclaim the orphan.
	advance(statusPollLease + time.Second)
	manager.RefreshStatuses()
	if n := probeDueLen(manager); n != 0 {
		t.Fatalf("after lease lapse, taskRunProbeDue size = %d, want 0 — a crashed attach must not leak its backstop entry (#2015)", n)
	}
}

// TestRefreshStatuses_SweepsTornDownTaskRunProbeDue guards the teardown path: a
// session killed or archived while attached leaves m.instances without ever
// sending Resume. Its backstop entry (keyed by the now-dead instance ID) can
// never be reached again, so the poll's GC must reclaim it.
func TestRefreshStatuses_SweepsTornDownTaskRunProbeDue(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerTaskRun(t, manager, repoID, repoPath, "torn-down")

	manager.PauseStatusPoll(repoID, "torn-down", "")
	manager.RefreshStatuses() // arms
	if n := probeDueLen(manager); n != 1 {
		t.Fatalf("after arming, taskRunProbeDue size = %d, want 1", n)
	}

	// The session is killed/archived out from under the pause: its record leaves
	// m.instances and Resume never fires.
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, "torn-down"))
	manager.mu.Unlock()

	manager.RefreshStatuses()
	if n := probeDueLen(manager); n != 0 {
		t.Fatalf("after teardown, taskRunProbeDue size = %d, want 0 — a killed/archived session must not leak its backstop entry (#2015)", n)
	}
}
