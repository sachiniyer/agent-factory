package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestHome builds a minimal home with real UI components and a tempdir-
// scoped storage. AGENT_FACTORY_HOME is redirected so nothing escapes into
// the user's real config dir.
func newTestHome(t *testing.T) *home {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	// TUI task CRUD routes through the daemon (#1029 PR 6). Point the write
	// seams at the direct task writers so tests never dial — or spawn — a real
	// daemon, while the existing disk-assertion tests (which read tasks.json
	// after driving the handlers) keep exercising the save orchestration
	// unchanged. Tests asserting the daemon-dispatch itself swap in a recorder.
	t.Cleanup(SetTaskAdderForTest(task.AddTask))
	// task.UpdateTask returns the merged record; the seam only needs the error,
	// so adapt it to the field-level updater signature (#1700).
	t.Cleanup(SetTaskUpdaterForTest(func(id string, update task.TaskUpdate) error {
		_, err := task.UpdateTask(id, update)
		return err
	}))
	t.Cleanup(SetTaskRemoverForTest(task.RemoveTask))
	t.Cleanup(SetLocalSessionPreflightForTest(func(*config.Config, string) error { return nil }))

	// The tab + PR-info mutations now route through daemon RPCs (#960 PR 2).
	// Stub the seams with safe defaults so tests that incidentally trigger them
	// never dial — or spawn — a real daemon. Tests exercising these paths
	// override the relevant seam. createTab/closeTab default to an error so an
	// unstubbed mutation fails loudly rather than reaching the daemon; setPRInfo
	// defaults to a no-op so the in-memory PR-badge path stays exercisable.
	t.Cleanup(SetTabCreatorForTest(func(title, repoID string) (string, error) {
		return "", fmt.Errorf("createShellTabThroughDaemon not stubbed in test")
	}))
	t.Cleanup(SetTabCloserForTest(func(title, repoID, tabName string) error {
		return fmt.Errorf("closeTabThroughDaemon not stubbed in test")
	}))
	t.Cleanup(SetPRInfoSetterForTest(func(title, repoID string, info session.PRInfoData) error {
		return nil
	}))

	// The live-termpane bind would dial a real WS PTY stream (#1592 PR6). Default
	// the factory to an inert fake so a test that incidentally reaches a bind
	// (mock-backed instances answer has-session) binds harmlessly instead of
	// dialing the daemon. Tests exercising the live path swap in a recording fake.
	origLiveTerm := newLiveTermPaneFn
	newLiveTermPaneFn = func(title, repoID string, tab, width, height int) liveTermAttachment {
		return newFakeLiveTerm()
	}
	t.Cleanup(func() { newLiveTermPaneFn = origLiveTerm })

	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	proj := store.NewProjection()

	state := config.DefaultState()
	repoID := "test-repo-" + filepath.Base(tmp)

	h := &home{
		ctx:       context.Background(),
		state:     stateDefault,
		appConfig: config.DefaultConfig(),
		appState:  state,
		// Default the snapshot fetcher to a failing stub so a test that incidentally
		// runs the tick loop (e.g. the teatest e2e harness) never dials — or spawns
		// — a real daemon. A FAILING fetch is the correct default: it mirrors
		// "no daemon reachable", so handleSnapshot leaves the sidebar intact rather
		// than reconciling it to an empty snapshot and wiping preloaded instances.
		// (A nil,nil success would do exactly that — the #960 PR 4 race-fix must not
		// regress this.) Tests exercising the fetch path assign their own fake to
		// h.snapshotFetcher. The fetcher is a per-home field, not a shared global, so
		// parallel tests can't race each other's swaps.
		snapshotFetcher: func(string) (daemon.SnapshotResponse, error) {
			return daemon.SnapshotResponse{}, fmt.Errorf("snapshot fetcher not stubbed in test")
		},
		// Default the #1160 poll-pause seams to no-ops so a test that incidentally
		// drives the attach heartbeat never dials — or spawns — a real daemon.
		// Per-home fields (not shared globals), so parallel tests can't race each
		// other's swaps; tests exercising the pause/resume path assign their own
		// recorders to h.pauseStatusPoll / h.resumeStatusPoll.
		pauseStatusPoll:  func(string, string) error { return nil },
		resumeStatusPoll: func(string, string) error { return nil },
		store:            proj,
		spinner:          spin,
		repoID:           repoID,
	}
	// Content capture routes through the daemon Preview RPC since #1592 PR6.
	// Resolve the title back to the in-store instance and read it directly, so the
	// TabPane state-machine tests exercise the same content path (previewTextBackend
	// etc.) without dialing a real daemon. Per-home field; set before any pane opens
	// so the source captures it.
	h.previewFetcher = testPreviewFetcher(h)

	h.sidebar = ui.NewSidebar(&h.spinner, false, proj)
	wireTestPanes(h, proj)
	return h
}

