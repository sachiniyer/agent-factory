package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
				ID:                   "mid-archive-id",
				Title:                "mid-archive",
				Path:                 repoPath,
				Branch:               "af/mid-archive",
				Program:              "claude",
				started:              true,
				liveness:             LiveRunning,
				inFlightOp:           OpArchiving,
				archivePushCompleted: true,
				backend:              newInertSandboxBackend(backendType),
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
				ID:                   "mid-archive-id",
				Title:                "mid-archive",
				Path:                 repoPath,
				Branch:               "af/mid-archive",
				Program:              "claude",
				started:              true,
				liveness:             LiveRunning,
				inFlightOp:           OpArchiving,
				archivePushCompleted: true,
				backend:              newInertSandboxBackend(backendType),
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

// TestSaveInstances_MidArchiveSandboxUseDiskVersionIfArchiveCommitted covers
// the interleaving where the shutdown checkpoint snapshots a mid-archive sandbox
// row BEFORE ArchiveSandbox has written i.Branch, but a targeted writer
// (persistInstanceData / CommitArchive) commits the correct Archived state to
// disk BEFORE SaveInstances acquires the file lock. The checkpoint must prefer
// the durable disk version (with Branch set and Liveness=LiveArchived) over its
// stale pre-Branch in-memory snapshot; otherwise the wholesale overwrite
// regresses the just-committed archive back to an empty-Branch row and the next
// restore re-provisions from the repo default branch.
//
// The scenario is exercised by pre-seeding disk with the committed Archived row
// (simulating the targeted writer) and then calling SaveInstances with the
// stale in-memory mid-archive snapshot.
func TestSaveInstances_MidArchiveSandboxUseDiskVersionIfArchiveCommitted(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			// Pre-seed disk with the committed Archived row — this simulates
			// persistInstanceData / CommitArchive having already run under the
			// file lock before SaveInstances acquires it.
			const pushedBranch = "af/mid-archive"
			snapshotTime := time.Now().Add(-time.Second)
			seedDisk(t, state, repoPath, []InstanceData{{
				ID:          "mid-archive-id",
				Title:       "mid-archive",
				Path:        repoPath,
				Branch:      pushedBranch,
				Program:     "claude",
				Status:      Archived,
				Liveness:    LiveArchived,
				BackendType: backendType,
				UpdatedAt:   snapshotTime.Add(time.Second),
			}})

			alive := makeAliveInstance("alive", repoPath)
			// Stale in-memory snapshot: OpArchiving raised, Branch not yet set —
			// the exact shape the checkpoint sees when it wakes between
			// BeginArchive and ArchiveSandbox writing i.Branch.
			midArchive := &Instance{
				ID:         "mid-archive-id",
				Title:      "mid-archive",
				Path:       repoPath,
				Branch:     "",
				Program:    "claude",
				started:    true,
				liveness:   LiveRunning,
				inFlightOp: OpArchiving,
				backend:    newInertSandboxBackend(backendType),
				UpdatedAt:  snapshotTime,
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
				if row.Branch != pushedBranch {
					t.Fatalf("checkpoint overwrote the committed archive: disk Branch = %q, want %q "+
						"(the stale pre-Branch snapshot must never clobber the committed row)", row.Branch, pushedBranch)
				}
				if row.Liveness != LiveArchived {
					t.Fatalf("checkpoint reverted committed archive: Liveness = %v, want LiveArchived", row.Liveness)
				}
				return
			}
			t.Fatalf("mid-archive row missing after checkpoint")
		})
	}
}

