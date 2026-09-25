package daemon

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// failingFromInstanceForRefreshFor swaps fromInstanceDataForRefresh for a stub
// that fails (returns an error) for any row whose Title is in failTitles and
// succeeds with a bare instance for every other row, mirroring the stub shape
// stubFromInstanceForRefresh uses. The seam keeps these tests tmux/PTY-free: a
// real session.FromInstanceData that errors on every row is exercised by the
// session-layer absent-worktree test; here the failure is staged through the
// same seam the existing skip-set tests use so the daemon skip-set behavior can
// be pinned without a live tmux.
func failingFromInstanceForRefreshFor(t *testing.T, failTitles ...string) {
	t.Helper()
	prev := fromInstanceDataForRefresh
	fail := make(map[string]bool, len(failTitles))
	for _, title := range failTitles {
		fail[title] = true
	}
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		if fail[d.Title] {
			return nil, fmt.Errorf("simulated materialize failure: worktree/session gone")
		}
		return &session.Instance{ID: d.ID, Title: d.Title}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })
}

// TestRefreshDaemonInstances_ParsesButZeroRowsRetractsReread pins the retraction
// at the unit level: a repo whose instances.json parses but whose every row
// fails fromInstanceDataForRefresh is NOT in reread (a non-empty file that
// yields nothing loadable is not a "read AND parsed" repair) and contributes no
// rows. Pre-fix, reread[repoID] was set the instant json.Unmarshal succeeded,
// before the materialize loop, so retainStillSkipped cleared the skip set on the
// "parses" signal alone (#4812).
func TestRefreshDaemonInstances_ParsesButZeroRowsRetractsReread(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	failingFromInstanceForRefreshFor(t, "stuck")
	_ = captureWarnings(t)

	// One row, with a stable ID so it skips the legacy-ID block and goes
	// straight through fromInstanceDataForRefresh (the production drop path).
	unloadableJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("r", unloadableJSON))

	got, _, skipped, reread, err := refreshDaemonInstances(nil)
	require.NoError(t, err, "a file that parses but yields no rows must not fail the refresh")
	require.Empty(t, skipped, "a file that PARSED is not a parse failure, so it is not reported as skipped")
	require.NotContains(t, reread, "r",
		"a parses-but-zero-rows repo is retracted from reread: it is not a repaired snapshot")
	require.Nil(t, got[daemonInstanceKey("r", "stuck")],
		"the unloadable row contributes nothing to the instance map")
}

// TestManager_RefreshLocked_KeepsSkippedRepoWhoseFileParsesButAllRowsFail is the
// headline repro. A repo skipped at startup for a corrupt instances.json is
// "repaired" to valid JSON whose single row fails to materialize. Pre-fix, the
// poll cleared it from the skip set (reread was set on parse alone) and the
// Snapshot RPC served [] with SkippedRepos empty — the silent substitution of a
// partial list as complete the skip set exists to prevent. Post-fix, the repo
// stays skipped so list/get/whoami refuse on the wire.
func TestManager_RefreshLocked_KeepsSkippedRepoWhoseFileParsesButAllRowsFail(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok", ID: "id-ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: m}

	m.mu.Lock()
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
		"startup must seed the skip set with the corrupted repo")
	require.NotNil(t, m.instances[daemonInstanceKey("valid-r", "ok")],
		"the healthy repo's session loads at startup")
	m.mu.Unlock()

	// Repair corrupt-r to valid JSON whose row will fail to materialize. The
	// row has a stable ID so it reaches fromInstanceDataForRefresh directly.
	repairJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairJSON))
	// Swap the materializer to fail corrupt-r's row (and only it: valid-r's
	// row re-hydrates from existing without calling the seam).
	failingFromInstanceForRefreshFor(t, "stuck")

	m.mu.Lock()
	require.NoError(t, m.refreshLocked(), "a parses-but-zero-rows poll must not error")
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonCorruptedInstancesJSON}}, m.skippedRepos,
		"a repo whose file parses but yields no loadable rows stays skipped (it is not a repaired snapshot)")
	require.Nil(t, m.instances[daemonInstanceKey("corrupt-r", "stuck")],
		"the unloadable row contributes no instance")
	require.NotNil(t, m.instances[daemonInstanceKey("valid-r", "ok")],
		"the unrelated healthy repo survives the poll")
	m.mu.Unlock()

	// Wire surface: the Snapshot RPC carries the repo in SkippedRepos with
	// empty Instances, so listSessionsRequest refuses instead of returning []
	// with err == nil — the report's headline misread, now closed. Called
	// WITHOUT m.mu: SnapshotWithSkipped acquires it itself, and sync.Mutex is
	// not reentrant.
	resp := SnapshotResponse{}
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "corrupt-r"}, &resp))
	require.Empty(t, resp.Instances, "the repo has no loadable sessions this poll")
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(resp.SkippedRepos),
		"the Snapshot RPC surfaces the repo as skipped so a CLI list can refuse")
}

