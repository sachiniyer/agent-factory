package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// stubFromInstanceForRefresh swaps fromInstanceDataForRefresh for a stub that
// returns a bare instance without touching tmux/PTY, restoring it on cleanup.
// Mirrors the seam TestRefreshDaemonInstances_SkipsCorruptedRepoAtStartup uses.
func stubFromInstanceForRefresh(t *testing.T) {
	t.Helper()
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		return &session.Instance{ID: d.ID, Title: d.Title}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })
}

// seedCorruptedRepo writes a corrupted instances.json so refreshDaemonInstances
// logs, skips, and reports it; returns the repoID it was written under.
func seedCorruptedRepo(t *testing.T, repoID string) {
	t.Helper()
	require.NoError(t, config.SaveRepoInstances(repoID, json.RawMessage("{not valid json")))
}

// TestRefreshDaemonInstances_ReportsSkippedRepos pins the wire-side fix's daemon
// half: refreshDaemonInstances returns the corrupted repo IDs it skipped at
// startup (#603) as a fourth return, so the Manager/Socket handler can carry them
// to clients. Pre-fix, the only signal was a daemon-local WARNING log a CLI user
// decoding SnapshotResponse never saw.
func TestRefreshDaemonInstances_ReportsSkippedRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	_, _, skipped, err := refreshDaemonInstances(nil)
	require.NoError(t, err, "startup must not fail on a corrupted repo (#603)")

	var ids []string
	for _, s := range skipped {
		require.Equal(t, SkippedRepoReasonCorruptedInstancesJSON, s.Reason, "reason must be the stable code for a corrupted instances.json")
		ids = append(ids, s.RepoID)
	}
	require.Equal(t, []string{"corrupt-r"}, ids,
		"skipped must name exactly the corrupted repo, so the Snapshot RPC can carry it to clients")
}

// TestManager_SkippedReposSeededAtStartupAndScoped verifies the end-to-end
// daemon path the bug report names: a Manager built (NewManager → RestoreInstances
// → refreshDaemonInstances(nil)) on disk with a corrupted repo seeds
// m.skippedRepos from the startup drop, and the accessor scopes it to the
// requested repo (all repos when empty) the way Instances are scoped.
func TestManager_SkippedReposSeededAtStartupAndScoped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err, "NewManager must start despite a corrupted repo (#603)")

	// m.instances must carry the valid repo's session and NOT the corrupted
	// repo's (the candidate drop the fix reports).
	require.NotNil(t, m.instances[daemonInstanceKey("valid-r", "ok")], "valid repo's session must load at startup")
	require.Nil(t, m.instances[daemonInstanceKey("corrupt-r", "anything")], "corrupted repo must contribute no rows")

	// All-repo read reports the dropped repo; a scoped read to the valid repo
	// reports nothing (its sessions are present); a scoped read to the corrupted
	// repo reports it.
	all := m.SkippedRepos("")
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(all), "all-repo read reports every dropped repo")
	require.Empty(t, m.SkippedRepos("valid-r"), "valid repo is not skipped, so a scoped read reports nothing")
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.SkippedRepos("corrupt-r")),
		"scoped read to the corrupted repo reports it")
}

// TestControlServerSnapshot_ReportsAndScopesSkippedRepos verifies the wire
// handler: the operator's Snapshot carries SkippedRepos scoped to the request
// (mirroring Instances), and a sandbox caller gets nothing — a repo ID is
// operator identity the sandbox projection already withholds.
func TestControlServerSnapshot_ReportsAndScopesSkippedRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: m}

	// All-repo Snapshot: the valid session is present and the corrupted repo is
	// named in SkippedRepos — the operator gets the incompleteness signal a CLI
	// list can refuse on, instead of a silently-truncated list.
	var resp SnapshotResponse
	require.NoError(t, cs.Snapshot(SnapshotRequest{}, &resp))
	require.Equal(t, []string{"ok"}, snapshotFilterTitles(resp.Instances), "valid repo's session survives the startup drop")
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(resp.SkippedRepos),
		"all-repo Snapshot must carry the dropped repo to the client")

	// Scoped to the corrupted repo: Instances is empty AND SkippedRepos names it,
	// so a 'sessions list --repo corrupt-r' can refuse rather than return [] with
	// err == nil (the report's headline misread).
	resp = SnapshotResponse{}
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "corrupt-r"}, &resp))
	require.Empty(t, resp.Instances, "corrupted repo's sessions are dropped at startup")
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(resp.SkippedRepos),
		"scoped Snapshot to the corrupted repo must still name it as skipped")

	// Scoped to the valid repo: its session is present and SkippedRepos is empty —
	// a different repo's drop does not pollute the scoped read.
	resp = SnapshotResponse{}
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "valid-r"}, &resp))
	require.Equal(t, []string{"ok"}, snapshotFilterTitles(resp.Instances))
	require.Empty(t, resp.SkippedRepos, "a scoped read to a healthy repo reports no skip")

	// Sandbox caller: repo IDs are operator identity the sandbox projection
	// withholds, so SkippedRepos is emptied just like DeliveryAlarms.
	resp = SnapshotResponse{}
	sandboxCtx := withSandboxOwner(context.Background(), "any-session-id")
	require.NoError(t, cs.snapshot(sandboxCtx, SnapshotRequest{}, &resp))
	require.Empty(t, resp.SkippedRepos, "a sandbox must not learn which repos were dropped (operator identity)")
}