// TestSaveInstances_MidArchiveSandboxPreservesRuntimeCleanupIdentity covers the
// second P1 finding: when the shutdown checkpoint retains a mid-archive sandbox
// row, ForStorage must preserve the backend's staged RuntimeCleanup identity.
// Without this, the !UserKilled && !RuntimeCleanupStateUnknown branch in
// ForStorage strips RuntimeCleanup, and after a restart the daemon has no handle
// to prove the old sandbox gone before reprovisioning — a second container or
// remote workspace could be provisioned while the original remains alive.
//
// The fix marks RuntimeCleanupStateUnknown on any pendingArchiveSandbox row
// before calling ForStorage, so the teardown identity survives to disk.
func TestSaveInstances_MidArchiveSandboxPreservesRuntimeCleanupIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	alive := makeAliveInstance("alive", repoPath)

	// A mid-archive docker session with a live cleanup identity: the backend
	// knows the container ID and engine ID that must survive to disk so a
	// restarted daemon can prove the container gone before re-provisioning.
	const containerID = "abc123container"
	const engineID = "engine-xyz"
	midArchive := &Instance{
		ID:                   "mid-archive-docker-id",
		Title:                "mid-archive-docker",
		Path:                 repoPath,
		Branch:               "af/mid-archive-docker",
		Program:              "claude",
		started:              true,
		liveness:             LiveRunning,
		inFlightOp:           OpArchiving,
		archivePushCompleted: true,
		backend: &dockerBackend{
			containerID: containerID,
			cleanup: &DockerRuntimeCleanupData{
				ContainerID: containerID,
				EngineID:    engineID,
			},
		},
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
		if !row.RuntimeCleanupStateUnknown {
			t.Fatal("mid-archive sandbox row lost its RuntimeCleanupStateUnknown marker: " +
				"the next restart has no way to know the sandbox teardown was in flight and " +
				"may provision a second container while the original is still alive")
		}
		if row.RuntimeCleanup == nil {
			t.Fatal("mid-archive sandbox row lost its RuntimeCleanup identity: " +
				"the next restart cannot prove the old container gone before re-provisioning")
		}
		if row.RuntimeCleanup.Docker == nil {
			t.Fatalf("RuntimeCleanup missing Docker identity: %+v", row.RuntimeCleanup)
		}
		if row.RuntimeCleanup.Docker.ContainerID != containerID {
			t.Fatalf("RuntimeCleanup.Docker.ContainerID = %q, want %q", row.RuntimeCleanup.Docker.ContainerID, containerID)
		}
		if row.RuntimeCleanup.Docker.EngineID != engineID {
			t.Fatalf("RuntimeCleanup.Docker.EngineID = %q, want %q", row.RuntimeCleanup.Docker.EngineID, engineID)
		}
		return
	}
	t.Fatal("mid-archive sandbox row was dropped from the checkpoint")
}

// TestSaveInstances_PrePushArchiveSandboxPreservesCommittedDiskRow covers
// finding 3966202344 / 3966405841: a sandbox snapshot taken between BeginArchive
// and the push returning (Branch still empty) must NOT use RuntimeCleanupState
// Unknown to retain, and a committed LiveArchived disk row present when the file
// lock is acquired must survive the wholesale write.
func TestSaveInstances_PrePushArchiveSandboxPreservesCommittedDiskRow(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			const pushedBranch = "af/pre-push-archive"
			commitTime := time.Now()
			// Committed disk row: archive succeeded while the checkpoint was in
			// the pre-push window (Branch was empty in memory at snapshot time).
			seedDisk(t, state, repoPath, []InstanceData{{
				ID:          "pre-push-id",
				Title:       "pre-push-archive",
				Path:        repoPath,
				Branch:      pushedBranch,
				Program:     "claude",
				Status:      Archived,
				Liveness:    LiveArchived,
				BackendType: backendType,
				UpdatedAt:   commitTime,
			}})

			alive := makeAliveInstance("alive", repoPath)
			// Pre-push in-memory snapshot: Branch empty, OpArchiving set.
			prePush := &Instance{
				ID:         "pre-push-id",
				Title:      "pre-push-archive",
				Path:       repoPath,
				Branch:     "",
				Program:    "claude",
				started:    true,
				liveness:   LiveRunning,
				inFlightOp: OpArchiving,
				backend:    newInertSandboxBackend(backendType),
				UpdatedAt:  commitTime.Add(-time.Second),
			}

			storage, err := NewStorage(state, "")
			if err != nil {
				t.Fatalf("NewStorage: %v", err)
			}
			if err := storage.SaveInstances([]*Instance{alive, prePush}); err != nil {
				t.Fatalf("SaveInstances: %v", err)
			}

			for _, row := range readDisk(t, state, repoPath) {
				if row.Title != prePush.Title {
					continue
				}
				if row.Branch != pushedBranch {
					t.Fatalf("checkpoint overwrote the committed archive: Branch = %q, want %q "+
						"(pre-push snapshot must not clobber a just-committed LiveArchived row)", row.Branch, pushedBranch)
				}
				if row.Liveness != LiveArchived {
					t.Fatalf("Liveness = %v, want LiveArchived", row.Liveness)
				}
				return
			}
			t.Fatalf("committed archive row for %s was erased by the checkpoint", backendType)
		})
	}
}

