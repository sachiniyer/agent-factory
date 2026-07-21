package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// TestSaveInstances_KeepsTombstonedRowAlongsideStartedSibling is #1917 round-5
// finding (1): the retain is undone by a writer in another layer.
//
// A kill clears started BEFORE teardown, so a teardown that could not complete
// safely leaves the record started=false, not Archived, workspace still on disk,
// and deliberately RETAINED as its only handle. SaveInstances — the daemon's
// shutdown checkpoint — rewrites the whole repo list from the started instances,
// and fires whenever ANY other started session in the repo is saved. It used to
// drop the retained row, orphaning the exact workspace the retention exists to
// protect.
//
// The started sibling is load-bearing: without it the repo has no rows to save and
// the checkpoint is a no-op, so the bug never fires.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the tombstoned row is absent after the save.
func TestSaveInstances_KeepsTombstonedRowAlongsideStartedSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()

	// A live session in the repo: its save is what triggers the wholesale rewrite.
	alive := &Instance{Title: "alive", Path: repoPath, started: true}
	alive.SetStatusForTest(Running)

	// The retained row: kill cleared started, the teardown could not finish, and the
	// record is the only pointer left at its workspace.
	doomed := &Instance{Title: "doomed", Path: repoPath, started: false}
	doomed.SetStatusForTest(Running)
	doomed.MarkUserKilled()

	storage, err := NewStorage(config.LoadState(), "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, doomed}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	repoID := config.RepoIDFromRoot(repoPath)
	scoped, err := NewStorage(config.LoadState(), repoID)
	if err != nil {
		t.Fatalf("NewStorage(scoped): %v", err)
	}
	rows, err := scoped.LoadInstanceData()
	if err != nil {
		t.Fatalf("LoadInstanceData: %v", err)
	}
	var kept bool
	for _, r := range rows {
		if r.Title == "doomed" {
			kept = true
			if !r.UserKilled {
				t.Fatal("the retained row lost its tombstone: the next daemon would treat it as Lost and RESTORE it")
			}
		}
	}
	if !kept {
		t.Fatal("the daemon's checkpoint silently dropped a RETAINED tombstoned row: its workspace " +
			"is still on disk and this record was its only handle, so the retention that KillSession " +
			"and finishUserKill deliberately performed is undone by a writer in another layer (#1917 round 5)")
	}
}

// TestSaveInstances_KeepsStartupUnknownRowAlongsideStartedSibling applies the
// same retention rule to #2207's inert startup record. It has no kill tombstone
// by design, so StartupStateUnknown must independently keep a wholesale storage
// checkpoint from orphaning its workspace.
func TestSaveInstances_KeepsStartupUnknownRowAlongsideStartedSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()

	alive := &Instance{Title: "alive", Path: repoPath, started: true}
	alive.SetStatusForTest(Running)

	uncertain := &Instance{Title: "uncertain", Path: repoPath, started: true}
	uncertain.SetStatusForTest(Running)
	uncertain.MarkStartupStateUnknown()

	storage, err := NewStorage(config.LoadState(), "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, uncertain}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	repoID := config.RepoIDFromRoot(repoPath)
	scoped, err := NewStorage(config.LoadState(), repoID)
	if err != nil {
		t.Fatalf("NewStorage(scoped): %v", err)
	}
	rows, err := scoped.LoadInstanceData()
	if err != nil {
		t.Fatalf("LoadInstanceData: %v", err)
	}
	for _, row := range rows {
		if row.Title != "uncertain" {
			continue
		}
		if !row.StartupStateUnknown {
			t.Fatal("the retained startup-unknown row lost its durable safety marker")
		}
		if row.UserKilled {
			t.Fatal("the startup-unknown row became an automatic-cleanup tombstone")
		}
		return
	}
	t.Fatal("the daemon checkpoint dropped an inert startup-unknown row and orphaned its workspace")
}

func TestSaveInstances_KeepsPendingHandoffAlongsideStartedSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()

	alive := &Instance{Title: "alive", Path: repoPath, started: true}
	alive.SetStatusForTest(Running)

	pending := &Instance{
		Title: "pending-handoff", Path: repoPath, started: true,
		liveness: LiveRunning, inFlightOp: OpReplacing,
		pendingHandoffMission: "continue the exact inherited work",
	}

	storage, err := NewStorage(config.LoadState(), "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, pending}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	repoID := config.RepoIDFromRoot(repoPath)
	scoped, err := NewStorage(config.LoadState(), repoID)
	if err != nil {
		t.Fatalf("NewStorage(scoped): %v", err)
	}
	rows, err := scoped.LoadInstanceData()
	if err != nil {
		t.Fatalf("LoadInstanceData: %v", err)
	}
	for _, row := range rows {
		if row.Title == pending.Title {
			if row.PendingHandoffMission != pending.pendingHandoffMission {
				t.Fatalf("pending handoff mission = %q, want %q", row.PendingHandoffMission, pending.pendingHandoffMission)
			}
			return
		}
	}
	t.Fatal("whole-repo save dropped the pending handoff row while its incoming runtime was live")
}