// testPreviewFetcher resolves a Preview request's title back to the in-store
// instance and captures from it, the test stand-in for the daemon's server-side
// capture. tmux.ErrSessionGone maps to gone=true, mirroring the daemon handler.
func testPreviewFetcher(h *home) func(daemon.PreviewRequest) (string, bool, error) {
	return func(req daemon.PreviewRequest) (string, bool, error) {
		inst := h.store.GetInstanceByTitle(req.Title)
		if inst == nil {
			return "", false, nil
		}
		var content string
		var err error
		switch {
		case req.Tab == 0 && req.Full:
			content, err = inst.PreviewFullHistory()
		case req.Tab == 0:
			content, err = inst.Preview()
		case req.Full:
			content, err = inst.PreviewTabFullHistory(req.Tab)
		default:
			content, err = inst.PreviewTab(req.Tab)
		}
		if errors.Is(err, tmux.ErrSessionGone) {
			return "", true, nil
		}
		return content, false, err
	}
}

// wireTestPanes installs the workspace panes + focus ring on a hand-built
// test home, mirroring newHome's wiring (#1024 PR 4). The workspace starts
// with no open panes (#1088); tests open them via the pane verbs or
// openTestPane.
func wireTestPanes(h *home, proj *store.Projection) {
	if h.menu == nil {
		h.menu = ui.NewMenu()
	}
	if h.errBox == nil {
		h.errBox = ui.NewErrBox()
	}
	if h.alarmBanner == nil {
		h.alarmBanner = ui.NewAlarmBanner()
	}
	h.paneWindows = make(map[int]*ui.TabbedWindow)
	h.lastPaneCapture = make(map[int]time.Time)
	h.liveTerms = make(map[int]liveTermAttachment)
	h.liveKeys = make(map[int]string)
	// The startup auto-open is production-launch sugar; tests drive the pane
	// verbs explicitly, so latch it off for determinism.
	h.initialPaneOpened = true
	h.automations = ui.NewAutomationsPane(proj)
	h.projects = ui.NewProjectsPane()
	h.statusBar = ui.NewStatusBar(h.menu, h.errBox)
	h.hooksPane = ui.NewHooksPane()
	h.ring = layout.NewRing(layout.RegionTree, layout.RegionAutomations, layout.RegionProjects)
	h.zones = zones.NewRegistry()
	h.mouseClock = time.Now
	h.wireZoneRegistry()
	h.syncFocus()
	h.rememberTUIViewState()
}

// openTestPane opens (instance, tab) as a pane the way the `s` verb does —
// store binding + window + relayout + focus — and returns the pane.
func openTestPane(t *testing.T, h *home, inst *session.Instance, tab int) *store.OpenPane {
	t.Helper()
	if p := h.store.FindOpenPane(inst, tab); p != nil {
		h.store.TouchOpenPane(p)
		h.relayout()
		h.focusRegion(layout.PaneRegion(p.ID()))
		return p
	}
	p := h.openPaneWindow(inst, tab)
	require.NotNil(t, p)
	h.relayout()
	h.focusRegion(layout.PaneRegion(p.ID()))
	return p
}