// TestSaveInstances_PrePushDropsRowWithoutCleanupMarker verifies the safety
// half of finding 3966202344: a pre-push row (Branch empty) that the archive
// does NOT commit to disk before the lock is simply dropped — it must not be
// retained with RuntimeCleanupStateUnknown, which would cause a restart to
// reap the live sandbox.
func TestSaveInstances_PrePushDropsRowWithoutCleanupMarker(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			alive := makeAliveInstance("alive", repoPath)
			prePush := &Instance{
				ID:         "pre-push-id",
				Title:      "pre-push",
				Path:       repoPath,
				Branch:     "",
				Program:    "claude",
				started:    true,
				liveness:   LiveRunning,
				inFlightOp: OpArchiving,
				backend: &dockerBackend{
					containerID: "live-container",
					cleanup: &DockerRuntimeCleanupData{
						ContainerID: "live-container",
						EngineID:    "engine-id",
					},
				},
			}

			storage, err := NewStorage(state, "")
			if err != nil {
				t.Fatalf("NewStorage: %v", err)
			}
			if err := storage.SaveInstances([]*Instance{alive, prePush}); err != nil {
				t.Fatalf("SaveInstances: %v", err)
			}

			for _, row := range readDisk(t, state, repoPath) {
				if row.Title == prePush.Title {
					t.Fatalf("pre-push row must be dropped; retaining with RuntimeCleanupStateUnknown "+
						"would allow a restart to reap the live sandbox mid-push (%s)", backendType)
				}
			}
		})
	}
}

// TestSaveInstances_ReconcileUsesAbortArchiveOutcome covers finding 3966202352:
// when archiveRemoteSession runs AbortArchiveToLost (push succeeded, teardown
// failed), the committed disk row has Liveness=LiveLost with a non-empty Branch.
// reconcilePendingArchiveRows must prefer that durable outcome over the stale
// in-memory pre-Branch snapshot, not only LiveArchived.
func TestSaveInstances_ReconcileUsesAbortArchiveOutcome(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			const pushedBranch = "af/abort-archive"
			snapshotTime := time.Now().Add(-time.Second)
			commitTime := time.Now()

			// Disk: AbortArchiveToLost row — push succeeded, teardown failed.
			seedDisk(t, state, repoPath, []InstanceData{{
				ID:          "abort-archive-id",
				Title:       "abort-archive",
				Path:        repoPath,
				Branch:      pushedBranch,
				Program:     "claude",
				Status:      Lost,
				Liveness:    LiveLost,
				BackendType: backendType,
				UpdatedAt:   commitTime,
			}})

			alive := makeAliveInstance("alive", repoPath)
			// Stale snapshot: Branch already set (post-push), but snapshot was
			// taken before AbortArchiveToLost ran.
			midArchive := &Instance{
				ID:                   "abort-archive-id",
				Title:                "abort-archive",
				Path:                 repoPath,
				Branch:               pushedBranch,
				Program:              "claude",
				started:              true,
				liveness:             LiveRunning,
				inFlightOp:           OpArchiving,
				archivePushCompleted: true,
				backend:              newInertSandboxBackend(backendType),
				UpdatedAt:            snapshotTime,
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
				if row.Liveness != LiveLost {
					t.Fatalf("committed AbortArchiveToLost outcome overwritten: Liveness = %v, want LiveLost", row.Liveness)
				}
				if row.Branch != pushedBranch {
					t.Fatalf("Branch = %q, want %q (pushed work must survive", row.Branch, pushedBranch)
				}
				return
			}
			t.Fatalf("abort-archive row missing after checkpoint for %s", backendType)
		})
	}
}

