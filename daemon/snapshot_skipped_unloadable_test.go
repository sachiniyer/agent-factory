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
// rows. It IS reported in the skip set with the rows-failed-to-load reason, so
// the startup orphan sweep defers (the repo's live containers cannot be
// distinguished from genuine orphans). The wire (Snapshot RPC) withholds a
// FRESH startup rows-failure from SkippedRepos (startupRowsFailedRepos) so
// list/get/whoami serve the loadable rows plus a log warning instead of
// refusing — matching master; the polling rewrite of a previously-skipped
// repo's stale reason still surfaces on the wire (#4998 follow-up). Pre-fix,
// reread[repoID] was set the instant json.Unmarshal succeeded, before the
// materialize loop, so retainStillSkipped cleared the skip set on the "parses"
// signal alone (#4812).
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
	require.Equal(t, []SkippedRepo{{RepoID: "r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}}, skipped,
		"a file that parsed but yields no loadable rows is skipped with the rows-failed reason, NOT the corrupted one (the file is fine) so the startup sweep defers; the wire withholds a fresh startup rows-failure (startupRowsFailedRepos)")
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
// stays skipped so list/get/whoami refuse on the wire, AND its reason is
// rewritten from the stale corrupted code to rows-failed-to-load (count 1): the
// file parsed, so "corrupted instances.json" would send the operator to repair a
// file that is fine (#4876).
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
	skipped := skippedRepoIDs(m.skippedRepos)
	validInst := m.instances[daemonInstanceKey("valid-r", "ok")]
	m.mu.Unlock()
	require.Equal(t, []string{"corrupt-r"}, skipped,
		"startup must seed the skip set with the corrupted repo")
	require.NotNil(t, validInst,
		"the healthy repo's session loads at startup")

	// Repair corrupt-r to valid JSON whose row will fail to materialize. The
	// row has a stable ID so it reaches fromInstanceDataForRefresh directly.
	repairJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairJSON))
	// Swap the materializer to fail corrupt-r's row (and only it: valid-r's
	// row re-hydrates from existing without calling the seam).
	failingFromInstanceForRefreshFor(t, "stuck")

	m.mu.Lock()
	refreshErr := m.refreshLocked()
	skippedSet := m.skippedRepos
	stuckInst2 := m.instances[daemonInstanceKey("corrupt-r", "stuck")]
	validInst2 := m.instances[daemonInstanceKey("valid-r", "ok")]
	m.mu.Unlock()
	require.NoError(t, refreshErr, "a parses-but-zero-rows poll must not error")
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}}, skippedSet,
		"a repo whose file parses but yields no loadable rows stays skippedSet, carrying the rows-failed reason and count instead of the stale corrupted one (it is not a repaired snapshot)")
	require.Nil(t, stuckInst2,
		"the unloadable row contributes no instance")
	require.NotNil(t, validInst2,
		"the unrelated healthy repo survives the poll")

	// Wire surface: the Snapshot RPC carries the repo in SkippedRepos with
	// empty Instances, so listSessionsRequest refuses instead of returning []
	// with err == nil — the report's headline misread, now closed. Called
	// WITHOUT m.mu: SnapshotWithSkipped acquires it itself, and sync.Mutex is
	// not reentrant.
	resp := SnapshotResponse{}
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "corrupt-r"}, &resp))
	require.Empty(t, resp.Instances, "the repo has no loadable sessions this poll")
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(resp.SkippedRepos),
		"the Snapshot RPC surfaces the repo as skippedSet so a CLI list can refuse")
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
	refreshErr := m.refreshLocked()
	skipped := skippedRepoIDs(m.skippedRepos)
	m.mu.Unlock()
	require.NoError(t, refreshErr)
	require.Equal(t, []string{"corrupt-r"}, skipped,
		"all rows failing keeps the repo skipped")

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
// the complete answer), so the repo drops from the skip set. A genuinely-empty
// file is short-circuited before the materialize loop (so it is never retracted),
// which is what keeps this unchanged behavior holding under the any-row-failed
// retraction.
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