// skippedRepoIDs asserts on the repo IDs carried by a Snapshot's skipped set.
func skippedRepoIDs(skipped []SkippedRepo) []string {
	ids := make([]string, 0, len(skipped))
	for _, s := range skipped {
		ids = append(ids, s.RepoID)
	}
	return ids
}

// TestManager_RefreshLocked_DropsRepairedRepoFromSkipSet pins the "clear
// repaired repos" lifecycle: a repo dropped at startup (corrupt-r) is removed
// from m.skippedRepos on the first polling refresh that parses its now-repaired
// instances.json, so list/get/whoami stop refusing the now-complete snapshot
// without waiting for a daemon restart (#603 closed over the wire).
func TestManager_RefreshLocked_DropsRepairedRepoFromSkipSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Repair corrupt-r by overwriting its instances.json with a parseable file;
	// the next polling refresh must drop it from m.skippedRepos and re-materialize
	// its session so the snapshot is complete again.
	repairedJSON, err := json.Marshal([]session.InstanceData{{Title: "repaired"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairedJSON))

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
		"startup must seed the skip set with the corrupted repo")
	require.NoError(t, m.refreshLocked(), "a polling refresh of a repaired repo must not error")
	require.Empty(t, m.skippedRepos, "a repaired repo drops out of the skip set on the next poll")
	require.NotNil(t, m.instances[daemonInstanceKey("corrupt-r", "repaired")],
		"the repaired repo's session must re-materialize on the poll")
	require.NotNil(t, m.instances[daemonInstanceKey("valid-r", "ok")],
		"the unrelated healthy repo survives the poll")
}

// TestManager_RefreshLocked_KeepsStillCorruptedRepoInSkipSet pins the other
// half of the lifecycle: a startup-skipped repo whose instances.json is STILL
// unreadable stays in m.skippedRepos across the polling refresh (so a
// long-running daemon keeps reporting the drop), and its prior-in-memory
// rows — there were none at startup — stay absent.
func TestManager_RefreshLocked_KeepsStillCorruptedRepoInSkipSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
		"startup must seed the skip set with the corrupted repo")
	require.NoError(t, m.refreshLocked(), "a polling refresh of a still-corrupted repo must not error")
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
		"a still-corrupted repo STAYS in the skip set across the poll")
	require.Nil(t, m.instances[daemonInstanceKey("corrupt-r", "anything")],
		"a still-corrupted repo contributes no rows (there were none at startup to re-hydrate)")
	require.NotNil(t, m.instances[daemonInstanceKey("valid-r", "ok")],
		"the unrelated healthy repo survives the poll")
}

// TestManager_RefreshLocked_DoesNotAddMidlifeCorruptionToSkipSet pins the
// "startup-only" guarantee the reviewer asked us to preserve: a repo that
// newly corrupts MID-LIFE (was healthy at startup, now unreadable on a poll)
// is NOT added to m.skippedRepos, because the polling refresh re-hydrates its
// prior in-memory rows so its sessions stay in the snapshot — only a restart
// re-runs startup and would report it. Mid-life corruption must stay invisible
// to the skip set.
func TestManager_RefreshLocked_DoesNotAddMidlifeCorruptionToSkipSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	liveJSON, err := json.Marshal([]session.InstanceData{{Title: "a-sess"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("repo-a", liveJSON))

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	m.mu.Lock()
	require.Empty(t, m.skippedRepos, "a clean startup seeds no skipped repos")
	require.NotNil(t, m.instances[daemonInstanceKey("repo-a", "a-sess")],
		"the healthy repo's session loads at startup")
	m.mu.Unlock()

	// Corrupt repo-a's instances.json MID-LIFE, after the daemon is already up.
	seedCorruptedRepo(t, "repo-a")

	m.mu.Lock()
	defer m.mu.Unlock()
	require.NoError(t, m.refreshLocked(), "a polling refresh after mid-life corruption must not error")
	require.Empty(t, m.skippedRepos,
		"a mid-life-corrupted repo is NOT added to the skip set (its rows are re-hydrated, so the snapshot is whole)")
	require.NotNil(t, m.instances[daemonInstanceKey("repo-a", "a-sess")],
		"the prior in-memory instance is re-hydrated, so the running session is not silently abandoned")
}
