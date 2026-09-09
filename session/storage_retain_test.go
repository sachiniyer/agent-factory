package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
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
		handoffDeliveryStatus: PromptCouldNotConfirm,
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
			if row.HandoffDeliveryStatus != pending.handoffDeliveryStatus {
				t.Fatalf("pending handoff delivery = %q, want %q", row.HandoffDeliveryStatus, pending.handoffDeliveryStatus)
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

// TestSaveInstances_KeepsLostSandboxRowAlongsideStartedSibling is #3422: the
// retention predicate must know that a lost SANDBOX row is durable.
//
// A non-archived sandbox session (docker/ssh/sandbox/hook) loads INERT on purpose
// — FromInstanceData sets LiveLost and deliberately does NOT call Start(), so the
// status poll and the Lost-restore loop pass it by. That leaves started=false,
// which is exactly the shape the wholesale checkpoint treats as disposable junk.
// It is the opposite of disposable: the container/remote is gone and this record
// is the user's only pointer to the branch the session pushed to origin, so
// dropping it destroys the only handle to real work with no recovery path.
//
// The started sibling is load-bearing: without it the repo has no rows to save
// and the checkpoint is a no-op, so the bug never fires.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the lost sandbox row is absent after the save.
func TestSaveInstances_KeepsLostSandboxRowAlongsideStartedSibling(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			alive := makeAliveInstance("alive", repoPath)
			lost, err := FromInstanceData(InstanceData{
				Title:       "lost-sandbox",
				Path:        repoPath,
				Branch:      "af/lost-sandbox",
				BackendType: backendType,
				Status:      Running,
			})
			if err != nil {
				t.Fatalf("FromInstanceData: %v", err)
			}
			// The inert load contract this fix must NOT disturb: a lost sandbox row
			// stays out of the poll/restore loops via the started=false fence.
			if lost.Started() {
				t.Fatal("a restored sandbox row must load inert — never started")
			}
			if lost.GetStatus() != Lost {
				t.Fatalf("restored sandbox liveness = %v, want Lost", lost.GetStatus())
			}

			storage, err := NewStorage(state, "")
			if err != nil {
				t.Fatalf("NewStorage: %v", err)
			}
			if err := storage.SaveInstances([]*Instance{alive, lost}); err != nil {
				t.Fatalf("SaveInstances: %v", err)
			}

			for _, row := range readDisk(t, state, repoPath) {
				if row.Title != lost.Title {
					continue
				}
				if row.Branch != "af/lost-sandbox" {
					t.Fatalf("retained lost sandbox row lost its branch pointer: %q", row.Branch)
				}
				if row.BackendType != backendType {
					t.Fatalf("retained row backend type = %q, want %q", row.BackendType, backendType)
				}
				return
			}
			t.Fatalf("the daemon's shutdown checkpoint dropped a lost %s session: its branch is "+
				"pushed to origin and this record was the user's only handle to it (#3422)", backendType)
		})
	}
}

// TestSaveInstances_KeepsMidArchiveSandboxRowAlongsideStartedSibling is the
// off-box mid-archive wholesale-checkpoint gap: an off-box session (docker/ssh/
// sandbox/hook) caught mid-archive at the daemon shutdown checkpoint has
// already pushed its branch to origin and recorded it on i.Branch, but
// BeginArchive raised OpArchiving while leaving liveness LiveRunning until
// CommitArchive committed the archive (composeStatus(LiveRunning, OpArchiving)
// is Deleting). The off-box row has no daemon-side gitWorktree, so the LOCAL
// mid-archive retention claim (archiveReportPending, sourced only from a live
// worktree's ProjectionSnapshot) never fires for it; lostSandbox requires
// LiveLost and a mid-archive row is still LiveRunning. The (Loading/Deleting)
// skip then silently drops the row, and with an alive same-repo sibling the
// per-repo OVERWRITE erases its only af-side handle to the pushed branch —
// the exact obligation lostSandboxRecord exists for (#3422), reached at a
// different op. With the pendingArchiveSandbox claim the mid-archive row is
// retained: the next restart restores from the pushed branch instead of
// stranding the work on origin behind a lost handle.
//
// The started sibling is load-bearing: without it the repo has no rows to save
// and the checkpoint is a no-op, so the bug never fires.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the mid-archive sandbox row is absent
// after the save for docker/ssh/sandbox/remote.
func TestSaveInstances_KeepsMidArchiveSandboxRowAlongsideStartedSibling(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			alive := makeAliveInstance("alive", repoPath)
			// The exact mid-archive off-box shape: branch pushed and recorded on
			// i.Branch, OpArchiving raised, liveness still LiveRunning, CommitArchive
			// not yet run. started=true because step 4 of ArchiveSandbox (the
			// started=false flip) runs AFTER as.Kill returns, which is the second
			// half of the window; this captures the first half, where the daemon
			// has not even reaped the sandbox yet. The inert sandbox backend is
			// only a Type()/Capabilities() carrier — its runtime has no live handle
			// on the daemon side, exactly like the loaded lost-sandbox shape.
			midArchive := &Instance{
				ID:         "mid-archive-id",
				Title:      "mid-archive",
				Path:       repoPath,
				Branch:     "af/mid-archive",
				Program:    "claude",
				started:    true,
				liveness:   LiveRunning,
				inFlightOp: OpArchiving,
				backend:    newInertSandboxBackend(backendType),
			}
			if got := midArchive.backend.Type(); got != backendType {
				t.Fatalf("mid-archive backend Type() = %q, want %q", got, backendType)
			}

			storage, err := NewStorage(state, "")
			if err != nil {
				t.Fatalf("NewStorage: %v", err)
			}
			if err := storage.SaveInstances([]*Instance{alive, midArchive}); err != nil {
				t.Fatalf("SaveInstances: %v", err)
			}

			for _, row := range readDisk(t, state, repoPath) {
				if row.Title != midArchive.Title {
					continue
				}
				if row.Branch != "af/mid-archive" {
					t.Fatalf("retained mid-archive %s row lost its pushed-branch pointer: %q (without it the next restore re-clones the repo DEFAULT branch)", backendType, row.Branch)
				}
				if row.BackendType != backendType {
					t.Fatalf("retained mid-archive row backend type = %q, want %q", row.BackendType, backendType)
				}
				return
			}
			t.Fatalf("daemon checkpoint dropped the mid-archive %s session and "+
				"erased the only af-side handle to its pushed origin branch (its record "+
				"was the only pointer, exactly the obligation lostSandboxRecord exists for)",
				backendType)
		})
	}
}

