package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// The sweep refuses to run without `gh` on PATH (discovery unavailable must
	// not read as "no PR"). The fetch itself is stubbed in every test, so a
	// do-nothing executable satisfies the availability probe deterministically,
	// whatever the CI runner has installed.
	ghDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ghDir, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	prInfoFetchFn = func(_ context.Context, repoPath, branch string) (*sessiongit.PRInfo, error) {
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

	f.manager.refreshStalePRInfo(context.Background())

	require.Equal(t, int32(1), calls.Load(), "one eligible stale session, one fetch")
	got := f.instance.GetPRInfo()
	require.NotNil(t, got, "the discovered PR must land on the instance")
	require.Equal(t, 41, got.Number)
	require.Equal(t, "af/pr-sweep", got.Branch, "the result must be bound to the branch it was looked up for")

	persisted := readPersistedPRInfo(t, f.repoID)
	require.Equal(t, 41, persisted.Number, "the discovery must persist, not just sit in memory")
	require.Equal(t, "OPEN", persisted.State)
	require.Equal(t, "af/pr-sweep", persisted.Branch,
		"the #921 provenance must survive the write path — a branchless record is never trusted for destructive decisions")
}

// TestPRInfoSweepSkipsFreshEntries pins the shared freshness clock that keeps
// the background sweep from duplicating a selected-session refresh poke.
func TestPRInfoSweepSkipsFreshEntries(t *testing.T) {
	f := newPRSweepFixture(t)
	calls := stubPRFetch(t, nil, nil)

	f.instance.MarkPRInfoFetched()
	f.manager.refreshStalePRInfo(context.Background())
	require.Equal(t, int32(0), calls.Load(), "a fresh entry must not be re-fetched")

	f.instance.SetPRInfoFetchedAtForTest(time.Now().Add(-time.Hour))
	f.manager.refreshStalePRInfo(context.Background())
	require.Equal(t, int32(1), calls.Load(), "a stale entry must be fetched")
}

// TestRefreshPRInfoPokeDiscoversAndDebounces pins #3296's ownership boundary:
// a client supplies only session identity, the daemon performs discovery and
// records the projection, and another poke inside the selected-session window
// does not launch a second gh lookup.
func TestRefreshPRInfoPokeDiscoversAndDebounces(t *testing.T) {
	f := newPRSweepFixture(t)
	calls := stubPRFetch(t, &sessiongit.PRInfo{
		Number: 42, Title: "daemon-owned", URL: "https://example.com/pr/42", State: "OPEN",
	}, nil)

	err := f.manager.RefreshPRInfo(context.Background(), RefreshPRInfoRequest{
		RepoID: f.repoID, Title: f.instance.Title, ID: f.instance.ID,
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, 42, f.instance.GetPRInfo().Number,
		"the daemon must discover and project PR data; the client never supplies it")

	err = f.manager.RefreshPRInfo(context.Background(), RefreshPRInfoRequest{
		RepoID: f.repoID, Title: f.instance.Title, ID: f.instance.ID,
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load(),
		"a second poke inside the server-owned debounce window must not fetch")

	// The TUI ticks every minute. Leave enough margin that scheduler jitter does
	// not turn a nominally minute-old entry into an every-other-tick refresh.
	f.instance.SetPRInfoFetchedAtForTest(time.Now().Add(-59 * time.Second))
	err = f.manager.RefreshPRInfo(context.Background(), RefreshPRInfoRequest{
		RepoID: f.repoID, Title: f.instance.Title, ID: f.instance.ID,
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load(), "a minute-cadence poke must refresh on every tick")
}

// TestRefreshPRInfoPokeClaimsDebounceAtomically covers concurrent clients. The
// first poke claims freshness before gh starts; a second TUI/web client that
// arrives while that lookup is blocked must return without launching another.
func TestRefreshPRInfoPokeClaimsDebounceAtomically(t *testing.T) {
	f := newPRSweepFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	prev := prInfoFetchFn
	prInfoFetchFn = func(context.Context, string, string) (*sessiongit.PRInfo, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil, nil
	}
	t.Cleanup(func() { prInfoFetchFn = prev })

	req := RefreshPRInfoRequest{RepoID: f.repoID, Title: f.instance.Title, ID: f.instance.ID}
	firstDone := make(chan error, 1)
	go func() { firstDone <- f.manager.RefreshPRInfo(context.Background(), req) }()
	<-entered

	secondDone := make(chan error, 1)
	go func() { secondDone <- f.manager.RefreshPRInfo(context.Background(), req) }()
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("a debounced poke must return without waiting for the in-flight fetch")
	}
	require.Equal(t, int32(1), calls.Load(), "the debounce claim must be atomic")
	close(release)
	require.NoError(t, <-firstDone)
}

func TestRefreshPRInfoPokeStopsOnCancel(t *testing.T) {
	f := newPRSweepFixture(t)
	entered := make(chan struct{})
	prev := prInfoFetchFn
	prInfoFetchFn = func(ctx context.Context, _, _ string) (*sessiongit.PRInfo, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { prInfoFetchFn = prev })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- f.manager.RefreshPRInfo(ctx, RefreshPRInfoRequest{
			RepoID: f.repoID, Title: f.instance.Title, ID: f.instance.ID,
		})
	}()
	<-entered
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("a cancelled refresh poke must stop its in-flight discovery")
	}
	require.Nil(t, f.instance.GetPRInfo(), "an abandoned lookup must project nothing")

	var retryCalls atomic.Int32
	prInfoFetchFn = func(context.Context, string, string) (*sessiongit.PRInfo, error) {
		retryCalls.Add(1)
		return nil, nil
	}
	require.NoError(t, f.manager.RefreshPRInfo(context.Background(), RefreshPRInfoRequest{
		RepoID: f.repoID, Title: f.instance.Title, ID: f.instance.ID,
	}))
	require.Equal(t, int32(1), retryCalls.Load(),
		"cancellation must release its claim so another producer can retry immediately")
}