// requireTUIInstancesEmpty asserts the TUI's repo instances file holds no
// records. This is the structural single-writer guarantee (#960 PR 6): the TUI
// no longer holds a session.Storage handle and has no write path, so its repo's
// instances.json is never written by the TUI. Reads the store directly because
// the home struct no longer carries a Storage field.
func requireTUIInstancesEmpty(t *testing.T, h *home) {
	t.Helper()
	raw, err := config.DefaultState().GetInstances(h.repoID)
	require.NoError(t, err)
	empty := len(raw) == 0 || string(raw) == "[]" || string(raw) == "null"
	require.True(t, empty,
		"the TUI must hold no instances.json write path (daemon is the sole writer, #960): got %s", string(raw))
}

// newLoadingInstance returns an instance in Loading status, matching the UI
// placeholder shown while the daemon starts a session.
func newLoadingInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStatusForTest(session.Loading)
	return inst
}

// ----------------------------------------------------------------------------
// Regression tests for issue #310 (sachiniyer/agent-factory):
// "instance creation (both remote and local) should be async, and should not
//  interfere with going to a different instance".
//
// The handler under test is the instanceStartedMsg case in app/app.go.
// ----------------------------------------------------------------------------

// TestInstanceStarted_Success_UserStillWatching verifies the original post-
// creation UX: when the user's selection is still on the newly-created
// instance and they haven't navigated into any other state, the attach-help
// modal pops and the status flips to Running.
func TestInstanceStarted_Success_UserStillWatching(t *testing.T) {
	h := newTestHome(t)
	inst := newLoadingInstance(t, "new-session")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.Update(instanceStartedMsg{instance: inst, err: nil})

	assert.Equal(t, session.Running, inst.GetStatus(), "status must flip to Running")
	assert.Equal(t, stateHelp, h.state, "user on the new instance should see the attach-help modal")
	require.NotNil(t, h.textOverlay, "help modal overlay should be installed")
}

// TestInstanceStarted_Success_UserMovedToAnotherInstance is the core #310
// fix: if the user navigated to a different instance while one was starting,
// completion must not yank the selection or pop a modal onto them.
func TestInstanceStarted_Success_UserMovedToAnotherInstance(t *testing.T) {
	h := newTestHome(t)
	creating := newLoadingInstance(t, "still-creating")
	other := newLoadingInstance(t, "other")
	other.SetStatusForTest(session.Running)
	h.store.AddInstance(creating)
	h.store.AddInstance(other)
	// User navigated to `other` while `creating` was still starting.
	h.sidebar.SetSelectedInstance(1)

	_, _ = h.Update(instanceStartedMsg{instance: creating, err: nil})

	assert.Equal(t, session.Running, creating.GetStatus(), "status must flip to Running")
	assert.Equal(t, stateDefault, h.state, "user state must not flip to stateHelp")
	assert.Nil(t, h.textOverlay, "no help overlay should be shown")
	assert.Same(t, other, h.sidebar.GetSelectedInstance(),
		"user selection must be preserved on the instance they navigated to")
}

// TestInstanceStarted_Success_UserCreatingAnotherInstance covers the case
// where the user is already mid-naming a *second* instance (stateNew) when
// the first completes. The help modal would clobber their input — it must
// not show.
func TestInstanceStarted_Success_UserCreatingAnotherInstance(t *testing.T) {
	h := newTestHome(t)
	first := newLoadingInstance(t, "first")
	second := newLoadingInstance(t, "second")
	h.store.AddInstance(first)
	h.store.AddInstance(second)
	// Simulate the user having typed a name and entered stateNew for `second`.
	h.sidebar.SetSelectedInstance(1)
	h.namingInstance = second
	h.state = stateNew

	_, _ = h.Update(instanceStartedMsg{instance: first, err: nil})

	assert.Equal(t, session.Running, first.GetStatus(), "first instance should flip to Running")
	assert.Equal(t, stateNew, h.state, "naming state must be preserved")
	assert.Same(t, second, h.namingInstance, "namingInstance pointer must not be clobbered")
	assert.Nil(t, h.textOverlay, "no help overlay should be shown over the naming flow")
}