// TestSaveInstances_NoSibling_KeepsMidArchiveRowReplacingStale documents the
// no-sibling failure mode of the off-box mid-archive gap covered by the
// pendingArchiveSandbox retention claim. When the mid-archive session is the
// ONLY in-memory row for its repo, the wholesale checkpoint leaves the repo's
// instances.json UNREWRITTEN without the claim (grouped[rid] is built AFTER the
// Loading/Deleting skip's continue fires), so a stale pre-archive on-disk row
// the daemon held BEFORE the branch push returns survives untouched —
// typically with Branch="" (the daemon's i.Branch is empty until
// ArchiveSandbox records the pushed branch). On restart the dead sandbox
// resolves Lost and the Lost re-provision fetches the repo DEFAULT branch
// because RestoreBranch is empty, a "successful" recovery that silently
// strands the pushed work onto the wrong branch (session/archive_sandbox.go).
// With the pendingArchiveSandbox claim the mid-archive row becomes the
// retained row and the per-repo overwrite REPLACES the stale row with the
// pushed-branch pointer.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the stale pre-archive row survives and the
// mid-archive row with its pushed branch is absent after the save.
func TestSaveInstances_NoSibling_KeepsMidArchiveRowReplacingStale(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()
			// The stale pre-archive on-disk row: LiveRunning with an empty Branch
			// pointer, the state the daemon snapshot persisted before the push
			// returned. After a restart that loses the in-memory mid-archive row,
			// restore re-provisions from this empty Branch — i.e. from the repo
			// DEFAULT branch — and silently strands the pushed work.
			seedDisk(t, state, repoPath, []InstanceData{{
				Title:       "mid-archive",
				Path:        repoPath,
				Branch:      "",
				Program:     "claude",
				Status:      Running,
				Liveness:    LiveRunning,
				BackendType: backendType,
			}})

			// The in-memory row now in the mid-archive window: branch pushed and
			// recorded, OpArchiving raised, CommitArchive not yet run.
			midArchive := &Instance{
				ID:         "mid-archive-id",
				Title:      "mid-archive",
				Path:       repoPath,
				Branch:     "af/mid-archive",
				Program:    "claude",
				started:    true,
				liveness:   LiveRunning,
				inFlightOp: OpArchiving,
				backend:    newInertSandboxBackend(backendType),
			}

			storage, err := NewStorage(state, "")
			if err != nil {
				t.Fatalf("NewStorage: %v", err)
			}
			if err := storage.SaveInstances([]*Instance{midArchive}); err != nil {
				t.Fatalf("SaveInstances: %v", err)
			}

			rows := readDisk(t, state, repoPath)
			if len(rows) != 1 {
				t.Fatalf("checkpoint persisted %d rows, want the mid-archive row replacing the stale pre-archive row (no stale Branch=\"\" survivor)", len(rows))
			}
			row := rows[0]
			if row.Title != midArchive.Title {
				t.Fatalf("persisted row title = %q, want %q (the stale pre-archive row survived untouched instead of being overwritten)", row.Title, midArchive.Title)
			}
			if row.Branch != "af/mid-archive" {
				t.Fatalf("mid-archive %s row persisted with stale Branch %q; want the pushed %q (the disaster case is the empty Branch the daemon held before the push that re-provisions onto the repo DEFAULT branch after restart)", backendType, row.Branch, "af/mid-archive")
			}
			if row.BackendType != backendType {
				t.Fatalf("retained mid-archive row backend type = %q, want %q", row.BackendType, backendType)
			}
		})
	}
}

