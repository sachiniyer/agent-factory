package session

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestSaveInstances_KeepsUnknownRuntimeCleanupAlongsideStartedSibling makes an
// unanswerable sandbox teardown a durable retention claim. The row is inert and
// not user-killed, but dropping it from a wholesale checkpoint also drops the
// only identity that can prove the old sandbox gone before reprovisioning.
func TestSaveInstances_KeepsUnknownRuntimeCleanupAlongsideStartedSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	alive := &Instance{Title: "alive", Path: repoPath, started: true, liveness: LiveRunning}
	uncertain := &Instance{
		ID:       "unknown-cleanup-id",
		Title:    "unknown-cleanup",
		Path:     repoPath,
		Program:  "claude",
		started:  false,
		liveness: LiveLost,
		backend: &dockerBackend{
			containerID: "possibly-live-container",
			cleanup: &DockerRuntimeCleanupData{
				ContainerID: "possibly-live-container",
				EngineID:    "engine-id",
			},
		},
		runtimeTeardown: func() error {
			return fmt.Errorf("%w: teardown timed out", ErrWorkspaceStateUnknown)
		},
	}
	if err := uncertain.teardownAfterStartFailure(); !TeardownStateUnknown(err) {
		t.Fatalf("teardown outcome = %v, want unknown", err)
	}

	storage, err := NewStorage(state, "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, uncertain}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	for _, row := range readDisk(t, state, repoPath) {
		if row.Title != uncertain.Title {
			continue
		}
		if row.RuntimeCleanup == nil {
			t.Fatal("retained unknown-cleanup row lost its teardown identity")
		}
		if !row.RuntimeCleanupStateUnknown {
			t.Fatal("retained unknown-cleanup row lost its durable state marker")
		}
		if row.UserKilled {
			t.Fatal("unknown cleanup was collapsed into a user-kill tombstone")
		}
		return
	}
	t.Fatal("daemon checkpoint dropped the unknown-cleanup row and orphaned its possible sandbox")
}

func TestSaveInstances_KeepsArchiveReportAcrossTransientRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	alive := &Instance{Title: "alive", Path: repoPath, started: true, liveness: LiveRunning}
	report := sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: filepath.Join(repoPath, ".af-source-retained"),
		Skipped: []sessiongit.ArchiveSkippedEntry{{
			Path: "private/credential", Reason: sessiongit.ArchiveSkipPermissionDenied,
		}},
	}}}
	makeReported := func(title string, started bool, liveness Liveness, op InFlightOp) *Instance {
		worktree, err := sessiongit.NewGitWorktreeFromStorage(
			repoPath, filepath.Join(repoPath, title+"-archive"), title, "af/"+title, "", false, true,
		)
		if err != nil {
			t.Fatalf("NewGitWorktreeFromStorage(%s): %v", title, err)
		}
		worktree.RestoreArchiveReport(report)
		return &Instance{
			ID: title + "-id", Title: title, Path: repoPath, Program: "claude",
			started: started, liveness: liveness, inFlightOp: op,
			backend: &LocalBackend{}, gitWorktree: worktree,
		}
	}
	deleting := makeReported("deleting-report", true, LiveRunning, OpArchiving)
	notStarted := makeReported("not-started-report", false, LiveLost, OpNone)

	storage, err := NewStorage(state, "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, deleting, notStarted}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	want := map[string]bool{deleting.Title: false, notStarted.Title: false}
	for _, row := range readDisk(t, state, repoPath) {
		if _, ok := want[row.Title]; !ok {
			continue
		}
		if row.ArchiveReport == nil || row.ArchiveReport.Empty() {
			t.Fatalf("retained row %q lost its archive report", row.Title)
		}
		want[row.Title] = true
	}
	for title, kept := range want {
		if !kept {
			t.Fatalf("daemon checkpoint dropped transient row %q while its archive report was the retained tree's only durable handle", title)
		}
	}
}

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

