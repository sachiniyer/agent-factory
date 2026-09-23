package daemon

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// stageUnreadableRepoForRefresh makes the refresh loader report repoID as
// unreadable, the way config.LoadAllRepoInstancesReportingSkipDetails reports a
// file it hit a permission or I/O error on, while every other repo loads from
// disk as usual. It returns a func that lifts the failure again.
//
// A seam rather than a chmod: a persistently unreadable file aborts the refresh
// earlier, in the schema migrator, so the shape #4783 reaches is a file that
// read there and failed a moment later in the loader. That window cannot be
// staged on a real file system deterministically, and chmod is a no-op under
// root anyway.
func stageUnreadableRepoForRefresh(t *testing.T, repoID string) (lift func()) {
	t.Helper()
	return stageRepoReadErrorForRefresh(t, repoID, fmt.Errorf("failed to read repo instances: %w", fs.ErrPermission))
}

// stageRepoReadErrorForRefresh is stageUnreadableRepoForRefresh with the
// loader error chosen by the caller.
func stageRepoReadErrorForRefresh(t *testing.T, repoID string, readErr error) (lift func()) {
	t.Helper()
	prev := loadAllRepoInstancesForRefresh
	staged := true
	loadAllRepoInstancesForRefresh = func() (map[string]json.RawMessage, []config.RepoInstancesSkip, map[string]bool, error) {
		all, skips, missing, err := prev()
		if err != nil || !staged {
			return all, skips, missing, err
		}
		delete(all, repoID)
		path, _ := config.RepoInstancesPath(repoID)
		skips = append(skips, config.RepoInstancesSkip{RepoID: repoID, Path: path, Err: readErr})
		return all, skips, missing, nil
	}
	t.Cleanup(func() { loadAllRepoInstancesForRefresh = prev })
	return func() { staged = false }
}

// TestManager_RefreshLocked_KeepsSkippedRepoThatBecomesUnreadable is the #4783
// repro. A repo skipped at startup for a corrupt instances.json becomes
// unreadable mid-life. The loader leaves it out of its result, and the poll used
// to read that absence as a repair: the repo left the skip set, and list/get/
// whoami served the truncated snapshot as complete again (the #4734 bug through
// a second path).
//
// The file on disk is repaired BEFORE the failed read on purpose: had the
// refresh read it, it would parse. Only a read that succeeds may clear the repo,
// which the last step proves by lifting the failure.
func TestManager_RefreshLocked_KeepsSkippedRepoThatBecomesUnreadable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	repairedJSON, err := json.Marshal([]session.InstanceData{{Title: "repaired"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairedJSON))
	lift := stageUnreadableRepoForRefresh(t, "corrupt-r")

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
		"startup must seed the skip set with the corrupted repo")

	require.NoError(t, m.refreshLocked(), "a poll that cannot read one repo must not error")
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonUnreadableInstancesJSON}}, m.skippedRepos,
		"an unreadable repo is not a repaired one: it stays skipped, and the reason says unreadable now")
	require.Nil(t, m.instances[daemonInstanceKey("corrupt-r", "repaired")],
		"nothing was read, so no row may appear")
	require.NotNil(t, m.instances[daemonInstanceKey("valid-r", "ok")],
		"the unrelated healthy repo survives the poll")

	lift()
	require.NoError(t, m.refreshLocked())
	require.Empty(t, m.skippedRepos, "a successful re-read clears the repo")
	require.NotNil(t, m.instances[daemonInstanceKey("corrupt-r", "repaired")],
		"the re-read repo's session materializes on the same poll that clears it")
}

// TestManager_RefreshLocked_KeepsSkippedRepoWhoseDirectoryVanished pins the
// other omission. A startup-skipped repo whose directory is removed mid-life is
// absent from the loader's result AND from its skips, which the poll also used
// to read as a repair. Nothing was re-read, so the repo stays skipped until a
// read succeeds or the daemon restarts and re-runs startup.
func TestManager_RefreshLocked_KeepsSkippedRepoWhoseDirectoryVanished(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	path, err := config.RepoInstancesPath("corrupt-r")
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Dir(path)))

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
		"startup must seed the skip set with the corrupted repo")
	require.NoError(t, m.refreshLocked())
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonCorruptedInstancesJSON}}, m.skippedRepos,
		"a vanished repo was not re-read, so it keeps its startup entry")
}

// TestManager_RefreshLocked_KeepsSkippedRepoWhoseFileWasDeleted is the
// directory case's sibling (Codex on #4812). The loader turns a missing
// instances.json into "[]", which looks exactly like a file that was read and
// held no sessions. Nothing was read, so the repo must stay skipped.
func TestManager_RefreshLocked_KeepsSkippedRepoWhoseFileWasDeleted(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	seedCorruptedRepo(t, "corrupt-r")
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	path, err := config.RepoInstancesPath("corrupt-r")
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	_, statErr := os.Stat(filepath.Dir(path))
	require.NoError(t, statErr, "fixture: the repo directory must remain, only the file goes")

	m.mu.Lock()
	defer m.mu.Unlock()
	require.NoError(t, m.refreshLocked())
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonCorruptedInstancesJSON}}, m.skippedRepos,
		"a deleted file was not re-read, so it keeps its startup entry")
}