// TestSaveInstances_ReconcileRequiresFresherUpdateAt covers finding 3966293189:
// reconcilePendingArchiveRows must only adopt a disk row whose UpdatedAt is
// strictly after the snapshot's. A stale Archived disk row from a prior archive
// cycle must not displace a fresh sandbox snapshot.
func TestSaveInstances_ReconcileRequiresFresherUpdateAt(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			state := newMockStorage()

			// Disk: stale Archived row from a PREVIOUS archive cycle, same key,
			// but UpdatedAt is BEFORE the snapshot.
			snapshotTime := time.Now()
			priorArchiveTime := snapshotTime.Add(-time.Second)
			const staleBranch = "af/prior-archive"
			const freshBranch = "af/fresh-archive"
			seedDisk(t, state, repoPath, []InstanceData{{
				ID:          "reused-id",
				Title:       "reused-session",
				Path:        repoPath,
				Branch:      staleBranch,
				Program:     "claude",
				Status:      Archived,
				Liveness:    LiveArchived,
				BackendType: backendType,
				UpdatedAt:   priorArchiveTime,
			}})

			alive := makeAliveInstance("alive", repoPath)
			// Fresh snapshot: branch pushed, archive in flight.
			midArchive := &Instance{
				ID:                   "reused-id",
				Title:                "reused-session",
				Path:                 repoPath,
				Branch:               freshBranch,
				Program:              "claude",
				started:              true,
				liveness:             LiveRunning,
				inFlightOp:           OpArchiving,
				archivePushCompleted: true,
				backend:              newInertSandboxBackend(backendType),
				UpdatedAt:            snapshotTime,
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
				// The stale Archived row must NOT displace the fresh snapshot.
				if row.Liveness == LiveArchived && row.Branch == staleBranch {
					t.Fatalf("stale Archived row from a prior archive cycle replaced the fresh snapshot: "+
						"reconciliation adopted a disk row with UpdatedAt before the snapshot (%s)", backendType)
				}
				return
			}
		})
	}
}

// TestSaveInstances_PostTeardownNoRuntimeCleanupUnknown covers finding 3966293181:
// when ArchiveSandbox has already completed teardown (as.Kill returned and cleared
// the backend's cleanup handle), the snapshot has runtimeCleanup == nil. The
// checkpoint must NOT set RuntimeCleanupStateUnknown in that case, because a
// restart would then re-run cleanup on a backend that is already gone.
func TestSaveInstances_PostTeardownNoRuntimeCleanupUnknown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()

	alive := makeAliveInstance("alive", repoPath)

	// Post-teardown snapshot: Branch set (push completed), OpArchiving set (commit
	// not yet run), but backend has no live cleanup handle (teardown is done).
	postTeardown := &Instance{
		ID:                   "post-teardown-id",
		Title:                "post-teardown",
		Path:                 repoPath,
		Branch:               "af/post-teardown",
		Program:              "claude",
		started:              true,
		liveness:             LiveRunning,
		inFlightOp:           OpArchiving,
		archivePushCompleted: true,
		// dockerBackend with nil cleanup: teardown completed, no identity to retain.
		backend: &dockerBackend{containerID: ""},
	}

	storage, err := NewStorage(state, "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, postTeardown}); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}

	for _, row := range readDisk(t, state, repoPath) {
		if row.Title != postTeardown.Title {
			continue
		}
		if row.RuntimeCleanupStateUnknown {
			t.Fatal("post-teardown row must not have RuntimeCleanupStateUnknown: teardown already completed " +
				"(runtimeCleanup nil), so marking unknown would cause a restart to attempt cleanup " +
				"on an already-gone backend")
		}
		return
	}
	t.Fatal("post-teardown row was not retained")
}

// failOnGetInstancesMockStorage wraps mockInstanceStorage and returns an error
// on GetInstances for a specific repo, simulating a transient read failure
// during reconciliation.
type failOnGetInstancesMockStorage struct {
	*mockInstanceStorage
	failRepoID string
}

func (m *failOnGetInstancesMockStorage) GetInstances(repoID string) (json.RawMessage, error) {
	if repoID == m.failRepoID {
		return nil, fmt.Errorf("simulated transient read failure")
	}
	return m.mockInstanceStorage.GetInstances(repoID)
}

