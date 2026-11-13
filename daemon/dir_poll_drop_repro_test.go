package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// A missing instances DIRECTORY (the whole per-repo subdir gone via an external
// `rm -rf`) is a recoverable disk inconsistency, the same threat model #736 was
// built to handle and the same class the #6048 provenance fix covered for a
// missing instances.json FILE. The loader never enumerates a directory-deleted
// repo, so it is absent from allInstances and Missing[repoID] is never set —
// unlike a deleted file, which the loader reports as present-but-empty ("[]")
// with Missing[repoID]=true. Before the fix, provenance was accrued solely from
// Missing[repoID], so a directory-deleted repo never entered the provenance set
// and was never seen by the restore checkpoint. The first targeted write made
// by any preserved session (which recreates the deleted directory plus a file
// holding only that write's row, or even a bare "[]") then ended preservation on
// the next poll: the repo was now present with real rows or "[]" and had no owed
// provenance, so the refresh took the normal load path and silently dropped
// every in-memory session the write didn't carry — with no warning, no restore
// checkpoint, and no ghost task-run carryover. These tests pin the fix: a
// directory-deleted repo's live sessions stay preserved across the
// targeted-write-recreates-storage boundary, the warning keeps firing, and the
// watch-task concurrency cap keeps counting its live run — exactly as the
// file-deletion sibling tests already pin for the file case.

// TestRefreshDaemonInstances_DirectoryDeletionPreservedAfterTargetedWrite is the
// directory-deletion sibling of
// TestRefreshDaemonInstances_PreservesInstancesAfterInstancesFileRecreatedEmpty:
// deleting the repo's entire instances directory, then a preserved session's
// targeted write that recreates the directory + instances.json holding only ONE
// of the repo's two live sessions, must NOT drop the other live session on the
// following poll. Before the fix, provenance was never accrued for a
// directory-deleted repo (Missing[repoID] was never set), so the second poll saw
// the recreated file as a normal present repo and dropped sess-b through the
// `continue` branch with no warning.
func TestRefreshDaemonInstances_DirectoryDeletionPreservedAfterTargetedWrite(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	warnBuf := teeWarnings(t)
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	repoID := "dir-vanish-repo"
	both, err := json.Marshal([]session.InstanceData{{Title: "sess-a"}, {Title: "sess-b"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, both))
	keyA := daemonInstanceKey(repoID, "sess-a")
	keyB := daemonInstanceKey(repoID, "sess-b")
	instA, instB := &session.Instance{}, &session.Instance{}
	existing := map[string]*session.Instance{keyA: instA, keyB: instB}

	// Remove the repo's entire instances directory, as an external `rm -rf`
	// of the repo's storage would.
	repoDir, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Dir(repoDir)))
	_, dirStatErr := os.Stat(filepath.Dir(repoDir))
	require.True(t, os.IsNotExist(dirStatErr), "fixture: the repo directory must be gone")

	// Poll 1: both sessions preserved (the #736 mechanism works on the first
	// poll — the repo is absent from allInstances so the preserve loop's
	// ok==false arm carries both).
	got, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Same(t, instA, got[keyA], "first poll must preserve sess-a")
	require.Same(t, instB, got[keyB], "first poll must preserve sess-b")
	require.Contains(t, warnBuf.String(), repoID, "first poll must warn naming the repo")

	// A preserved session's targeted write recreates the directory + file with
	// only sess-a's row — exactly what a preserved session's own persist does:
	// it reads the still-missing file as "[]" and saves back one row.
	sessAOnly, err := json.Marshal([]session.InstanceData{{Title: "sess-a"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, sessAOnly))
	recreatedPath, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	_, statErr := os.Stat(recreatedPath)
	require.NoError(t, statErr, "fixture: the directory + instances.json must have been recreated")

	// Poll 2: the file is no longer missing — it carries real rows (sess-a
	// only). Without provenance the normal load path would drop sess-b; with
	// provenance (accrued on poll 1 for the directory-deleted repo) sess-b is
	// still owed and stays preserved.
	warnBuf.Reset()
	got2, _, _, _, err := refreshDaemonInstances(got)
	require.NoError(t, err)
	require.Same(t, instA, got2[keyA], "sess-a must survive via the normal load path (its row is on disk)")
	require.Same(t, instB, got2[keyB],
		"sess-b must still be preserved after a targeted write recreated the directory with only sess-a; without directory-deletion provenance the second poll drops it silently")
	require.Contains(t, warnBuf.String(), repoID,
		"second poll must still warn naming the repo; the preserved session is still owed its record")
}

