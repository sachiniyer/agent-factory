package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// A missing instances.json FILE (repo directory still present) is a recoverable
// external disk inconsistency, the same threat model #736/#603/#4783 were built
// to handle. The loader reports such a repo as present-but-empty ("[]") with
// Missing[repoID]=true, so the empty branch of the refresh loop ran out before
// any per-row processing and the #736 preservation loop's guard — `if _, ok :=
// allInstances[repoID]; ok { continue }` — treated the placeholder exactly like a
// healthy repo and skipped preservation. refreshLocked then replaced m.instances
// wholesale with the empty result, dropping every live in-memory session for the
// repo with no log line and releasing its in-flight task-run slots. Every sibling
// damage class (corrupt JSON, unreadable file, deleted repo DIRECTORY) warned and
// preserved; only the deleted FILE dropped silently. These pin the fix: a
// missing-file repo is preserved and warned exactly like a missing-directory
// repo, and the watch-task concurrency cap keeps counting its live run.

// TestRefreshDaemonInstances_PreservesInstancesForMissingInstancesFile is the
// file-deletion sibling of TestRefreshDaemonInstances_PreservesInstancesForMissingRepoDirectory:
// deleting ONLY the repo's instances.json (its directory left in place) must
// preserve the repo's prior in-memory instance, log a warning naming the repo and
// the missing storage, and continue to load a healthy sibling repo normally.
func TestRefreshDaemonInstances_PreservesInstancesForMissingInstancesFile(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	warnBuf := teeWarnings(t)

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	// A repo that stays on disk — proves we don't over-preserve and that the
	// normal load path still works alongside a file-vanished repo.
	presentRepoID := "present-repo"
	presentJSON, err := json.Marshal([]session.InstanceData{{Title: "present-session"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(presentRepoID, presentJSON))

	// A repo whose instances.json we delete out from under the daemon, leaving
	// its directory in place — an external `rm` of exactly the per-repo file.
	missingRepoID := "midlife-vanish-repo"
	missingJSON, err := json.Marshal([]session.InstanceData{{Title: "running-session"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(missingRepoID, missingJSON))

	// In-memory state as if both sessions had already been loaded.
	presentKey := daemonInstanceKey(presentRepoID, "present-session")
	missingKey := daemonInstanceKey(missingRepoID, "running-session")
	presentInst := &session.Instance{}
	missingInst := &session.Instance{}
	existing := map[string]*session.Instance{
		presentKey: presentInst,
		missingKey: missingInst,
	}

	// Remove only the missing repo's instances.json file, keeping the directory.
	missingPath, err := config.RepoInstancesPath(missingRepoID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(missingPath))
	_, statErr := os.Stat(filepath.Dir(missingPath))
	require.NoError(t, statErr, "fixture: the repo directory must remain, only the file goes")
	_, fileStatErr := os.Stat(missingPath)
	require.True(t, os.IsNotExist(fileStatErr), "fixture: the instances.json file must be gone")

	got, _, skipped, reread, err := refreshDaemonInstances(existing)
	require.NoError(t, err, "refreshDaemonInstances returned error on missing-file repo")

	// The live session is preserved — same pointer, not re-materialized or dropped.
	require.Same(t, missingInst, got[missingKey],
		"running session for missing-file repo %q was dropped; in-memory instance must be preserved, not silently abandoned", missingRepoID)
	// The healthy sibling still loads through the normal path.
	require.Same(t, presentInst, got[presentKey],
		"session for present repo %q should still be loaded (reused) alongside a file-vanished repo", presentRepoID)
	// A missing file is a recoverable omission, not corruption or an unreadable
	// file: it must not land in the skip set (matching the missing-directory case),
	// or list/get/whoami would refuse a snapshot whose preserved sessions are whole.
	require.Empty(t, skipped, "a missing-file repo is recoverable; it must not be reported skipped")
	// Nothing was read for the missing file, so it must not count as a re-read
	// (#4783) — which would otherwise clear a startup skip entry as a repair.
	require.False(t, reread[missingRepoID], "a missing file was not read; it must not count as a re-read")
	require.True(t, reread[presentRepoID], "the present repo WAS read and must count as a re-read")
	// A loud warning names the repo and the missing storage.
	require.Contains(t, warnBuf.String(), missingRepoID, "expected warning naming missing-file repo %q; got: %q", missingRepoID, warnBuf.String())
	require.Contains(t, warnBuf.String(), "instances.json", "expected warning to name the missing instances.json file; got: %q", warnBuf.String())
}

// TestRefreshDaemonInstances_PreservesInstancesForMissingInstancesFileAcrossPolls
// pins that the loss is not recovered and not periodic: a missing file carries no
// file signature, so the per-tick row-outcome cache never keys a replay for it and
// every poll re-hits the empty branch. The preservation loop must re-hydrate on
// EVERY tick, or the sessions would survive the first poll only to vanish on the
// second — a flakier but equally permanent version of the bug.
func TestRefreshDaemonInstances_PreservesInstancesForMissingInstancesFileAcrossPolls(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	missingRepoID := "midlife-vanish-repo"
	missingJSON, err := json.Marshal([]session.InstanceData{{Title: "running-session"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(missingRepoID, missingJSON))
	missingKey := daemonInstanceKey(missingRepoID, "running-session")
	missingInst := &session.Instance{}
	existing := map[string]*session.Instance{missingKey: missingInst}

	missingPath, err := config.RepoInstancesPath(missingRepoID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(missingPath))

	// First poll after the file vanishes.
	got, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Same(t, missingInst, got[missingKey], "first poll must preserve the live session")

	// Second poll, feeding the first poll's preserved map back as `existing`.
	got2, _, _, _, err := refreshDaemonInstances(got)
	require.NoError(t, err)
	require.Same(t, missingInst, got2[missingKey],
		"second poll must continue to preserve the live session; the file carries no signature so the outcome cache never re-keys it and the preservation loop must run every tick")

	// A third poll, to be definite about the steady state.
	got3, _, _, _, err := refreshDaemonInstances(got2)
	require.NoError(t, err)
	require.Same(t, missingInst, got3[missingKey], "steady state must keep the live session preserved")
}

// TestManager_RefreshLocked_MissingFilePreservesTaskRunSlotAgainstCap is the
// end-to-end impact regression (#1892): a watch-task session holding a
// max_concurrent_runs slot that has its repo's instances.json deleted mid-life
// must stay in m.instances and keep counting against the cap, so a cap that was
// saturated before the deletion still refuses a replacement run. Before the fix
// the empty branch dropped the instance from m.instances with no compensating
// ghost count (the row was never decoded), under-counting the cap and admitting a
// run past the limit while the original was still executing.
func TestManager_RefreshLocked_MissingFilePreservesTaskRunSlotAgainstCap(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	const limit = 1
	// One task-spawned session, booted through Running exactly as the watch
	// delivery path does, so it holds its slot (max_concurrent_runs=1, now full).
	data, err := createForTask(manager, repoPath, "task1", "dlq-triage", limit)
	require.NoError(t, err)
	key := daemonInstanceKey(repo.ID, data.Title)

	// Pin the live session pointer and the saturated cap BEFORE the external
	// deletion, so the post-refresh assertion is a pointer-identity check, not a
	// "an instance exists" check (which a freshly materialized one would satisfy).
	manager.mu.Lock()
	liveInst := manager.instances[key]
	require.NotNil(t, liveInst, "precondition: created task session must be in m.instances")
	require.Equal(t, 1, manager.countTaskRunsLocked(repo.ID, "task1"),
		"precondition: the cap is full with one in-flight run")
	require.True(t, isAtConcurrencyLimitErr(manager.admitTaskRunLocked(repo.ID, "task1", limit)),
		"precondition: a second run must be refused at the limit")
	manager.mu.Unlock()

	// An external actor deletes exactly the repo's instances.json (directory kept).
	path, err := config.RepoInstancesPath(repo.ID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	// The polling refresh rebuilds m.instances from (now missing) disk.
	manager.mu.Lock()
	require.NoError(t, manager.refreshLocked())
	// The live session is the SAME instance — preserved, not dropped and
	// re-materialized into a fresh identity.
	require.Same(t, liveInst, manager.instances[key],
		"a missing instances.json must preserve the repo's live in-memory session; refreshLocked replaced m.instances wholesale")
	// Its in-flight run is still counted against the cap.
	require.Equal(t, 1, manager.countTaskRunsLocked(repo.ID, "task1"),
		"the preserved session's run must still count against max_concurrent_runs; without preservation the count drops to 0 and the cap over-admits")
	require.True(t, isAtConcurrencyLimitErr(manager.admitTaskRunLocked(repo.ID, "task1", limit)),
		"a saturated cap must still refuse a replacement run after the file vanishes; admitting one would exceed max_concurrent_runs while the original is still in flight")
	manager.mu.Unlock()
}