// TestSaveInstances_StillPrunesDisposableSandboxRows is the other half of #3422:
// making a lost sandbox durable must not degrade into "never prune anything".
// Both rows below are sandbox-backed and !Started, and both must still be
// dropped — a never-started in-memory row has no persisted workspace behind it,
// and a create still in flight is the transient junk the Loading skip exists for.
func TestSaveInstances_StillPrunesDisposableSandboxRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	alive := makeAliveInstance("alive", repoPath)
	neverStarted := &Instance{
		ID: "never-started-id", Title: "never-started", Path: repoPath, Program: "claude",
		started: false, liveness: LiveReady, backend: &dockerBackend{},
	}
	creating := &Instance{
		ID: "creating-id", Title: "creating", Path: repoPath, Program: "claude",
		started: false, liveness: LiveReady, inFlightOp: OpCreating, backend: &dockerBackend{},
	}
	// Same treatment for a local row that never started: unchanged by this fix.
	localJunk := makeInstance("local-junk", repoPath, false)

	storage, err := NewStorage(state, "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, neverStarted, creating, localJunk}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	rows := readDisk(t, state, repoPath)
	for _, row := range rows {
		if row.Title != alive.Title {
			t.Fatalf("checkpoint retained disposable row %q — the #3422 retention widened "+
				"into a blanket keep-everything", row.Title)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("checkpoint persisted %d rows, want only the live sibling", len(rows))
	}
}

// TestSaveInstances_RetainedNonGitAliasScope reproduces macOS's symlinked temp
// root on every OS and keeps checkpoint identity separate from display identity.
func TestSaveInstances_RetainedNonGitAliasScope(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	root := testguard.CanonicalTempDir(t)
	alias := filepath.Join(testguard.CanonicalTempDir(t), "alias")
	require.NoError(t, os.Symlink(root, alias))
	rawID, resolvedID := config.RepoIDFromRoot(alias), config.RepoIDFromRoot(root)
	require.NotEqual(t, rawID, resolvedID)
	require.Equal(t, resolvedID, config.RepoIDForPath(alias), "display aliases normalize")
	for _, spelling := range []string{root, alias} {
		for _, binding := range []struct{ name, pinned, want string }{
			{"fresh-historical", "", config.RepoIDFromRoot(spelling)},
			{"pinned-raw", rawID, rawID},
			{"pinned-resolved", resolvedID, resolvedID},
		} {
			t.Run(filepath.Base(spelling)+"/"+binding.name, func(t *testing.T) {
				state := newMockStorage()
				alive := &Instance{ID: "alive-id", Title: "alive", Path: spelling, started: true, liveness: LiveRunning}
				retained := &Instance{ID: "retained-id", Title: "retained", Path: spelling, liveness: LiveLost, started: false}
				retained.MarkUserKilled()
				alive.PinStorageRepoID(binding.pinned)
				retained.PinStorageRepoID(binding.pinned)
				storage, err := NewStorage(state, "")
				require.NoError(t, err)
				require.NoError(t, storage.SaveInstances([]*Instance{alive, retained}))
				var rows []InstanceData
				require.Contains(t, state.data, binding.want, "checkpoint must preserve its historical or retained scope")
				require.NoError(t, json.Unmarshal(state.data[binding.want], &rows))
				require.Len(t, rows, 2, "retained row must remain beside its live sibling")
				require.Len(t, state.data, 1, "no second identity bucket may be invented")
				require.Equal(t, "retained", rows[1].Title)
				require.True(t, rows[1].UserKilled)
			})
		}
	}
	// Loaded rows keep their actual containing key even after the alias vanishes.
	require.NoError(t, os.Remove(alias))
	for _, pinned := range []string{rawID, resolvedID} {
		state := newMockStorage()
		data, err := json.Marshal([]InstanceData{{ID: "loaded-id", Title: "loaded", Path: alias, Program: "claude", UserKilled: true, Liveness: LiveLost}})
		require.NoError(t, err)
		state.data[pinned] = data
		storage, err := NewStorage(state, "")
		require.NoError(t, err)
		loaded, err := storage.LoadInstances()
		require.NoError(t, err)
		require.Len(t, loaded, 1)
		require.NoError(t, storage.SaveInstances(loaded))
		require.Len(t, state.data, 1)
		require.Contains(t, state.data, pinned)
	}
}