// TestRefreshDaemonInstances_DirectoryDeletionPreservedAfterEmptyRecreate is the
// directory-deletion sibling of the "[]" recreate case: a targeted write that
// saves back an unchanged "[]" (the shape of a status persist whose row is not
// on disk) recreates the directory with a bare empty file. Before the fix, both
// sessions were dropped on poll 2 with no warning — the empty file was treated
// as a legitimately empty repo because no provenance was owed.
func TestRefreshDaemonInstances_DirectoryDeletionPreservedAfterEmptyRecreate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	warnBuf := teeWarnings(t)
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	repoID := "dir-vanish-empty-repo"
	both, err := json.Marshal([]session.InstanceData{{Title: "sess-a"}, {Title: "sess-b"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, both))
	keyA := daemonInstanceKey(repoID, "sess-a")
	keyB := daemonInstanceKey(repoID, "sess-b")
	instA, instB := &session.Instance{}, &session.Instance{}
	existing := map[string]*session.Instance{keyA: instA, keyB: instB}

	repoDir, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Dir(repoDir)))

	// Poll 1: both preserved.
	got, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Same(t, instA, got[keyA], "first poll must preserve sess-a")
	require.Same(t, instB, got[keyB], "first poll must preserve sess-b")

	// A targeted write recreates the directory + file as a bare "[]".
	require.NoError(t, config.SaveRepoInstances(repoID, json.RawMessage("[]")))
	recreatedPath, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	_, statErr := os.Stat(recreatedPath)
	require.NoError(t, statErr, "fixture: the directory + empty instances.json must have been recreated")

	// Poll 2: the file is present-but-empty. Without provenance the empty branch
	// would drop both sessions; with provenance both are still owed and stay
	// preserved.
	warnBuf.Reset()
	got2, _, _, _, err := refreshDaemonInstances(got)
	require.NoError(t, err)
	require.Same(t, instA, got2[keyA],
		"sess-a must still be preserved after a targeted write recreated an empty file; without directory-deletion provenance the second poll drops it silently")
	require.Same(t, instB, got2[keyB],
		"sess-b must still be preserved after a targeted write recreated an empty file; without directory-deletion provenance the second poll drops it silently")
	require.Contains(t, warnBuf.String(), repoID, "second poll must still warn naming the repo")
}

// TestRefreshDaemonInstances_DirectoryDeletionPreservedAcrossPolls pins that the
// directory-deletion preservation re-hydrates on EVERY tick while the directory
// stays gone — not just the first poll. The directory carries no file
// signature, so the per-tick row-outcome cache never keys a replay, and every
// poll re-hits the absent-repo path. Without provenance accruing on every poll,
// the sessions would survive the first poll only to vanish on the second.
func TestRefreshDaemonInstances_DirectoryDeletionPreservedAcrossPolls(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	repoID := "dir-vanish-across-polls-repo"
	rows, err := json.Marshal([]session.InstanceData{{Title: "running-session"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, rows))
	key := daemonInstanceKey(repoID, "running-session")
	inst := &session.Instance{}
	existing := map[string]*session.Instance{key: inst}

	repoDir, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Dir(repoDir)))

	// Three polls, feeding each prior map back as existing: the session must
	// stay preserved on every tick.
	got, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Same(t, inst, got[key], "first poll must preserve the live session")

	got2, _, _, _, err := refreshDaemonInstances(got)
	require.NoError(t, err)
	require.Same(t, inst, got2[key], "second poll must continue to preserve the live session")

	got3, _, _, _, err := refreshDaemonInstances(got2)
	require.NoError(t, err)
	require.Same(t, inst, got3[key], "third poll must continue to preserve the live session")
}

// TestRefreshDaemonInstances_DirectoryDeletionLegitimatelyEmptyNotOverPreserved
// pins the other half of the directory-deletion provenance fix: a repo that was
// NEVER observed as directory-deleted (its directory exists and its file is
// legitimately "[]") must NOT be preserved — the empty file is the authoritative
// state, not data loss to paper over. This mirrors
// TestRefreshDaemonInstances_DropsInstancesForLegitimatelyEmptyInstancesFile for
// the directory case: provenance is keyed to observed absence, so a present repo
// with an empty file the daemon never saw vanish is dropped, not preserved.
func TestRefreshDaemonInstances_DirectoryDeletionLegitimatelyEmptyNotOverPreserved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	// A repo whose roster an external actor replaced with "[]" — the daemon
	// never saw its directory go missing, so it has no provenance to preserve
	// from.
	legitEmptyRepoID := "legit-empty-dir-repo"
	require.NoError(t, config.SaveRepoInstances(legitEmptyRepoID, json.RawMessage("[]")))
	legitKey := daemonInstanceKey(legitEmptyRepoID, "running-session")
	legitInst := &session.Instance{}
	existing := map[string]*session.Instance{legitKey: legitInst}

	got, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.NotContains(t, got, legitKey,
		"a legitimately emptied instances.json (directory present, never observed missing) must not preserve stale in-memory sessions; the empty file is the authoritative state")
}

