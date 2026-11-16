package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// A daemon that accepts the connection and never answers must not park the event
// loop (#6045). The fetcher here behaves like the real transport: it returns only
// when the context it was handed is done. Without a deadline on that context the
// call below never returns; with one, the picker reports the snapshot as degraded
// — the state its destructive D path already gates on — and the Projects section,
// which builds through the same function, still gets its registry rows.
func TestProjectListSnapshotFetchIsBoundedOnTheEventLoop(t *testing.T) {
	h := newTestHome(t)
	oldTimeout := projectSnapshotFetchTimeout
	projectSnapshotFetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { projectSnapshotFetchTimeout = oldTimeout })
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(ctx context.Context) ([]session.InstanceData, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))

	type result struct{ degraded bool }
	done := make(chan result, 1)
	go func() {
		_, _, snapshotErr := h.buildProjectListWithSnapshotStatus()
		done <- result{snapshotErr != nil}
	}()
	select {
	case r := <-done:
		require.True(t, r.degraded, "a timed-out snapshot read is the degraded state, not an empty success")
	case <-time.After(5 * time.Second):
		t.Fatal("buildProjectListWithSnapshotStatus did not return: the event loop would be parked on an unanswered daemon")
	}

	// The sidebar path shares the builder, so it is bounded by the same deadline.
	finished := make(chan struct{})
	go func() { h.refreshSidebarProjects(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("refreshSidebarProjects did not return against an unanswered daemon")
	}
}

// wedgedSnapshotFetcher mimics a local daemon that accepts the unix-socket
// connection and then never writes a response: the transport's read parks on
// a socket that never answers, so the fetch returns ONLY when the caller's
// context — the one switchSnapshotFetchTimeout bounds — is done.
func wedgedSnapshotFetcher(ctx context.Context, _ string) (daemon.SnapshotResponse, error) {
	<-ctx.Done()
	return daemon.SnapshotResponse{}, ctx.Err()
}

// switchProjectNow drives h.switchProject(repo) to completion the way the
// program does: the cmd it returns — the switch's snapshot fetch — runs (here,
// on the test goroutine), and its reply is applied. It returns what applying
// the reply returned, or nil for a switch that started nothing.
func switchProjectNow(t *testing.T, h *home, repo *config.RepoContext) (tea.Model, tea.Cmd) {
	t.Helper()
	_, cmd := h.switchProject(repo)
	if cmd == nil {
		require.Nil(t, h.projectSwitch, "a switch that returned no fetch must not be left pending")
		return h, nil
	}
	msg, ok := cmd().(projectSwitchFetchedMsg)
	require.True(t, ok, "switchProject's cmd must be the switch's snapshot fetch")
	return h.handleProjectSwitchFetched(msg)
}

// settleProjectSwitch completes a switch an entry point left pending — picker
// Enter, a Projects row, a click, the rebind follow-up — by running its fetch
// and applying the reply, as the program would. It is a no-op when no switch is
// pending, so a test of a REFUSED switch calls it too: a switch that should
// have been refused but was started then lands, and the test sees it.
func settleProjectSwitch(t *testing.T, h *home) {
	t.Helper()
	p := h.projectSwitch
	if p == nil {
		return
	}
	msg, ok := projectSwitchFetchCmd(p.ctx, p.gen, p.repo, h.snapshotFetcher)().(projectSwitchFetchedMsg)
	require.True(t, ok)
	h.handleProjectSwitchFetched(msg)
}