// TestSaveInstances_RestoresReapedLocalTombstone covers the crash window after
// a local kill has successfully torn down its tmux sessions and worktree, but
// deleting the durable kill record fails. teardownKill has cleared the
// gitWorktree pointer, so the shutdown checkpoint legitimately writes an empty
// Worktree payload. The next daemon must still load that tombstone so its poll
// can finish deleting the record; dropping it turns a retryable record-delete
// failure into a permanent ghost.
func TestSaveInstances_RestoresReapedLocalTombstone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	doomed := &Instance{
		ID:       "reaped-local-id",
		Title:    "reaped-local",
		Path:     repoPath,
		Program:  "claude",
		liveness: LiveLost,
		backend:  &LocalBackend{},
		started:  false,
	}
	doomed.MarkUserKilled()

	storage, err := NewStorage(state, "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{doomed}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}
	rows := readDisk(t, state, repoPath)
	if len(rows) != 1 {
		t.Fatalf("SaveInstances persisted %d sessions, want the tombstone", len(rows))
	}
	if rows[0].Worktree.RepoPath != "" || rows[0].Worktree.WorktreePath != "" {
		t.Fatalf("reaped tombstone persisted a worktree: %+v", rows[0].Worktree)
	}

	restored, err := storage.LoadInstances()
	if err != nil {
		t.Fatalf("LoadInstances: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("LoadInstances restored %d sessions, want the retained tombstone", len(restored))
	}
	if !restored[0].UserKilled() {
		t.Fatal("restored local session lost its kill tombstone")
	}
	if restored[0].Started() {
		t.Fatal("reaped tombstone was started and synthesized a new tmux teardown target")
	}
	if got := restored[0].GetWorktreePath(); got != "" {
		t.Fatalf("restored reaped tombstone worktree path = %q, want empty", got)
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

func TestSaveInstances_KeepsRelocationRecoveryAlongsideStartedSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	alive := &Instance{Title: "alive", Path: repoPath, started: true, liveness: LiveRunning}
	gw, err := sessiongit.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(repoPath, "uncertain-wt"), "uncertain", "af/uncertain", "", false, true,
	)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}
	if err := gw.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State: sessiongit.RelocationRecoveryStalled,
	}); err != nil {
		t.Fatalf("RestoreRelocationRecovery: %v", err)
	}
	uncertain := &Instance{
		Title: "uncertain", Path: repoPath, started: false, liveness: LiveRunning,
		backend: &LocalBackend{}, gitWorktree: gw,
	}

	storage, err := NewStorage(state, "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, uncertain}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	for _, row := range readDisk(t, state, repoPath) {
		if row.Title != uncertain.Title {
			continue
		}
		if row.Worktree.RelocationRecovery == nil {
			t.Fatal("retained relocation-recovery row lost its recovery record")
		}
		return
	}
	t.Fatal("daemon checkpoint dropped the inert relocation-recovery row and orphaned its worktree")
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

// A failed off-box recovery can stage a metadata-only tab before Launch while
// leaving the restored instance inert. The staged row is a durability claim:
// if a live sibling causes a whole-repo checkpoint, dropping the inert instance
// also drops the only copy of the web tab before the next recovery can retry.
func TestSaveInstances_KeepsPendingTabsAlongsideStartedSibling(t *testing.T) {
	repoPath := t.TempDir()
	state := newMockStorage()

	alive := makeAliveInstance("alive", repoPath)
	pending, err := FromInstanceData(InstanceData{
		Title: "failed-off-box-recovery", Path: repoPath, BackendType: "ssh", Status: Running,
		PendingTabs: []TabData{{ID: "web-1", Name: "docs", Kind: TabKindWeb, URL: "https://example.com/docs"}},
	})
	if err != nil {
		t.Fatalf("FromInstanceData: %v", err)
	}
	if pending.Started() {
		t.Fatal("a restored off-box row must remain inert until recovery launches it")
	}

	storage, err := NewStorage(state, "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, pending}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	for _, row := range readDisk(t, state, repoPath) {
		if row.Title != pending.Title {
			continue
		}
		if len(row.PendingTabs) != 1 || row.PendingTabs[0].ID != "web-1" {
			t.Fatalf("retained off-box row lost its staged web tab: %+v", row.PendingTabs)
		}
		return
	}
	t.Fatal("whole-repo save dropped the inert off-box row while PendingTabs was its recovery claim")
}