// TestManager_RefreshLocked_ParsesButZeroRowsStaysSkippedAcrossPolls pins the
// "stuck" behavior: if every row keeps failing poll after poll, the daemon keeps
// refusing the repo on the wire every poll (once per second at the 1s poll
// interval) — the correct outcome for a permanently-lost worktree, where serving
// [] as if the repo had no sessions would hide the incompleteness forever.
func TestManager_RefreshLocked_ParsesButZeroRowsStaysSkippedAcrossPolls(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	seedCorruptedRepo(t, "corrupt-r")
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	repairJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairJSON))
	failingFromInstanceForRefreshFor(t, "stuck")

	m.mu.Lock()
	defer m.mu.Unlock()
	for poll := 1; poll <= 3; poll++ {
		require.NoError(t, m.refreshLocked())
		require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
			"poll %d: a still-unloadable repo stays skipped (no silent [] on the wire)", poll)
		require.Nil(t, m.instances[daemonInstanceKey("corrupt-r", "stuck")],
			"poll %d: no row materializes while the failure persists", poll)
	}
}

// TestManager_RefreshLocked_SelfHealsWhenRowsMaterializeAgain pins the
// self-healing half: a repo staying skipped because every row fails drops out of
// the skip set on the first poll that materializes at least one row again — so a
// transient total failure (a daemon build bug, a tmux hiccup, worktree
// recovery) is corrected the moment the snapshot is actually complete, not held
// in the skip set forever.
func TestManager_RefreshLocked_SelfHealsWhenRowsMaterializeAgain(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	seedCorruptedRepo(t, "corrupt-r")
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	repairJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairJSON))

	// First poll: every row fails -> repo stays skipped.
	failingFromInstanceForRefreshFor(t, "stuck")
	m.mu.Lock()
	require.NoError(t, m.refreshLocked())
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
		"all rows failing keeps the repo skipped")
	m.mu.Unlock()

	// Restore the real (stubbed-success) materializer for the next poll by
	// re-seeding the success seam over the failing one.
	stubFromInstanceForRefresh(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NoError(t, m.refreshLocked())
	require.Empty(t, m.skippedRepos,
		"a poll that re-materializes the row clears the repo (self-healing)")
	require.NotNil(t, m.instances[daemonInstanceKey("corrupt-r", "stuck")],
		"the recovered row re-materializes on the same poll that clears the repo")
}

// TestManager_RefreshLocked_GenuinelyEmptyFileStillClearsSkipSet pins the case
// the fix must PRESERVE: a startup-skipped repo repaired to a genuinely-empty
// instances.json ([] with no rows) IS a now-complete snapshot (zero sessions is
// the complete answer), so the repo drops from the skip set. The retraction is
// gated on len(data) > 0 specifically so this unchanged behavior holds.
func TestManager_RefreshLocked_GenuinelyEmptyFileStillClearsSkipSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	seedCorruptedRepo(t, "corrupt-r")
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Repair to a genuinely-empty file. Even a failing materializer must not
	// keep the repo skipped: there is nothing to fail to load.
	failingFromInstanceForRefreshFor(t, "stuck")
	require.NoError(t, config.SaveRepoInstances("corrupt-r", json.RawMessage("[]")))

	m.mu.Lock()
	defer m.mu.Unlock()
	require.NoError(t, m.refreshLocked())
	require.Empty(t, m.skippedRepos,
		"a genuinely-empty repaired file clears the skip set (zero sessions is the complete snapshot)")
}

// TestManager_RefreshLocked_PartialRowFailureStillClearsSkipSet pins the case
// the fix deliberately leaves alone: a startup-skipped repo whose file parses
// and materializes SOME rows (N-1 of N) drops from the skip set as today. The
// one dropped row's absence is covered by the existing ghost/on_complete
// recovery surface, and the loaded rows are served — the design's tolerance for
// a single unmaterializable row within an otherwise-loaded repo.
func TestManager_RefreshLocked_PartialRowFailureStillClearsSkipSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	seedCorruptedRepo(t, "corrupt-r")
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Two rows: one will load, one will fail. Both carry stable IDs so they go
	// straight through fromInstanceDataForRefresh (no startup rows to re-hydrate
	// for a skipped repo).
	repairJSON, err := json.Marshal([]session.InstanceData{
		{Title: "loads", ID: "id-loads"},
		{Title: "stuck", ID: "id-stuck"},
	})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairJSON))
	failingFromInstanceForRefreshFor(t, "stuck")

	m.mu.Lock()
	defer m.mu.Unlock()
	require.NoError(t, m.refreshLocked())
	require.Empty(t, m.skippedRepos,
		"a repo that materializes at least one row clears the skip set (it is not a zero-rows non-repair)")
	require.NotNil(t, m.instances[daemonInstanceKey("corrupt-r", "loads")],
		"the loadable row is served")
	require.Nil(t, m.instances[daemonInstanceKey("corrupt-r", "stuck")],
		"the unloadable row is dropped (its slot is covered by the ghost surface)")
}