// TestSaveInstances_ReconcileReadFailureIsPropagatd covers finding 3966293172:
// if GetInstances fails during the archive reconciliation read, SaveInstances
// must return an error rather than silently proceeding to overwrite disk. A
// silent overwrite would clobber a committed archive row with the stale snapshot.
func TestSaveInstances_ReconcileReadFailureIsPropagated(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	ms := newMockStorage()
	rid := config.RepoIDFromRoot(repoPath)
	failingState := &failOnGetInstancesMockStorage{
		mockInstanceStorage: ms,
		failRepoID:          rid,
	}

	alive := makeAliveInstance("alive", repoPath)
	midArchive := &Instance{
		ID:                   "mid-archive-id",
		Title:                "mid-archive",
		Path:                 repoPath,
		Branch:               "af/mid-archive",
		Program:              "claude",
		started:              true,
		liveness:             LiveRunning,
		inFlightOp:           OpArchiving,
		archivePushCompleted: true,
		backend:              newInertSandboxBackend("docker"),
	}

	storage, err := NewStorage(failingState, "")
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if err := storage.SaveInstances([]*Instance{alive, midArchive}); err == nil {
		t.Fatal("SaveInstances must return an error when GetInstances fails during archive reconciliation: " +
			"silently proceeding would overwrite a committed archive row with the stale snapshot")
	}
}

// snapshotHookBackend runs hook when SaveInstances reaches this instance's
// ToInstanceData call. Placing it after another instance makes the hook an exact
// seam between that earlier instance's snapshot and the per-repo file lock.
type snapshotHookBackend struct {
	Backend
	once sync.Once
	hook func()
}

func (b *snapshotHookBackend) Type() string {
	b.once.Do(b.hook)
	return b.Backend.Type()
}

// TestSaveInstances_ReconcilesArchiveStartedAfterCollection covers finding
// 3966405841's exact interleaving: the target was snapshotted as Running, then
// its archive started and committed before SaveInstances acquired the repo
// lock. The checkpoint must not overwrite that newer durable archive with the
// stale snapshot merely because the snapshot had not observed OpArchiving.
func TestSaveInstances_ReconcilesArchiveStartedAfterCollection(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()
	snapshotTime := time.Now().Add(-time.Hour)
	target := &Instance{
		ID: "late-archive-id", Title: "late-archive", Path: repoPath,
		Program: "claude", started: true, liveness: LiveRunning,
		backend: &dockerBackend{}, UpdatedAt: snapshotTime,
	}
	trigger := makeAliveInstance("snapshot-trigger", repoPath)
	trigger.backend = &snapshotHookBackend{
		Backend: trigger.backend,
		hook: func() {
			require.NoError(t, target.Transition(BeginArchive()))
			target.recordArchivePush("af/late-archive")
			require.NoError(t, target.Transition(CommitArchive()))
			seedDisk(t, state, repoPath, []InstanceData{target.ToInstanceData().ForStorage()})
		},
	}

	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{target, trigger}))

	for _, row := range readDisk(t, state, repoPath) {
		if row.ID != target.ID {
			continue
		}
		if row.Liveness != LiveArchived || row.Branch != "af/late-archive" {
			t.Fatalf("checkpoint overwrote the archive that committed after collection: "+
				"Liveness=%v Branch=%q, want LiveArchived and %q", row.Liveness, row.Branch, "af/late-archive")
		}
		return
	}
	t.Fatal("checkpoint erased the archive that committed after collection")
}

