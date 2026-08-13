package daemon

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// prSweepFixture is one manager tracking one local-worktree session, the
// minimal world the sweep operates on.
type prSweepFixture struct {
	manager  *Manager
	instance *session.Instance
	repoID   string
}

func newPRSweepFixture(t *testing.T) *prSweepFixture {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, manager.RestoreInstances())

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "pr-sweep",
		Path:    repoPath,
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, repoPath, "pr-sweep", "af/pr-sweep", "", true, false)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	// Seed the on-disk record the write path updates in place:
	// persistInstanceData refuses to invent rows, so the projection must already
	// know this session (as it would after any real create).
	seedDiskInstanceWithID(t, repo.ID, inst.ID, "pr-sweep", repoPath)

	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repo.ID, "pr-sweep")] = inst
	manager.mu.Unlock()
	return &prSweepFixture{manager: manager, instance: inst, repoID: repo.ID}
}

// stubPRFetch swaps the sweep's fetch seam and counts invocations.
func stubPRFetch(t *testing.T, info *sessiongit.PRInfo, err error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	prev := prInfoFetchFn
	prInfoFetchFn = func(repoPath, branch string) (*sessiongit.PRInfo, error) {
		calls.Add(1)
		if info == nil {
			return nil, err
		}
		copied := *info
		return &copied, err
	}
	t.Cleanup(func() { prInfoFetchFn = prev })
	return &calls
}

// TestPRInfoSweepDiscoversWithoutAnyClient pins the #3232 fix at its root: a
// session no TUI has ever looked at gets its PR discovered by the daemon alone —
// recorded on the live instance AND persisted, so every surface reads it from
// the projection.
func TestPRInfoSweepDiscoversWithoutAnyClient(t *testing.T) {
	f := newPRSweepFixture(t)
	calls := stubPRFetch(t, &sessiongit.PRInfo{Number: 41, Title: "fix: sweep", URL: "https://example.com/pr/41", State: "OPEN"}, nil)

	f.manager.RefreshStalePRInfo()

	require.Equal(t, int32(1), calls.Load(), "one eligible stale session, one fetch")
	got := f.instance.GetPRInfo()
	require.NotNil(t, got, "the discovered PR must land on the instance")
	require.Equal(t, 41, got.Number)
	require.Equal(t, "af/pr-sweep", got.Branch, "the result must be bound to the branch it was looked up for")

	persisted := readPersistedPRInfo(t, f.repoID)
	require.Equal(t, 41, persisted.Number, "the discovery must persist, not just sit in memory")
	require.Equal(t, "OPEN", persisted.State)
}

// TestPRInfoSweepSkipsFreshEntries pins the debounce that keeps the sweep from
// duplicating a client's just-made refresh: SetPRInfo (the write every producer
// funnels through) bumps the freshness clock, so a fresh entry costs no fetch.
func TestPRInfoSweepSkipsFreshEntries(t *testing.T) {
	f := newPRSweepFixture(t)
	calls := stubPRFetch(t, nil, nil)

	f.instance.MarkPRInfoFetched()
	f.manager.RefreshStalePRInfo()
	require.Equal(t, int32(0), calls.Load(), "a fresh entry must not be re-fetched")

	f.instance.SetPRInfoFetchedAtForTest(time.Now().Add(-time.Hour))
	f.manager.RefreshStalePRInfo()
	require.Equal(t, int32(1), calls.Load(), "a stale entry must be fetched")
}