// TestSwitchProjectSnapshotFetchIsBoundedOnTheEventLoop is the regression for the
// switch-path half of #6045, now that the fetch runs in a tea.Cmd (#7377):
// switchProject returns at once against a daemon that accepts the connection and
// never answers, and the fetch it starts ends at switchSnapshotFetchTimeout. Its
// expiry is a failed fetch, so the switch reverts and the current project
// survives intact (#1788) instead of "Loading…" never ending.
func TestSwitchProjectSnapshotFetchIsBoundedOnTheEventLoop(t *testing.T) {
	h := newTestHome(t)
	h.sidebar.SetSize(40, 20)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	// Shrink the bound so the regression is fast: the wedged read must return at
	// this deadline, not hold the switch until the process is killed.
	oldTimeout := switchSnapshotFetchTimeout
	switchSnapshotFetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { switchSnapshotFetchTimeout = oldTimeout })

	// Give repo A a concrete root and an open session, as a live TUI would have.
	repoARoot := t.TempDir()
	h.repoRoot = repoARoot
	h.sidebar.SetProjectName(filepath.Base(repoARoot))
	repoAID := h.repoID
	h.store.AddInstance(newSnapshotTestInstance(t, "repoA-session"))
	require.NotNil(t, findSidebarInstance(h, "repoA-session"))

	repoBRoot := t.TempDir()
	repoB := &config.RepoContext{Root: repoBRoot, ID: config.RepoIDFromRoot(repoBRoot)}

	// The incoming project's daemon accepts the connection and never answers.
	h.snapshotFetcher = wedgedSnapshotFetcher

	_, cmd := h.switchProject(repoB)
	require.NotNil(t, cmd, "the switch's fetch must be handed to the program, not run here")

	replies := make(chan tea.Msg, 1)
	go func() { replies <- cmd() }()
	select {
	case msg := <-replies:
		h.Update(msg)
	case <-time.After(5 * time.Second):
		t.Fatal("the switch's fetch did not end at its deadline against a wedged daemon")
	}

	// The switch aborted at the bound: the current project is exactly as it
	// was (#1788), so polls and new sessions keep targeting it.
	assert.Equal(t, repoAID, h.repoID, "a bounded-out switch must not re-scope repoID")
	assert.Equal(t, repoARoot, h.repoRoot, "a bounded-out switch must not re-scope repoRoot")
	assert.Nil(t, h.projectSwitch, "a bounded-out switch is no longer pending")
	require.NotNil(t, findSidebarInstance(h, "repoA-session"),
		"the current project's sessions must survive a switch whose fetch was bounded out")
	assert.Contains(t, h.sidebar.String(), filepath.Base(repoARoot),
		"the sidebar must still name the current project after a bounded-out switch")

	// A session created after the bounded-out switch still targets the project
	// the sidebar is showing.
	h.startNewInstance()
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, repoARoot, h.namingInstance.Path,
		"new sessions must still target the current project after a bounded-out switch")
}

// TestFetchColdStartSnapshotIsBoundedOnTheEventLoop pins the cold-start fetcher
// directly: it shares fetchSnapshotWaitingOutWarmup with the switch's cmd, and a
// wedged read must return at the bound there too — surfacing a real error
// rather than parking newHome forever against a daemon that never answers. The
// warming case (IsDaemonStartingErr) must still retry up to coldStartWarmupWait
// once each bounded attempt returns promptly, so the 2-minute ceiling behaves as
// the whole-operation deadline.
func TestFetchColdStartSnapshotIsBoundedOnTheEventLoop(t *testing.T) {
	h := newTestHome(t)
	oldTimeout := switchSnapshotFetchTimeout
	switchSnapshotFetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { switchSnapshotFetchTimeout = oldTimeout })

	h.snapshotFetcher = wedgedSnapshotFetcher

	done := make(chan error, 1)
	go func() {
		_, _, err := h.fetchColdStartSnapshot(h.repoID)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "a wedged read must surface as a failed fetch, not an empty success")
	case <-time.After(5 * time.Second):
		t.Fatal("fetchColdStartSnapshot did not return against a wedged daemon: the launch/event loop would be parked on an unanswered read")
	}
}

// TestFetchColdStartSnapshotStillWaitsOutWarmingDaemon pins that the per-attempt
// bound did not regress the #829 warm-up retry: while the daemon reports the
// typed "still restoring" error, fetchColdStartSnapshot keeps re-entering until
// the daemon answers — every attempt returns promptly (well inside the bound), so
// the deadline never fires on the warming branch, and coldStartWarmupWait's
// 2-minute ceiling remains the whole-operation deadline.
func TestFetchColdStartSnapshotStillWaitsOutWarmingDaemon(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	// A bound well above the warming retry cadence so the deadline cannot fire on
	// the (promptly-returning) warming branch, but tight enough to fatals a
	// regression that re-appplied the bound to the retry ceiling instead of the
	// per-attempt read.
	oldTimeout := switchSnapshotFetchTimeout
	switchSnapshotFetchTimeout = 5 * time.Second
	t.Cleanup(func() { switchSnapshotFetchTimeout = oldTimeout })

	// No real sleeps between retries.
	prevPoll := coldStartWarmupPoll
	coldStartWarmupPoll = 0
	t.Cleanup(func() { coldStartWarmupPoll = prevPoll })

	calls := 0
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		calls++
		if calls < 3 {
			return daemon.SnapshotResponse{}, errDaemonStarting()
		}
		return daemon.SnapshotResponse{Instances: []session.InstanceData{{Title: "restored"}}}, nil
	}

	data, _, err := h.fetchColdStartSnapshot(h.repoID)
	require.NoError(t, err, "a warming daemon that eventually answers must yield its session list")
	require.Equal(t, 3, calls, "the cold start must keep retrying while the bounded attempts report warming")
	require.Len(t, data, 1)
	assert.Equal(t, "restored", data[0].Title,
		"the warming daemon's session list must come back once it answers")
}