// TestRefreshDaemonInstances_ReportsNewerSchemaRepoWithItsOwnReason: a file a
// newer af wrote needs an upgrade, not a permissions check, so it carries its
// own reason for the client's remedy (Codex on #4812).
func TestRefreshDaemonInstances_ReportsNewerSchemaRepoWithItsOwnReason(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	require.NoError(t, config.SaveRepoInstances("newer-r", json.RawMessage("[]")))
	path, err := config.RepoInstancesPath("newer-r")
	require.NoError(t, err)
	stageRepoReadErrorForRefresh(t, "newer-r", &config.UnsupportedSchemaVersionError{
		StoreName: config.InstancesFileName, Path: path, FileVersion: 99, SupportedVersion: config.InstancesSchemaVersion,
	})

	_, _, skipped, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	require.Equal(t, []SkippedRepo{{RepoID: "newer-r", Reason: SkippedRepoReasonNewerSchemaInstancesJSON}}, skipped)
}

// TestRefreshDaemonInstances_ReportsUnreadableRepo pins the loader half: a repo
// the loader could not read is reported under its own reason, is left out of
// reread, and contributes no rows. Before #4783 the refresh loaded through the
// omitting form and never learned the repo existed, so a startup that lost a
// file to a transient read failure served the truncated list as complete.
func TestRefreshDaemonInstances_ReportsUnreadableRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	warnings := captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	hiddenJSON, err := json.Marshal([]session.InstanceData{{Title: "hidden"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("unreadable-r", hiddenJSON))
	stageUnreadableRepoForRefresh(t, "unreadable-r")

	got, _, skipped, reread, err := refreshDaemonInstances(nil)
	require.NoError(t, err, "one unreadable repo must not fail the whole load")
	require.Equal(t, []SkippedRepo{{RepoID: "unreadable-r", Reason: SkippedRepoReasonUnreadableInstancesJSON}}, skipped)
	require.Equal(t, map[string]bool{"valid-r": true}, reread,
		"reread names only the repo that was actually read and parsed")
	require.Nil(t, got[daemonInstanceKey("unreadable-r", "hidden")])
	require.NotNil(t, got[daemonInstanceKey("valid-r", "ok")])
	require.Contains(t, warnings.String(), "unreadable instances.json",
		"the daemon log names the read failure, not a corruption")
}

// TestManager_RefreshLocked_DoesNotAddMidlifeUnreadableToSkipSet is the control
// for the fix above; its skip-set half held before #4783 too. A repo that was
// healthy at startup and becomes unreadable mid-life keeps its re-hydrated rows, so the
// snapshot is whole and the repo is NOT added to the skip set. The fix must not
// turn a transient read failure on a loaded repo into a refusal.
func TestManager_RefreshLocked_DoesNotAddMidlifeUnreadableToSkipSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	warnings := captureWarnings(t)

	liveJSON, err := json.Marshal([]session.InstanceData{{Title: "a-sess"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("repo-a", liveJSON))

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	stageUnreadableRepoForRefresh(t, "repo-a")

	m.mu.Lock()
	defer m.mu.Unlock()
	require.NoError(t, m.refreshLocked())
	require.Empty(t, m.skippedRepos, "a mid-life read failure on a loaded repo is not added to the skip set")
	require.NotNil(t, m.instances[daemonInstanceKey("repo-a", "a-sess")],
		"the prior in-memory instance is re-hydrated")
	require.NotContains(t, warnings.String(), "missing repo directory",
		"an unreadable file is not a missing directory, so the log must not say it is")
}

// TestRetainStillSkipped_OnlyARereadClears pins the rule in isolation: a
// startup-skipped repo leaves the set only when reread names it, and a repo the
// fresh refresh still reports keeps fresh's entry so its reason is current.
func TestRetainStillSkipped_OnlyARereadClears(t *testing.T) {
	corrupt := SkippedRepo{RepoID: "r", Reason: SkippedRepoReasonCorruptedInstancesJSON}
	unreadable := SkippedRepo{RepoID: "r", Reason: SkippedRepoReasonUnreadableInstancesJSON}
	prev := []SkippedRepo{corrupt}

	require.Equal(t, prev, retainStillSkipped(prev, nil, nil),
		"absent from fresh and not re-read (vanished) is an omission, not a repair")
	require.Equal(t, []SkippedRepo{unreadable}, retainStillSkipped(prev, []SkippedRepo{unreadable}, nil),
		"still reported: kept, carrying the fresh reason")
	require.Equal(t, prev, retainStillSkipped(prev, []SkippedRepo{corrupt}, nil),
		"still corrupt: kept")
	require.Empty(t, retainStillSkipped(prev, nil, map[string]bool{"r": true}),
		"re-read and parsed: cleared")
	require.Nil(t, retainStillSkipped(nil, []SkippedRepo{unreadable}, nil),
		"a repo that fails mid-life is never added")
}