// TestInstanceStarted_Failure_RemovesByTitleNotBySelection is the second half
// of #310: on failure, the old code called sidebar.Kill() which operated on
// the currently-selected instance. If the user had navigated away, it would
// kill the *wrong* instance. The fix removes by title.
func TestInstanceStarted_Failure_RemovesByTitleNotBySelection(t *testing.T) {
	h := newTestHome(t)
	failing := newLoadingInstance(t, "failing")
	innocent := newLoadingInstance(t, "innocent")
	innocent.SetStatusForTest(session.Running)
	h.store.AddInstance(failing)
	h.store.AddInstance(innocent)
	// User moved to `innocent` while `failing` was still starting.
	h.sidebar.SetSelectedInstance(1)

	_, _ = h.Update(instanceStartedMsg{instance: failing, err: errors.New("boom")})

	titles := collectTitles(h.store.GetInstances())
	assert.NotContains(t, titles, "failing", "failed instance must be removed")
	assert.Contains(t, titles, "innocent", "unrelated instance must NOT be killed")
	assert.Same(t, innocent, h.sidebar.GetSelectedInstance(),
		"user selection must remain on the instance they navigated to")
}

// TestInstanceStarted_Failure_OnFailedInstance verifies the simpler case:
// user stayed on the creating instance, it failed, it gets removed.
func TestInstanceStarted_Failure_OnFailedInstance(t *testing.T) {
	h := newTestHome(t)
	failing := newLoadingInstance(t, "failing")
	h.store.AddInstance(failing)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.Update(instanceStartedMsg{instance: failing, err: errors.New("boom")})

	assert.Empty(t, h.store.GetInstances(), "failed instance must be removed")
	assert.Nil(t, h.sidebar.GetSelectedInstance(), "no instance should remain selected")
}

// TestInstanceStarted_Success_AutoYesApplied — sanity check that the autoYes
// assignment didn't get lost in the refactor.
func TestInstanceStarted_Success_AutoYesApplied(t *testing.T) {
	h := newTestHome(t)
	h.autoYes = true
	inst := newLoadingInstance(t, "auto-yes")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.Update(instanceStartedMsg{instance: inst, err: nil})

	assert.True(t, inst.AutoYes, "autoYes must propagate to the new instance when enabled")
}

func TestSaveContentPaneStateDoesNotOverwriteUnreadableRepoConfig(t *testing.T) {
	h := newTestHome(t)

	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	repoConfigPath := filepath.Join(configDir, "repos", h.repoID, config.RepoConfigFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(repoConfigPath), 0755))
	corruptConfig := []byte(`{"remote_hooks":{"launch_cmd":"/bin/launch"`)
	require.NoError(t, os.WriteFile(repoConfigPath, corruptConfig, 0644))

	hooks := h.hooksPane
	hooks.SetFocus(true)
	require.True(t, hooks.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}))
	require.True(t, hooks.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")}))
	require.True(t, hooks.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
	require.True(t, hooks.IsDirty())

	h.saveContentPaneState()

	raw, err := os.ReadFile(repoConfigPath)
	require.NoError(t, err)
	assert.Equal(t, string(corruptConfig), string(raw))
}

func TestInstanceStartedRegistersRepoAfterStart(t *testing.T) {
	h := newTestHome(t)
	repoRoot := filepath.Join(t.TempDir(), "repo-a")
	worktreePath := filepath.Join(t.TempDir(), "repo-a-session")
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoRoot, worktreePath, "new-session", "branch", "sha", false, true)
	require.NoError(t, err)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "new-session",
		Path:    repoRoot,
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStatusForTest(session.Loading)
	inst.SetStartedForTest(true)
	inst.SetGitWorktreeForTest(gw)

	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	require.Equal(t, 0, h.store.NumRepos())

	_, _ = h.Update(instanceStartedMsg{instance: inst, err: nil})

	assert.Equal(t, 1, h.store.NumRepos())
}