func TestRefreshPRInfoPokeReportsGhUnavailable(t *testing.T) {
	f := newPRSweepFixture(t)
	t.Setenv("PATH", t.TempDir())

	err := f.manager.RefreshPRInfo(context.Background(), RefreshPRInfoRequest{
		RepoID: f.repoID, Title: f.instance.Title, ID: f.instance.ID,
	})
	require.ErrorContains(t, err, "gh")
	require.Nil(t, f.instance.GetPRInfo())
}

// TestPRInfoSweepUnchangedResultWritesNothing pins the churn guard: re-learning
// the same PR must not persist or publish anything — otherwise every sweep
// emits a session.updated per session with zero information in it.
func TestPRInfoSweepUnchangedResultWritesNothing(t *testing.T) {
	f := newPRSweepFixture(t)
	stubPRFetch(t, &sessiongit.PRInfo{Number: 41, Title: "fix: sweep", URL: "https://example.com/pr/41", State: "OPEN"}, nil)

	f.manager.refreshStalePRInfo(context.Background())
	require.NotNil(t, f.instance.GetPRInfo())

	// Age the entry and corrupt the persisted copy: if the second sweep
	// persisted again, the corruption would be healed and the assertion below
	// would catch the extra write.
	f.instance.SetPRInfoFetchedAtForTest(time.Now().Add(-time.Hour))
	markPersistedPRInfoState(t, f.repoID, "SENTINEL")

	f.manager.refreshStalePRInfo(context.Background())
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
	f.manager.refreshStalePRInfo(context.Background())

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
	f.manager.refreshStalePRInfo(context.Background())
	require.Equal(t, int32(0), calls.Load(), "a detached-HEAD worktree has no branch to look up")
}

// TestPRInfoSweepFailedFetchWaitsAFullWindow pins the retry discipline: a fetch
// error (network down) bumps the freshness clock anyway, so the next sweep tick
// does not retry immediately.
func TestPRInfoSweepFailedFetchWaitsAFullWindow(t *testing.T) {
	f := newPRSweepFixture(t)
	calls := stubPRFetch(t, nil, fmt.Errorf("gh: network unreachable"))

	f.manager.refreshStalePRInfo(context.Background())
	require.Equal(t, int32(1), calls.Load())
	f.manager.refreshStalePRInfo(context.Background())
	require.Equal(t, int32(1), calls.Load(), "a failed fetch must wait out the staleness window, not retry every sweep")
}