// TestPRInfoSweepUnchangedResultWritesNothing pins the churn guard: re-learning
// the same PR must not persist or publish anything — otherwise every sweep
// emits a session.updated per session with zero information in it.
func TestPRInfoSweepUnchangedResultWritesNothing(t *testing.T) {
	f := newPRSweepFixture(t)
	stubPRFetch(t, &sessiongit.PRInfo{Number: 41, Title: "fix: sweep", URL: "https://example.com/pr/41", State: "OPEN"}, nil)

	f.manager.RefreshStalePRInfo()
	require.NotNil(t, f.instance.GetPRInfo())

	// Age the entry and corrupt the persisted copy: if the second sweep
	// persisted again, the corruption would be healed and the assertion below
	// would catch the extra write.
	f.instance.SetPRInfoFetchedAtForTest(time.Now().Add(-time.Hour))
	markPersistedPRInfoState(t, f.repoID, "SENTINEL")

	f.manager.RefreshStalePRInfo()
	persisted := readPersistedPRInfo(t, f.repoID)
	require.Equal(t, "SENTINEL", persisted.State, "an unchanged result must not re-persist")
}

// TestPRInfoSweepRespectsLifecycleAdmission pins the #3231 discipline for the
// daemon's own producer: while the daemon is quiescing for an upgrade hand-off,
// the sweep neither fetches nor writes — the same answer every client mutation
// gets from the admission gate.
func TestPRInfoSweepRespectsLifecycleAdmission(t *testing.T) {
	f := newPRSweepFixture(t)
	calls := stubPRFetch(t, &sessiongit.PRInfo{Number: 7}, nil)

	f.manager.lifecycle.markQuiescing()
	f.manager.RefreshStalePRInfo()

	require.Equal(t, int32(0), calls.Load(), "a quiescing daemon must not run PR discovery")
	require.Nil(t, f.instance.GetPRInfo())
}

// TestPRInfoSweepSkipsIneligibleRows pins the eligibility gate: archived rows
// and worktrees without a branch have nothing to discover, and fetching for
// them would only earn refusals from the write path.
func TestPRInfoSweepSkipsIneligibleRows(t *testing.T) {
	f := newPRSweepFixture(t)
	calls := stubPRFetch(t, &sessiongit.PRInfo{Number: 7}, nil)

	detached, err := sessiongit.NewGitWorktreeFromStorage(f.instance.Path, f.instance.Path, "pr-sweep", "", "", true, false)
	require.NoError(t, err)
	f.instance.SetGitWorktreeForTest(detached)
	f.manager.RefreshStalePRInfo()
	require.Equal(t, int32(0), calls.Load(), "a detached-HEAD worktree has no branch to look up")
}

// TestPRInfoSweepFailedFetchWaitsAFullWindow pins the retry discipline: a fetch
// error (network down) bumps the freshness clock anyway, so the next sweep tick
// does not retry immediately.
func TestPRInfoSweepFailedFetchWaitsAFullWindow(t *testing.T) {
	f := newPRSweepFixture(t)
	calls := stubPRFetch(t, nil, fmt.Errorf("gh: network unreachable"))

	f.manager.RefreshStalePRInfo()
	require.Equal(t, int32(1), calls.Load())
	f.manager.RefreshStalePRInfo()
	require.Equal(t, int32(1), calls.Load(), "a failed fetch must wait out the staleness window, not retry every sweep")
}

// readPersistedPRInfo loads the repo's persisted instance list and returns the
// pr-sweep session's stored PR projection.
func readPersistedPRInfo(t *testing.T, repoID string) session.PRInfoData {
	t.Helper()
	data := loadPersistedInstances(t, repoID)
	for _, d := range data {
		if d.Title == "pr-sweep" {
			return d.PRInfo
		}
	}
	t.Fatal("pr-sweep not found in persisted instances")
	return session.PRInfoData{}
}

func markPersistedPRInfoState(t *testing.T, repoID, state string) {
	t.Helper()
	data := loadPersistedInstances(t, repoID)
	for i := range data {
		if data[i].Title == "pr-sweep" {
			data[i].PRInfo.State = state
		}
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, raw))
}

func loadPersistedInstances(t *testing.T, repoID string) []session.InstanceData {
	t.Helper()
	raw, err := config.LoadState().GetInstances(repoID)
	require.NoError(t, err)
	var data []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &data))
	return data
}