func collectTitles(instances []*session.Instance) []string {
	out := make([]string, 0, len(instances))
	for _, i := range instances {
		out = append(out, i.Title)
	}
	return out
}

// TestInstanceStarted_TimeoutError_SurfacesPaneSnippet covers the UX half of
// sachiniyer/agent-factory#502: a daemon-side timeout error that carries a
// "last pane content:" snippet should reach the user-facing ErrBox unchanged
// so the user can see what the agent was doing when it stalled.
func TestInstanceStarted_TimeoutError_SurfacesPaneSnippet(t *testing.T) {
	h := newTestHome(t)
	failing := newLoadingInstance(t, "stalled")
	h.store.AddInstance(failing)
	h.sidebar.SetSelectedInstance(0)
	h.errBox.SetSize(500, 1)

	daemonErr := errors.New("failed to start instance: timed out waiting for program to start (1m0s)\nlast pane content:\n  Loading config...\n  Connecting to MCP server...")
	_, _ = h.Update(instanceStartedMsg{instance: failing, err: daemonErr})

	rendered := h.errBox.String()
	assert.Contains(t, rendered, "timed out waiting for program to start")
	assert.Contains(t, rendered, "last pane content:")
	assert.Contains(t, rendered, "Connecting to MCP server...")
}

// TestInstanceStarted_TimeoutError_EmptyContentOmitsHeader covers the
// no-snippet branch: when the daemon couldn't capture any pane content the
// error string should remain the original bare timeout message — no empty
// "last pane content:" header.
func TestInstanceStarted_TimeoutError_EmptyContentOmitsHeader(t *testing.T) {
	h := newTestHome(t)
	failing := newLoadingInstance(t, "stalled-empty")
	h.store.AddInstance(failing)
	h.sidebar.SetSelectedInstance(0)
	h.errBox.SetSize(500, 1)

	daemonErr := errors.New("failed to start instance: timed out waiting for program to start (1m0s)")
	_, _ = h.Update(instanceStartedMsg{instance: failing, err: daemonErr})

	rendered := h.errBox.String()
	assert.Contains(t, rendered, "timed out waiting for program to start")
	assert.NotContains(t, rendered, "last pane content:")
}

// TestInstanceStarted_ReplacesSwappedSameTitleRow is the #808 regression.
//
// While a start RPC is in flight, the daemon persists the new session to
// instances.json before responding, so a background sync can swap the
// Loading placeholder for a disk-built copy of that same session. When the
// start then completes, ReplaceInstance(placeholder, started) misses (the
// placeholder pointer is gone) and ContainsInstance(started) is also
// pointer-based — re-adding unconditionally left two sidebar rows with one
// title, which SaveInstances persisted as byte-identical duplicate records.
// The handler must fall back to replacing the same-title row.
func TestInstanceStarted_ReplacesSwappedSameTitleRow(t *testing.T) {
	h := newTestHome(t)

	// The placeholder the user created; a background sync already swapped it
	// out of the sidebar for a disk-built copy of the same session.
	placeholder := newLoadingInstance(t, "scripts")
	diskCopy := newLoadingInstance(t, "scripts")
	diskCopy.SetStartedForTest(true)
	diskCopy.SetStatusForTest(session.Running)
	h.store.AddInstance(diskCopy)

	started := newLoadingInstance(t, "scripts")
	started.SetStartedForTest(true)

	_, _ = h.Update(instanceStartedMsg{instance: placeholder, started: started})

	instances := h.store.GetInstances()
	require.Len(t, instances, 1, "one logical session must occupy exactly one sidebar row (#808)")
	assert.Same(t, started, instances[0], "the started instance must replace the disk-built copy")
	assert.Equal(t, session.Running, started.GetStatus())
}
