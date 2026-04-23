package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
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

	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sidebar := ui.NewSidebar(&spin, false)
	tw := ui.NewTabbedWindow(ui.NewPreviewPane(), ui.NewTerminalPane())
	cp := ui.NewContentPane(tw)

	state := config.DefaultState()
	repoID := "test-repo-" + filepath.Base(tmp)
	storage, err := session.NewStorage(state, repoID)
	require.NoError(t, err)

	return &home{
		ctx:         context.Background(),
		state:       stateDefault,
		appConfig:   config.DefaultConfig(),
		appState:    state,
		storage:     storage,
		sidebar:     sidebar,
		contentPane: cp,
		menu:        ui.NewMenu(),
		errBox:      ui.NewErrBox(),
		spinner:     spin,
		repoID:      repoID,
	}
}

// newLoadingInstance returns an instance in Loading status — mirroring the
// state that preSaveInstances produces before Start() runs. started=false so
// ToInstanceData / SaveInstances include it via the "|| Status == Loading"
// branch.
func newLoadingInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStatus(session.Loading)
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
	h.sidebar.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.Update(instanceStartedMsg{instance: inst, err: nil})

	assert.Equal(t, session.Running, inst.Status, "status must flip to Running")
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
	other.SetStatus(session.Running)
	h.sidebar.AddInstance(creating)
	h.sidebar.AddInstance(other)
	// User navigated to `other` while `creating` was still starting.
	h.sidebar.SetSelectedInstance(1)

	_, _ = h.Update(instanceStartedMsg{instance: creating, err: nil})

	assert.Equal(t, session.Running, creating.Status, "status must flip to Running")
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
	h.sidebar.AddInstance(first)
	h.sidebar.AddInstance(second)
	// Simulate the user having typed a name and entered stateNew for `second`.
	h.sidebar.SetSelectedInstance(1)
	h.namingInstance = second
	h.state = stateNew

	_, _ = h.Update(instanceStartedMsg{instance: first, err: nil})

	assert.Equal(t, session.Running, first.Status, "first instance should flip to Running")
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
	innocent.SetStatus(session.Running)
	h.sidebar.AddInstance(failing)
	h.sidebar.AddInstance(innocent)
	// User moved to `innocent` while `failing` was still starting.
	h.sidebar.SetSelectedInstance(1)

	_, _ = h.Update(instanceStartedMsg{instance: failing, err: errors.New("boom")})

	titles := collectTitles(h.sidebar.GetInstances())
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
	h.sidebar.AddInstance(failing)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.Update(instanceStartedMsg{instance: failing, err: errors.New("boom")})

	assert.Empty(t, h.sidebar.GetInstances(), "failed instance must be removed")
	assert.Nil(t, h.sidebar.GetSelectedInstance(), "no instance should remain selected")
}

// TestInstanceStarted_Success_AutoYesApplied — sanity check that the autoYes
// assignment didn't get lost in the refactor.
func TestInstanceStarted_Success_AutoYesApplied(t *testing.T) {
	h := newTestHome(t)
	h.autoYes = true
	inst := newLoadingInstance(t, "auto-yes")
	h.sidebar.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	_, _ = h.Update(instanceStartedMsg{instance: inst, err: nil})

	assert.True(t, inst.AutoYes, "autoYes must propagate to the new instance when enabled")
}

func collectTitles(instances []*session.Instance) []string {
	out := make([]string, 0, len(instances))
	for _, i := range instances {
		out = append(out, i.Title)
	}
	return out
}