// TestReconcilePendingArchiveRows_SelectsNewestDuplicate covers finding
// 3968810374. persistInstanceData updates the first matching duplicate, so a
// stale duplicate can follow it on disk; reconciliation must select by mutation
// time instead of whichever duplicate happens to occur last.
func TestReconcilePendingArchiveRows_SelectsNewestDuplicate(t *testing.T) {
	snapshotTime := time.Now()
	group := []InstanceData{{
		ID: "duplicate-id", Title: "duplicate", BackendType: "docker",
		Branch: "af/in-flight", Liveness: LiveRunning, UpdatedAt: snapshotTime,
	}}
	onDisk := []InstanceData{
		{
			ID: "duplicate-id", Title: "duplicate", BackendType: "docker",
			Branch: "af/committed", Liveness: LiveArchived,
			UpdatedAt: snapshotTime.Add(time.Second),
		},
		{
			ID: "duplicate-id", Title: "duplicate", BackendType: "docker",
			Branch: "", Liveness: LiveRunning,
			UpdatedAt: snapshotTime.Add(-time.Second),
		},
	}

	reconcilePendingArchiveRows(group, map[string]struct{}{"duplicate-id": {}}, onDisk)

	if group[0].Liveness != LiveArchived || group[0].Branch != "af/committed" {
		t.Fatalf("later stale duplicate hid the committed archive: Liveness=%v Branch=%q, "+
			"want LiveArchived and %q", group[0].Liveness, group[0].Branch, "af/committed")
	}
}

// TestSaveInstances_HistoricalBranchDoesNotFenceCurrentPush covers finding
// 3968810390. A restored sandbox deliberately retains its previous archive
// branch. BeginArchive alone therefore cannot make that non-empty branch proof
// that the second archive's push finished.
func TestSaveInstances_HistoricalBranchDoesNotFenceCurrentPush(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	state := newMockStorage()
	const historicalBranch = "af/previous-archive"
	snapshotTime := time.Now()
	seedDisk(t, state, repoPath, []InstanceData{{
		ID: "restored-id", Title: "restored", Path: repoPath,
		Program: "claude", BackendType: "docker", Branch: historicalBranch,
		Status: Archived, Liveness: LiveArchived,
		UpdatedAt: snapshotTime.Add(-time.Hour),
	}})

	alive := makeAliveInstance("alive", repoPath)
	restored := &Instance{
		ID: "restored-id", Title: "restored", Path: repoPath,
		Program: "claude", Branch: historicalBranch, started: true,
		liveness: LiveRunning, UpdatedAt: snapshotTime,
		backend: &dockerBackend{
			containerID: "fresh-live-container",
			cleanup: &DockerRuntimeCleanupData{
				ContainerID: "fresh-live-container", EngineID: "engine-id",
			},
		},
	}
	require.NoError(t, restored.Transition(BeginArchive()))

	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{alive, restored}))

	for _, row := range readDisk(t, state, repoPath) {
		if row.ID != restored.ID {
			continue
		}
		if row.RuntimeCleanupStateUnknown || row.RuntimeCleanup != nil {
			t.Fatalf("second archive retained a pre-push row using historical Branch %q: "+
				"RuntimeCleanupStateUnknown=%v RuntimeCleanup=%+v; restart could reap the live sandbox "+
				"and restore the previous archive", row.Branch, row.RuntimeCleanupStateUnknown, row.RuntimeCleanup)
		}
		t.Fatalf("second archive's pre-push row survived from historical Branch %q; "+
			"the current push has not made its new work durable", row.Branch)
	}
}

func TestArchivePushFenceTracksOnlyCurrentGeneration(t *testing.T) {
	inst := &Instance{
		Title: "restored", Branch: "af/previous-archive", started: true,
		liveness: LiveRunning, archivePushCompleted: true,
	}
	require.NoError(t, inst.Transition(BeginArchive()))
	if inst.ToInstanceData().archivePushCompleted {
		t.Fatal("BeginArchive retained the previous generation's push fence")
	}

	// A second archive normally pushes the same branch. The phase must advance
	// even when recording the branch does not change the Branch field.
	beforePush := inst.UpdatedAt
	inst.recordArchivePush("af/previous-archive")
	afterPush := inst.ToInstanceData()
	if !afterPush.archivePushCompleted {
		t.Fatal("a successful current-generation push did not raise its checkpoint fence")
	}
	if !afterPush.UpdatedAt.After(beforePush) {
		t.Fatal("raising the current-generation push fence did not advance UpdatedAt")
	}

	require.NoError(t, inst.Transition(AbortArchiveToLost()))
	if inst.ToInstanceData().archivePushCompleted {
		t.Fatal("the settled archive retained a process-local push fence")
	}
}