// TestManager_RefreshLocked_PartialRowFailureStaysSkipped is the 1-of-N repro
// for #4876: a startup-skipped repo repaired to valid JSON whose N=2 rows parse,
// one loads and one fails to materialize, is NOT a complete snapshot, so the
// repo stays skipped (reread retracted on materialized < len(data)) and its
// reason is rewritten to rows-failed-to-load carrying the count. Serving the N-1
// loaded rows as the complete answer is the same lie the zero-rows case is.
// Pre-fix, the repo cleared the skip set (materialized > 0 was treated as a
// repair) and the Snapshot RPC silently served 1 of 2 sessions.
func TestManager_RefreshLocked_PartialRowFailureStaysSkipped(t *testing.T) {
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
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}}, m.skippedRepos,
		"a repo with any unloadable row stays skipped and carries the rows-failed reason and count (1 of N=2)")
	require.NotNil(t, m.instances[daemonInstanceKey("corrupt-r", "loads")],
		"the loadable row is still served")
	require.Nil(t, m.instances[daemonInstanceKey("corrupt-r", "stuck")],
		"the unloadable row is dropped")
}

// TestStartup_ParsesButUnloadableRepoWithheldFromSnapshotWire_MatchesMaster
// pins the wire surface for a FRESH repo whose instances.json parses but whose
// row fails to materialize at STARTUP: the Snapshot RPC's SkippedRepos must NOT
// carry it (master never seeded a fresh startup rows-failure, and the refusal
// names a count with no working remedy — restore/kill resolve titles against
// the in-memory set, which excludes the unloadable row). The loadable rows are
// served (N-1 plus a daemon log warning), NOT a refusal. The startup orphan
// sweep still defers (the row's container is spared via the internal
// startupRowsFailedRepos set, not the wire), and a polling-path previously-
// skipped repo's rows-failed rewrite still surfaces on the wire — but a fresh
// startup rows-failure does not. This keeps the wire behavior at startup
// matching master while the sweep deferral (#4998) still arms.
func TestStartup_ParsesButUnloadableRepoWithheldFromSnapshotWire_MatchesMaster(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	failingFromInstanceForRefreshFor(t, "stuck")
	_ = captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok", ID: "id-ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	unloadableJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("stuck-r", unloadableJSON))

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: m}

	// The internal skip set DOES carry the parses-but-unloadable repo (so the
	// startup orphan sweep defers and spares its live container), but the
	// startupRowsFailedRepos set withholds it from the wire.
	m.mu.Lock()
	skipped := m.skippedRepos
	rowsFailed := m.startupRowsFailedRepos["stuck-r"]
	m.mu.Unlock()
	require.Equal(t, []SkippedRepo{{RepoID: "stuck-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}},
		skipped, "the internal skip set carries the parses-but-unloadable repo so the sweep defers")
	require.True(t, rowsFailed,
		"the fresh startup rows-failure is tracked so the wire withholds it")

	// All-repo Snapshot: the healthy session is served, and the parses-but-
	// unloadable repo is NOT named in SkippedRepos — the wire matches master
	// (no refusal for a fresh startup rows-failure).
	var resp SnapshotResponse
	require.NoError(t, cs.Snapshot(SnapshotRequest{}, &resp))
	require.Equal(t, []string{"ok"}, snapshotFilterTitles(resp.Instances), "the loadable row is served")
	require.Empty(t, resp.SkippedRepos, "a fresh startup rows-failure is withheld from the wire (master behavior)")

	// Scoped to the unloadable repo: Instances is empty and SkippedRepos is
	// empty too, so a scoped list returns [] with err == nil (master behavior)
	// rather than refusing — the refusal's remedy names a count, not a session.
	resp = SnapshotResponse{}
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "stuck-r"}, &resp))
	require.Empty(t, resp.Instances, "the unloadable row is not served")
	require.Empty(t, resp.SkippedRepos, "a scoped read to a fresh rows-failed repo does not refuse (master behavior)")

	// Scoped to the healthy repo: no skip pollutes the scoped read.
	resp = SnapshotResponse{}
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "valid-r"}, &resp))
	require.Equal(t, []string{"ok"}, snapshotFilterTitles(resp.Instances))
	require.Empty(t, resp.SkippedRepos, "a scoped read to a healthy repo reports no skip")
}