// TestPRInfoSweepSkipsWhenGhUnavailable pins the availability guard (#3287
// review): with `gh` gone from PATH, discovery is unavailable — not answering
// "no PR" — so the sweep must not run at all, and a previously recorded badge
// survives instead of being cleared by the indistinguishable (nil, nil).
func TestPRInfoSweepSkipsWhenGhUnavailable(t *testing.T) {
	f := newPRSweepFixture(t)
	calls := stubPRFetch(t, &sessiongit.PRInfo{Number: 41, Title: "fix: sweep", URL: "https://example.com/pr/41", State: "OPEN"}, nil)

	f.manager.refreshStalePRInfo(context.Background())
	require.Equal(t, int32(1), calls.Load())
	require.NotNil(t, f.instance.GetPRInfo())

	f.instance.SetPRInfoFetchedAtForTest(time.Now().Add(-time.Hour))
	t.Setenv("PATH", t.TempDir()) // a PATH with no gh in it
	f.manager.refreshStalePRInfo(context.Background())

	require.Equal(t, int32(1), calls.Load(), "no fetch may run while gh is unavailable")
	require.NotNil(t, f.instance.GetPRInfo(), "the cached badge must survive gh unavailability")
	require.Equal(t, 41, readPersistedPRInfo(t, f.repoID).Number, "the persisted badge must survive gh unavailability")
}

// TestPRInfoSweepDiscardsResultWhenNewerProducerLanded pins the overwrite race
// (#3287 review): a producer that lands while the sweep's `gh` call is in
// flight (the TUI's selected-session refresh) advances the generation, and the
// sweep's now-older result must be discarded, not recorded over it.
func TestPRInfoSweepDiscardsResultWhenNewerProducerLanded(t *testing.T) {
	f := newPRSweepFixture(t)
	var calls atomic.Int32
	prev := prInfoFetchFn
	prInfoFetchFn = func(_ context.Context, _, branch string) (*sessiongit.PRInfo, error) {
		calls.Add(1)
		// A newer producer lands mid-fetch.
		f.instance.SetPRInfo(&sessiongit.PRInfo{Number: 99, State: "MERGED", Branch: branch})
		return &sessiongit.PRInfo{Number: 41, State: "OPEN"}, nil
	}
	t.Cleanup(func() { prInfoFetchFn = prev })

	f.manager.refreshStalePRInfo(context.Background())

	require.Equal(t, int32(1), calls.Load())
	got := f.instance.GetPRInfo()
	require.NotNil(t, got)
	require.Equal(t, 99, got.Number, "the sweep's stale result must not overwrite the newer producer's write")
}

// TestPRInfoSweepRechecksAdmissionBeforeRecording pins the quiesce race (#3287
// review): upgrade activation can close admission while `gh` runs, and the
// sweep's guarded write is a direct Manager call — no RPC gate fronts it — so
// the recheck immediately before recording is what keeps a mutation
// from persisting after admission closed.
func TestPRInfoSweepRechecksAdmissionBeforeRecording(t *testing.T) {
	f := newPRSweepFixture(t)
	var calls atomic.Int32
	prev := prInfoFetchFn
	prInfoFetchFn = func(context.Context, string, string) (*sessiongit.PRInfo, error) {
		calls.Add(1)
		f.manager.lifecycle.markQuiescing()
		return &sessiongit.PRInfo{Number: 41, State: "OPEN"}, nil
	}
	t.Cleanup(func() { prInfoFetchFn = prev })

	f.manager.refreshStalePRInfo(context.Background())

	require.Equal(t, int32(1), calls.Load())
	require.Nil(t, f.instance.GetPRInfo(), "a result fetched before quiescing must not be recorded after it")
}