// TestManager_RefreshLocked_DirectoryDeletionPreservesTaskRunSlotAgainstCap is
// the end-to-end impact regression (#1892) for the directory-deletion case: a
// watch-task session holding a max_concurrent_runs slot that has its repo's
// instances DIRECTORY deleted mid-life must stay in m.instances and keep counting
// against the cap across the targeted-write-recreates-storage boundary, so a cap
// that was saturated before the deletion still refuses a replacement run. This
// mirrors TestManager_RefreshLocked_MissingFilePreservesTaskRunSlotAgainstCap
// (which pins the file-deletion case) for the directory case.
func TestManager_RefreshLocked_DirectoryDeletionPreservesTaskRunSlotAgainstCap(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	const limit = 1
	data, err := createForTask(manager, repoPath, "task1", "dlq-triage", limit)
	require.NoError(t, err)
	key := daemonInstanceKey(repo.ID, data.Title)

	// Pin the live session pointer and the saturated cap BEFORE the external
	// deletion.
	manager.mu.Lock()
	liveInst := manager.instances[key]
	runs := manager.countTaskRunsLocked(repo.ID, "task1")
	admitErr := manager.admitTaskRunLocked(repo.ID, "task1", limit)
	manager.mu.Unlock()
	require.NotNil(t, liveInst, "precondition: created task session must be in m.instances")
	require.Equal(t, 1, runs, "precondition: the cap is full with one in-flight run")
	require.True(t, isAtConcurrencyLimitErr(admitErr), "precondition: a second run must be refused at the limit")

	// An external actor deletes the repo's entire instances directory.
	path, err := config.RepoInstancesPath(repo.ID)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Dir(path)))

	// The polling refresh rebuilds m.instances from the now-missing directory.
	manager.mu.Lock()
	refreshErr := manager.refreshLocked()
	after := manager.instances[key]
	runsAfter := manager.countTaskRunsLocked(repo.ID, "task1")
	admitErrAfter := manager.admitTaskRunLocked(repo.ID, "task1", limit)
	manager.mu.Unlock()
	require.NoError(t, refreshErr)
	require.Same(t, liveInst, after,
		"a missing instances directory must preserve the repo's live in-memory session; refreshLocked replaced m.instances wholesale")
	require.Equal(t, 1, runsAfter,
		"the preserved session's run must still count against max_concurrent_runs; without preservation the count drops to 0 and the cap over-admits")
	require.True(t, isAtConcurrencyLimitErr(admitErrAfter),
		"a saturated cap must still refuse a replacement run after the directory vanishes")
}