// TestStartup_RowsFailedThenCorrupt_SurfacesOnWire pins the fix for a startup
// rows-failure whose instances.json later becomes corrupted or unreadable on the
// polling path. The skip entry's reason is rewritten to corrupted/unreadable by
// retainStillSkipped, which is an actionable state with a real remedy (repair
// the file), so it must surface on the wire even though the repo ID is still in
// startupRowsFailedRepos. Pre-fix, skippedReposScopedLocked checked the repo ID
// alone, so the now-corrupted entry stayed suppressed and list/get/whoami
// silently served an incomplete snapshot for the daemon's lifetime.
func TestStartup_RowsFailedThenCorrupt_SurfacesOnWire(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	failingFromInstanceForRefreshFor(t, "stuck")
	_ = captureWarnings(t)

	unloadableJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("stuck-r", unloadableJSON))

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: m}

	// Startup: the repo's file parses but its row fails to materialize, so it
	// is a fresh rows-failure seeded into startupRowsFailedRepos and withheld
	// from the wire (master behavior).
	m.mu.Lock()
	skipped := m.skippedRepos
	rowsFailed := m.startupRowsFailedRepos["stuck-r"]
	m.mu.Unlock()
	require.Equal(t, []SkippedRepo{{RepoID: "stuck-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}},
		skipped, "startup seeds the rows-failed skip entry")
	require.True(t, rowsFailed,
		"the fresh startup rows-failure is tracked so the wire withholds it")

	var resp SnapshotResponse
	require.NoError(t, cs.Snapshot(SnapshotRequest{}, &resp))
	require.Empty(t, resp.SkippedRepos, "fresh startup rows-failure is withheld from the wire")

	// Poll: the file becomes corrupted (unparseable JSON). retainStillSkipped
	// rewrites the skip entry's reason to corrupted-instances-json, which is
	// actionable, so the wire must surface it even though the repo ID is still
	// in startupRowsFailedRepos.
	seedCorruptedRepo(t, "stuck-r")
	m.mu.Lock()
	refreshErr := m.refreshLocked()
	skippedSet := m.skippedRepos
	m.mu.Unlock()
	require.NoError(t, refreshErr)
	require.NotEmpty(t, skippedSet)
	require.Equal(t, SkippedRepoReasonCorruptedInstancesJSON, skippedSet[0].Reason,
		"the polling path rewrites the skip entry's reason to the file's current state")

	resp = SnapshotResponse{}
	require.NoError(t, cs.Snapshot(SnapshotRequest{}, &resp))
	require.Equal(t, []string{"stuck-r"}, skippedRepoIDs(resp.SkippedRepos),
		"a repo whose reason changed from rows-failed-to-load to corrupted surfaces on the wire (it has a real remedy)")
	require.Equal(t, SkippedRepoReasonCorruptedInstancesJSON, resp.SkippedRepos[0].Reason,
		"the surfaced entry carries the actionable corrupted reason, not the withheld rows-failed one")

	// A scoped read to the now-corrupted repo also surfaces the actionable skip.
	resp = SnapshotResponse{}
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "stuck-r"}, &resp))
	require.Equal(t, []string{"stuck-r"}, skippedRepoIDs(resp.SkippedRepos),
		"a scoped read to the now-corrupted repo surfaces the actionable skip")
}