// TestPRInfoSweepStopsOnCancel pins the shutdown property (#3287 review): a
// sweep mid-fetch must observe cancellation — the in-flight lookup aborts via
// the context handed to `gh`, nothing is recorded from it, and no further
// entry is fetched — so daemon shutdown never waits out sessions × the network
// timeout ahead of its final persistence.
func TestPRInfoSweepStopsOnCancel(t *testing.T) {
	f := newPRSweepFixture(t)

	second, err := session.NewInstance(session.InstanceOptions{
		Title:   "pr-sweep-2",
		Path:    f.instance.Path,
		Program: "claude",
	})
	require.NoError(t, err)
	second.SetBackend(session.NewFakeBackend())
	second.SetStartedForTest(true)
	second.SetStatusForTest(session.Running)
	gw, err := sessiongit.NewGitWorktreeFromStorage(f.instance.Path, f.instance.Path, "pr-sweep-2", "af/pr-sweep-2", "", true, false)
	require.NoError(t, err)
	second.SetGitWorktreeForTest(gw)
	f.manager.mu.Lock()
	f.manager.instances[daemonInstanceKey(f.repoID, "pr-sweep-2")] = second
	f.manager.mu.Unlock()

	var calls atomic.Int32
	prev := prInfoFetchFn
	prInfoFetchFn = func(ctx context.Context, repoPath, branch string) (*sessiongit.PRInfo, error) {
		calls.Add(1)
		<-ctx.Done() // model a stalled `gh` that only the context can end
		return nil, ctx.Err()
	}
	t.Cleanup(func() { prInfoFetchFn = prev })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.manager.refreshStalePRInfo(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled sweep must return promptly, not wait out the network timeout")
	}
	require.Equal(t, int32(1), calls.Load(), "cancellation must stop the sweep before the next entry")
	require.Nil(t, f.instance.GetPRInfo(), "nothing may be recorded from an abandoned lookup")
	require.Nil(t, second.GetPRInfo())
}

// TestPRInfoSweepAbandonsLockWaitOnCancel pins the round-three shutdown hole
// (#3287 review): with a teardown holding the session's op-lock, the guarded
// write must abandon its lock wait when the sweep's context ends instead of
// blocking wg.Wait past the stop path's kill escalation.
func TestPRInfoSweepAbandonsLockWaitOnCancel(t *testing.T) {
	f := newPRSweepFixture(t)
	stubPRFetch(t, &sessiongit.PRInfo{Number: 41, State: "OPEN"}, nil)

	opLock := f.manager.opLockFor(daemonInstanceKey(f.repoID, "pr-sweep"))
	opLock.Lock() // a kill/archive/restore owns the session for many seconds
	defer opLock.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.manager.refreshStalePRInfo(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled guarded write must abandon its lock wait promptly")
	}
	require.Nil(t, f.instance.GetPRInfo(), "an abandoned write must record nothing")
}

// TestSetPRInfoFailedPersistLeavesGenerationUntouched pins the daemon half of
// the rollback contract (#3287 review): an RPC write whose persist fails must
// leave the instance's PR-info generation exactly as it found it, so a sweep
// waiting on the op-lock does not discard its still-valid result over a write
// that committed nothing.
func TestSetPRInfoFailedPersistLeavesGenerationUntouched(t *testing.T) {
	f := newPRSweepFixture(t)
	prevHook := testHookPersistInstanceData
	testHookPersistInstanceData = func(string, session.InstanceData) error {
		return fmt.Errorf("persist refused by test")
	}
	t.Cleanup(func() { testHookPersistInstanceData = prevHook })

	genBefore := f.instance.PRInfoGeneration()
	err := f.manager.SetPRInfo(SetPRInfoRequest{
		RepoID: f.repoID, Title: "pr-sweep", ID: f.instance.ID,
		PRInfo: session.PRInfoData{Number: 7, State: "OPEN", Branch: "af/pr-sweep"},
	})
	require.Error(t, err, "the failed persist must surface")
	require.Nil(t, f.instance.GetPRInfo(), "the rolled-back value must not remain in memory")
	require.Equal(t, genBefore, f.instance.PRInfoGeneration(),
		"a write that committed nothing must not advance the generation")
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