// TestRefreshDaemonInstances_UnreadableRepoNotAccruedAsDirectoryDeletion pins
// the Codex review fix: a repo the loader reports in load.Skipped after a
// transient read failure is also absent from allInstances, but it is NOT a
// directory deletion — its file still exists on disk. Before the fix, the
// !observedRepo arm accrued provenance for it too, so restoreMissingRecordsLocked
// checkpointed the in-memory instances over a file that was never missing
// (overwriting newer fields), or retried a misleading missing-record restore
// every poll on a persistent failure. The unreadable repo is still PRESERVED in
// memory by the preserve loop's ok==false arm (the existing #736/#4783
// behavior), but it must NOT enter the missing-file provenance set, and the
// unreadable file on disk must be left untouched.
func TestRefreshDaemonInstances_UnreadableRepoNotAccruedAsDirectoryDeletion(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	repoID := "unreadable-not-deleted-repo"
	rows, err := json.Marshal([]session.InstanceData{{Title: "sess-a"}, {Title: "sess-b"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, rows))
	keyA := daemonInstanceKey(repoID, "sess-a")
	keyB := daemonInstanceKey(repoID, "sess-b")
	instA, instB := &session.Instance{}, &session.Instance{}
	existing := map[string]*session.Instance{keyA: instA, keyB: instB}

	// The file is present on disk; only the read fails this poll (a transient
	// read failure, the #4783 shape — not a deletion).
	stagedPath, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	_, statErr := os.Stat(stagedPath)
	require.NoError(t, statErr, "fixture: the unreadable repo's file must exist on disk")
	baseline, err := os.ReadFile(stagedPath)
	require.NoError(t, err)
	lift := stageUnreadableRepoForRefresh(t, repoID)

	// Poll: both sessions are preserved in memory (the ok==false arm carries
	// them, exactly as before this PR), but provenance must NOT be accrued —
	// the file was never missing, only unreadable.
	got, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Same(t, instA, got[keyA], "an unreadable repo's live sessions are still preserved in memory")
	require.Same(t, instB, got[keyB], "an unreadable repo's live sessions are still preserved in memory")
	require.False(t, preservedMissingFileRepos.observed()[repoID],
		"an unreadable repo must not accrue missing-file provenance; its file was never missing, only unreadable, so the restore checkpoint must not fire against it")

	// The restore checkpoint is a no-op for the unreadable repo: the file on
	// disk must still be byte-for-byte the original, untouched.
	(&Manager{}).restoreMissingRecordsLocked(got)
	onDisk, err := os.ReadFile(stagedPath)
	require.NoError(t, err)
	require.Equal(t, string(baseline), string(onDisk),
		"the restore checkpoint must not overwrite an unreadable repo's file that was never missing")
	require.False(t, preservedMissingFileRepos.observed()[repoID],
		"the episode must not be opened for an unreadable repo")

	// When the read failure lifts, the next poll loads the rows normally and
	// the in-memory sessions reconcile to disk — no provenance was owed.
	lift()
	got2, _, _, _, err := refreshDaemonInstances(got)
	require.NoError(t, err)
	require.Contains(t, got2, keyA, "the re-read repo's session materializes once the read succeeds")
	require.False(t, preservedMissingFileRepos.observed()[repoID],
		"no provenance was ever owed for the unreadable repo, so none lingers after the read succeeds")
}

// TestRestoreMissingRecords_DirectoryDeletionFiresRestoreCheckpoint pins that
// the restore checkpoint — which writes preserved sessions' records back to disk
// so preservation ends durably instead of hanging on a single targeted write —
// now fires for the directory-deletion case. Before the fix, a directory-deleted
// repo never entered the provenance set, so restoreMissingRecordsLocked was a
// no-op and the sessions sat in memory one write away from loss. With the fix,
// provenance is accrued for directory deletion, so the checkpoint writes the
// records back and recreates the directory.
func TestRestoreMissingRecords_DirectoryDeletionFiresRestoreCheckpoint(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	resetPreservedMissingFileReposForTest()
	t.Cleanup(resetPreservedMissingFileReposForTest)

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		return &session.Instance{Title: d.Title}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	repoID := "dir-restore-repo"
	rows, err := json.Marshal([]session.InstanceData{{Title: "a"}, {Title: "b"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, rows))
	keyA, keyB := daemonInstanceKey(repoID, "a"), daemonInstanceKey(repoID, "b")
	instA, instB := &session.Instance{Title: "a"}, &session.Instance{Title: "b"}
	existing := map[string]*session.Instance{keyA: instA, keyB: instB}

	// Delete the repo's entire instances directory.
	repoDir, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Dir(repoDir)))

	// Poll 1: both sessions preserved and provenance accrued (the fix).
	got, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Same(t, instA, got[keyA])
	require.Same(t, instB, got[keyB])
	require.True(t, preservedMissingFileRepos.observed()[repoID],
		"provenance must be accrued for a directory-deleted repo so the restore checkpoint fires")

	// The restore checkpoint writes the preserved sessions' records back to
	// disk, recreating the directory + file. Stub saveRestoredRepoRecords to
	// write through the real config.SaveRepoInstances path (which recreates the
	// directory via MkdirAll), so the test verifies the checkpoint fires and
	// the directory is durably recreated — not just that it was called.
	prevSave := saveRestoredRepoRecords
	var writtenTitles []string
	saveRestoredRepoRecords = func(_ *Manager, id string, insts []*session.Instance, _ time.Duration) ([]string, error) {
		rows := make([]session.InstanceData, 0, len(insts))
		for _, inst := range insts {
			rows = append(rows, session.InstanceData{Title: inst.Title})
			writtenTitles = append(writtenTitles, inst.Title)
		}
		data, err := json.Marshal(rows)
		require.NoError(t, err)
		require.NoError(t, config.SaveRepoInstances(id, data))
		return writtenTitles, nil
	}
	t.Cleanup(func() { saveRestoredRepoRecords = prevSave })
	(&Manager{}).restoreMissingRecordsLocked(got)

	// The directory + file must have been recreated with both sessions.
	recreatedPath, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	_, statErr := os.Stat(recreatedPath)
	require.NoError(t, statErr, "the restore checkpoint must have recreated the directory + instances.json")
	require.ElementsMatch(t, []string{"a", "b"}, writtenTitles,
		"the restore checkpoint must write both preserved sessions' records back to disk")
	require.False(t, preservedMissingFileRepos.observed()[repoID],
		"the episode must end once the records are back on disk")
}