// TestManager_RefreshLocked_SelfHealsWhenRehydratedRowCountsTowardMaterialized
// pins the baf14817 regression: a multi-row repo that partially fails on one
// poll (one row loads, one fails to materialize) and fully recovers on the next
// must drop from the skip set on the recovery poll. The recovery poll re-hydrates
// the previously-loaded row from in-memory state and materializes the recovered
// row fresh; both rows count toward `materialized`, so `materialized ==
// len(data)` and the repo self-heals. Pre-fix, the re-hydrated row took a
// `continue` that did NOT increment `materialized`, so `materialized(1) <
// len(data)(2)` retracted reread on every poll and the repo stayed skippedSet
// forever while m.instances held every row — `af sessions list/get/whoami`
// refused a now-complete snapshot until a daemon restart or an in-band repair of
// instances.json to [].
//
// The single-row self-heal test above (SelfHealsWhenRowsMaterializeAgain) never
// re-hydrates (its one row never entered memory), so it cannot catch a
// regression that forgets to count re-hydrated rows.
func TestManager_RefreshLocked_SelfHealsWhenRehydratedRowCountsTowardMaterialized(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	seedCorruptedRepo(t, "corrupt-r")
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Two rows with stable IDs so they reach fromInstanceDataForRefresh directly
	// on poll 1 (no prior in-memory rows for a startup-skipped repo).
	partialJSON, err := json.Marshal([]session.InstanceData{
		{Title: "ok", ID: "id-ok"},
		{Title: "stuck", ID: "id-stuck"},
	})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", partialJSON))

	// Poll 1: `ok` loads, `stuck` fails to materialize -> repo stays skipped with
	// the rows-failed reason and count (the partial-loss case the skip set
	// exists to prevent).
	failingFromInstanceForRefreshFor(t, "stuck")
	m.mu.Lock()
	refreshErr := m.refreshLocked()
	skipped := m.skippedRepos
	okInst := m.instances[daemonInstanceKey("corrupt-r", "ok")]
	stuckInst := m.instances[daemonInstanceKey("corrupt-r", "stuck")]
	m.mu.Unlock()
	require.NoError(t, refreshErr)
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}}, skipped,
		"poll 1 (partial failure): repo stays skipped with the rows-failed reason and count")
	require.NotNil(t, okInst, "poll 1: the loadable row is in memory")
	require.Nil(t, stuckInst, "poll 1: the unloadable row is absent from memory")

	// Poll 2: fromInstanceDataForRefresh succeeds for every row. `ok` re-hydrates
	// from m.instances; `stuck` materializes fresh. Counting the re-hydrated row
	// toward `materialized` is what lets `materialized == len(data)` re-arm
	// reread and drop the repo from the skip set.
	stubFromInstanceForRefresh(t)
	m.mu.Lock()
	refreshErr2 := m.refreshLocked()
	okInst2 := m.instances[daemonInstanceKey("corrupt-r", "ok")]
	stuckInst2 := m.instances[daemonInstanceKey("corrupt-r", "stuck")]
	skippedSet := m.skippedRepos
	m.mu.Unlock()
	require.NoError(t, refreshErr2)
	require.NotNil(t, okInst2)
	require.NotNil(t, stuckInst2, "poll 2: the recovered row re-materializes")
	require.Empty(t, skippedSet, "poll 2: a now-complete snapshot self-heals out of the skip set")

	// Wire surface: the Snapshot RPC serves the complete snapshot and carries
	// no SkippedRepos, so `af sessions list/get/whoami --repo <r>` stop refusing.
	// Called WITHOUT m.mu: Snapshot acquires it itself, and sync.Mutex is not
	// reentrant.
	cs := &controlServer{manager: m}
	var resp SnapshotResponse
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "corrupt-r"}, &resp))
	require.Len(t, resp.Instances, 2, "wire: the complete snapshot is served")
	require.Empty(t, skippedRepoIDs(resp.SkippedRepos), "wire: a self-healed repo is no longer refused")
}

// TestManager_RefreshLocked_DroppedLegacyIDRowStaysSkipped pins the case the
// self-heal fix must PRESERVE: a row that is silently DROPPED (never enters
// `next`) keeps the repo skippedSet, because its snapshot is genuinely incomplete.
// Here persistLegacyInstanceID fails with no prior in-memory instance, so the
// row is dropped via the ghost + continue branch (materialized stays 0); the
// drop branch now counts it as a failed row (`failedRows++`, #4998), so
// `materialized(0) < len(data)(1)` retracts reread and the fresh
// rows-failed-to-load entry rewrites the stale startup reason — the partial
// loss is no longer served as the complete answer the skip set exists to
// prevent, and the destructive orphan sweep defers on the same incomplete-view
// principle as a corrupted repo.
//
// This pins the PR's intentional change to the drop branch: before #4998 the
// row was dropped with failedRows==0, leaving the stale corrupted reason and
// (at startup) an empty protected-slug set the orphan sweep could reap from.
// Now the drop seeds the rows-failed-to-load skip entry so the sweep defers.
func TestManager_RefreshLocked_DroppedLegacyIDRowStaysSkipped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	seedCorruptedRepo(t, "corrupt-r")
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// A legacy row (no stable ID) whose durable ID persist fails. No prior
	// in-memory instance exists for a startup-skipped repo, so the row is
	// dropped — not re-hydrated, not materialized, and now counted as a
	// failed row so the skip entry seeds and the sweep defers.
	repairJSON, err := json.Marshal([]session.InstanceData{{Title: "legacy-row", ID: ""}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairJSON))
	prevPersist := persistLegacyInstanceID
	persistLegacyInstanceID = func(string, session.InstanceData) error {
		return fmt.Errorf("forced stable ID persistence failure")
	}
	t.Cleanup(func() { persistLegacyInstanceID = prevPersist })

	m.mu.Lock()
	defer m.mu.Unlock()
	require.NoError(t, m.refreshLocked())
	// The row is dropped, so reread retracts and the fresh
	// rows-failed-to-load entry (failedRows==1) rewrites the stale
	// corrupted-instances-json reason via retainStillSkipped, keeping the
	// repo skipped with the accurate reason and count.
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}}, m.skippedRepos,
		"a dropped legacy-ID row keeps the repo skipped (incomplete snapshot)")
	require.Nil(t, m.instances[daemonInstanceKey("corrupt-r", "legacy-row")], "the dropped row contributes no instance")
}

// TestManager_RefreshLocked_ReplacedIDRowDoesNotCountTowardMaterialized pins
// the discriminator against the same-title/different-stable-id case: a row that
// loaded during an earlier partial poll can be replaced on disk by a new
// session sharing the title but carrying a NEW stable id. The re-hydrate path
// keys only by repo+title, so it reuses the stale in-memory instance; counting
// that toward `materialized` would let `materialized == len(data)` clear the
// skip set while Snapshot still serves the OLD instance and id rather than the
// repaired file's row. persistInstanceData treats a same-title, different-id
// record as a kill/recreate replacement, so such a row is NOT a true
// re-hydration and must not count — the repo stays skipped.
func TestManager_RefreshLocked_ReplacedIDRowDoesNotCountTowardMaterialized(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	_ = captureWarnings(t)

	seedCorruptedRepo(t, "corrupt-r")
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Poll 1: `ok` loads (id-ok-1) and `stuck` fails, so the repo stays skipped
	// and `ok` survives in memory under id-ok-1.
	partialJSON, err := json.Marshal([]session.InstanceData{
		{Title: "ok", ID: "id-ok-1"},
		{Title: "stuck", ID: "id-stuck"},
	})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", partialJSON))

	failingFromInstanceForRefreshFor(t, "stuck")
	m.mu.Lock()
	refreshErr := m.refreshLocked()
	skipped := m.skippedRepos
	okInst := m.instances[daemonInstanceKey("corrupt-r", "ok")]
	stuckInst := m.instances[daemonInstanceKey("corrupt-r", "stuck")]
	m.mu.Unlock()
	require.NoError(t, refreshErr)
	require.Equal(t, []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}}, skipped,
		"poll 1 (partial failure): repo stays skipped with the rows-failed reason and count")
	require.NotNil(t, okInst, "poll 1: the loadable row is in memory")
	require.Equal(t, "id-ok-1", okInst.ID, "poll 1: the loadable row is in memory under its on-disk id")
	require.Nil(t, stuckInst, "poll 1: the unloadable row is absent from memory")

	// Poll 2: every row is now loadable, but `ok` has been replaced on disk by a
	// same-titled session with a NEW stable id (id-ok-2). The re-hydrate path
	// must NOT count this row toward `materialized` — it is a replacement, not
	// the in-memory session coming back — so `materialized(1) < len(data)(2)`
	// keeps the repo skipped rather than serving the stale id as a complete
	// snapshot.
	stubFromInstanceForRefresh(t)
	replacedJSON, err := json.Marshal([]session.InstanceData{
		{Title: "ok", ID: "id-ok-2"},
		{Title: "stuck", ID: "id-stuck"},
	})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", replacedJSON))

	m.mu.Lock()
	refreshErr2 := m.refreshLocked()
	skipped2 := m.skippedRepos
	okInst2 := m.instances[daemonInstanceKey("corrupt-r", "ok")]
	stuckInst2 := m.instances[daemonInstanceKey("corrupt-r", "stuck")]
	m.mu.Unlock()
	require.NoError(t, refreshErr2)
	require.NotEmpty(t, skipped2,
		"poll 2: a same-title/different-id replacement row is not a true re-hydration, so the repo stays skipped")
	require.NotNil(t, okInst2)
	require.Equal(t, "id-ok-1", okInst2.ID,
		"poll 2: the stale in-memory id is retained, not the replaced on-disk id")
	require.NotNil(t, stuckInst2, "poll 2: the recovered row re-materializes")

	// Wire surface: the Snapshot RPC still refuses the partial (stale-id)
	// snapshot, so `af sessions list/get/whoami --repo <r>` do not serve it as
	// complete. Called WITHOUT m.mu: Snapshot acquires it itself.
	cs := &controlServer{manager: m}
	var resp SnapshotResponse
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "corrupt-r"}, &resp))
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(resp.SkippedRepos),
		"wire: a repo with a replaced-id row is still refused, not served as complete")
}
