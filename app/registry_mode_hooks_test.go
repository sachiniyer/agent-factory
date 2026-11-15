package app

import (
	"context"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hooksPaneAcceptsEditKey reports whether the hooks pane accepts an edit key
// (`n`) to start an add, the behavioral probe for "the pane is editable." A
// read-only pane consumes `n` without entering add mode, so IsEditing() stays
// false; an editable pane enters add mode. Focus is required for the pane's
// key loop to process the key at all, so the helper sets it first.
func hooksPaneAcceptsEditKey(hp *ui.HooksPane) bool {
	hp.SetFocus(true)
	hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	return hp.IsEditing()
}

// registryModeHooksHome returns a home in registry mode (#2477: launched outside
// a git repo, no active project) on a local target. repoRoot/repoID are emptied
// to mirror what newHome does when repo == nil, and isRemoteTarget is left at
// its default (false) so this is a *local* registry mode, not a remote target.
func registryModeHooksHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	h.repoRoot = ""
	h.repoID = ""
	return h
}

// TestShowHooksOverlayInRegistryModeIsReadOnly is the core regression guard for
// this bug. Before the fix, the hooks key dispatch in registry mode opened the
// editor fully editable despite there being no active project (m.repoRoot == "")
// and therefore no save target. Every edit dirtied the pane and every save failed
// at SaveInRepoPostWorktreeCommands("", ...) with "repo root is required to save
// in-repo config", trapping the user in a dirty-pane/failed-save cycle.
//
// The fix mirrors the remote-mode treatment (remoteHooksReadOnlyReason): the pane
// is made read-only at open time, so the edit keys are inert and no unsaveable
// edit can be made in the first place.
func TestShowHooksOverlayInRegistryModeIsReadOnly(t *testing.T) {
	h := registryModeHooksHome(t)
	hp := h.hooksPane
	require.False(t, isRemoteTarget(), "precondition: this is a local-target registry mode")

	_, _ = h.showHooksOverlay()

	require.Equal(t, stateHooks, h.state, "the overlay still opens (read-only, not refused)")
	require.True(t, hp.HasFocus(), "the editor still takes focus so esc closes it")
	require.False(t, hooksPaneAcceptsEditKey(hp),
		"a read-only pane must not accept edit keys — the edit can never be saved")

	// Every edit verb is inert: none may dirty the pane or open a field.
	hp.SetFocus(true)
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("n")},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune("D")},
		{Type: tea.KeyRunes, Runes: []rune("x")},
	} {
		hp.HandleKeyPress(k)
		assert.False(t, hp.IsDirty(), "edit key %q must not dirty a read-only pane", k.String())
		assert.False(t, hp.IsEditing(), "edit key %q must not open a field on a read-only pane", k.String())
	}
	assert.Empty(t, hp.GetCommands(), "no edit may land a command in registry mode")
}

// TestShowHooksOverlayInRegistryModeRenderShowsReasonAndNoEditInvitation pins
// the user-facing surface: the overlay must explain why editing is disabled
// (the read-only reason) and must NOT show the empty-state invitation
// "No hooks configured. Press Enter to focus, then n to add." that the editable
// pane renders. The invitation is exactly the prompt that lured registry-mode
// users into the doomed edit before the fix.
func TestShowHooksOverlayInRegistryModeRenderShowsReasonAndNoEditInvitation(t *testing.T) {
	h := registryModeHooksHome(t)
	hp := h.hooksPane

	_, _ = h.showHooksOverlay()
	// Size the pane explicitly so render's fitLine does not truncate the reason
	// (the overlay's own sizing depends on the terminal size, unsized in a unit
	// test); assert against a generous width that fits the whole reason.
	hp.SetSize(120, 40)

	rendered := hp.String()
	assert.Contains(t, rendered, "Hooks belong to a project",
		"the pane must render the read-only reason explaining there is no active project")
	assert.NotContains(t, rendered, "No hooks configured. Press Enter to focus, then n to add.",
		"the editable-pane invitation that lured the doomed edit must not render when read-only")
	// The pane shows the read-only footer, not the editable footer.
	assert.Contains(t, rendered, "esc back", "the read-only footer's escape hint must render")
	assert.NotContains(t, rendered, "n add", "the editable footer's add hint must not render when read-only")
}

// TestRegistryHooksReadOnlyReasonNamesProjectSwitchKey guards the key-naming
// convention the create refusal (#2764) established: the reason must name the
// configured project-switch key, not hardcode ctrl+p — a rebound key must stay
// actionable, and an unbound one must name the section instead of a dead key.
func TestRegistryHooksReadOnlyReasonNamesProjectSwitchKey(t *testing.T) {
	// Default binding: names ctrl+p.
	require.Contains(t, registryHooksReadOnlyReason(), "press ctrl+p",
		"the reason must name the configured project-switch key")

	// A rebound project-switch key is named, not the stale default. No app test
	// calls t.Parallel, so swapping the global binding for the duration of this
	// test (restored via t.Cleanup) is safe.
	orig := keys.GlobalKeyBindings[keys.KeySwitchProject]
	keys.GlobalKeyBindings[keys.KeySwitchProject] = key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "switch project"),
	)
	t.Cleanup(func() { keys.GlobalKeyBindings[keys.KeySwitchProject] = orig })
	require.Contains(t, registryHooksReadOnlyReason(), "press p",
		"a rebound project-switch key must be named; hardcoding ctrl+p would be a dead key")
}

// openRegistryHooksOverlay opens the hooks overlay on a registry-mode home, with
// the project picker's cross-repo snapshot answered from memory so a test that
// goes on to open the picker never dials a daemon.
func openRegistryHooksOverlay(t *testing.T) *home {
	t.Helper()
	h := registryModeHooksHome(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))
	resizeHome(h, 120, 45)
	_, _ = h.showHooksOverlay()
	require.Equal(t, stateHooks, h.state, "precondition: the hooks overlay owns the keyboard")
	return h
}

// TestRegistryHooksReasonSwitchKeyWorksFromTheOverlay pins #7324: every key the
// read-only reason names works from the screen the reason is shown on. The
// reason tells the user to press the project-switch key, and while stateHooks
// owns the keyboard the focused pane consumes every key but Esc and quit — so
// the advertised key did nothing. It now closes the read-only overlay and opens
// the project picker, through the real key dispatch.
func TestRegistryHooksReasonSwitchKeyWorksFromTheOverlay(t *testing.T) {
	h := openRegistryHooksOverlay(t)
	require.Contains(t, registryHooksReadOnlyReason(), "press ctrl+p",
		"precondition: this is the key the reason on screen names")

	_, _ = h.dispatchKeyAction(tea.KeyMsg{Type: tea.KeyCtrlP})

	require.Equal(t, stateSwitchProject, h.state,
		"the key the reason names must open the project picker from the hooks overlay")
	require.NotNil(t, h.projectPickerOverlay, "the project picker is open")
	assert.False(t, h.hooksPane.HasFocus(), "the hooks overlay released the keyboard")
	assert.False(t, h.hooksPane.IsDirty(), "a read-only pane closes with nothing to save")
}

// A rebound project-switch key is the one the reason names, so it is the one
// that must pass through — not a hardcoded ctrl+p.
func TestRegistryHooksReasonReboundSwitchKeyWorksFromTheOverlay(t *testing.T) {
	orig := keys.GlobalKeyBindings[keys.KeySwitchProject]
	keys.GlobalKeyBindings[keys.KeySwitchProject] = key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "switch project"),
	)
	t.Cleanup(func() { keys.GlobalKeyBindings[keys.KeySwitchProject] = orig })
	h := openRegistryHooksOverlay(t)
	require.Contains(t, registryHooksReadOnlyReason(), "press p")

	// The stale default is not the advertised key any more: still swallowed.
	_, _ = h.dispatchKeyAction(tea.KeyMsg{Type: tea.KeyCtrlP})
	require.Equal(t, stateHooks, h.state, "a key the reason does not name stays with the overlay")

	_, _ = h.dispatchKeyAction(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	require.Equal(t, stateSwitchProject, h.state,
		"the rebound key the reason names must open the project picker from the hooks overlay")
}

// The pass-through belongs to the registry-mode reason only. With an active
// project the overlay is an editor and shows no such reason, so the switch key
// stays with the pane exactly as before.
func TestHooksOverlayWithAProjectKeepsTheSwitchKey(t *testing.T) {
	h := activeProjectHome(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))
	resizeHome(h, 120, 45)
	require.NotEmpty(t, h.repoRoot, "precondition: an active project")
	_, _ = h.showHooksOverlay()
	require.Equal(t, stateHooks, h.state)

	_, _ = h.dispatchKeyAction(tea.KeyMsg{Type: tea.KeyCtrlP})

	require.Equal(t, stateHooks, h.state, "an editable hooks overlay keeps the keyboard")
	assert.Nil(t, h.projectPickerOverlay, "no picker opens over an editable hooks overlay")
}

// TestShowHooksOverlayRemoteRegistryModeKeepsRemoteReason pins the
// interaction with remote mode: a remote target is read-only with its OWN
// reason (the hooks live on the daemon host) set at startup, and registry mode
// (also repoRoot == "") must NOT override it with the local registry reason.
// The open gate is local-target-only (m.repoRoot == "" && !isRemoteTarget()).
func TestShowHooksOverlayRemoteRegistryModeKeepsRemoteReason(t *testing.T) {
	h := registryModeHooksHome(t)
	stubRemote(t) // remote target AND no active project
	require.True(t, isRemoteTarget(), "precondition: remote target")
	// newHome sets the remote reason at startup; newTestHome bypasses newHome,
	// so set it the same way to model the production startup state.
	h.hooksPane.SetReadOnly(remoteHooksReadOnlyReason())
	hp := h.hooksPane

	_, _ = h.showHooksOverlay()
	// Size the pane explicitly so render's fitLine does not truncate the reason
	// (see TestShowHooksOverlayInRegistryModeRenderShowsReasonAndNoEditInvitation).
	hp.SetSize(120, 40)

	rendered := hp.String()
	require.Contains(t, rendered, "Hooks live in the project's checkout on the daemon host",
		"the remote reason set at startup must stay when repoRoot is also empty")
	assert.NotContains(t, rendered, "Hooks belong to a project",
		"the local registry reason must not override the remote reason on a remote target")
}

// TestSaveContentPaneState_RegistryModeStrayDirtyHookEditDiscardsNotErrors is
// the safety-net guard, mirroring the remote-mode safety net
// (TestRemoteHooksAreReadOnlyAndAStraySaveDiscardsInsteadOfWedging). The
// read-only gate at showHooksOverlay prevents edits from being made; this
// covers a pane that got dirty anyway (a stale state, an unguarded path):
// the save must discard the unsaveable edit with a notice and return no error
// instead of erroring "repo root is required" on every attempt and trapping
// quit (#5860).
func TestSaveContentPaneState_RegistryModeStrayDirtyHookEditDiscardsNotErrors(t *testing.T) {
	// Build a dirty pane in LOCAL mode (editing works there), then drop into
	// registry mode to isolate the safety net from the open-time read-only gate.
	h := dirtyHooksHome(t)
	require.False(t, isRemoteTarget(), "precondition: local target")
	require.True(t, h.hooksPane.IsDirty(), "precondition: a stray dirty edit")
	require.Contains(t, h.hooksPane.GetCommands(), "echo test")

	h.repoRoot = "" // drop into registry mode (local target, no active project)
	h.errBox.SetSize(500, 1)

	// The safety net discards the unsaveable edit instead of erroring forever.
	require.NoError(t, h.saveContentPaneState(),
		"a stray registry-mode edit must not trap the user with a perpetual save error")
	require.False(t, h.hooksPane.IsDirty(), "the unsaveable edit is dropped, not kept dirty forever")
	require.NotContains(t, h.hooksPane.GetCommands(), "echo test",
		"the discarded edit must not remain in the pane")
	require.Contains(t, h.takePendingPaneNotice(), "unsaved hook edit was discarded",
		"the discard must say so once via the pending pane notice")
}

// TestHandleQuit_RegistryModeStrayDirtyHookEditDoesNotTrapQuit is the quit-path
// half of the safety net: a stray dirty hook edit in registry mode must not
// reproduce the two-stage quit-discard flow the save-failure path triggers. The
// safety net discards cleanly (no error), so the first quit surfaces the discard
// notice rather than arming a "press quit again to discard" confirmation, and a
// second quit goes through.
func TestHandleQuit_RegistryModeStrayDirtyHookEditDoesNotTrapQuit(t *testing.T) {
	h := dirtyHooksHome(t)
	h.repoRoot = "" // registry mode (local target, no active project)
	h.errBox.SetSize(500, 1)
	require.True(t, h.hooksPane.IsDirty(), "precondition: a stray dirty edit")

	// First quit: the safety net discards the edit and posts a notice; no save
	// error means no "press quit again to discard" confirmation is armed.
	// handleQuit drains the notice itself (takePendingPaneNotice) to surface it
	// as a transient message, so the observable is the returned cmd plus a clean
	// pane and an unarmed confirmation — not the (already-drained) notice field.
	_, cmd := h.handleQuit()
	require.Empty(t, h.quitSaveFailure,
		"a cleanly-discarded edit must not arm the quit-discard confirmation")
	require.False(t, h.hooksPane.IsDirty(), "the stray edit is discarded on the first quit")
	require.False(t, h.quitting,
		"the first quit surfaces the discard notice as a transient message, not a clean exit")
	require.NotNil(t, cmd, "the discard notice is surfaced as a transient message")

	// Second quit: nothing is dirty, so it goes through cleanly.
	_, _ = h.handleQuit()
	require.True(t, h.quitting, "the second quit must proceed — no edit is held")
}

// TestSwitchProjectFromRegistryModeMakesHooksEditableAgain pins the
// reset-the-reason half of the fix: after picking a project from registry mode,
// the hooks pane must become editable again. switchProject's local branches
// clear the read-only reason (SetReadOnly("")); without that, a pane left
// read-only by a registry-mode hooks open would stay read-only forever, even
// with an active project selected.
func TestSwitchProjectFromRegistryModeMakesHooksEditableAgain(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	// Start in registry mode with the pane read-only, as showHooksOverlay would
	// leave it after an open/close in registry mode.
	h.repoRoot = ""
	h.repoID = ""
	h.hooksPane.SetReadOnly(registryHooksReadOnlyReason())
	require.False(t, hooksPaneAcceptsEditKey(h.hooksPane),
		"precondition: the pane is read-only in registry mode")

	// Switch to a real local project whose config resolves.
	projectRoot := initTestGitRepo(t)
	repo := &config.RepoContext{Root: projectRoot, ID: config.RepoIDFromRoot(projectRoot)}
	_, _ = h.switchProject(repo)

	require.Equal(t, projectRoot, h.repoRoot, "switched to the new project")
	require.True(t, hooksPaneAcceptsEditKey(h.hooksPane),
		"the pane must be editable again after switching to a local project — the registry read-only reason must be cleared")
}

// TestSwitchProjectFromRegistryModeMakesHooksEditableWhenConfigResolveFails is
// the failure-branch guard: even when the incoming project's config cannot be
// resolved, the pane must still become editable (the project's checkout is on
// this machine, and an add would save to the new m.repoRoot). The failure
// branch clears the hooks (SetCommands(nil)) and must also clear the read-only
// reason.
func TestSwitchProjectFromRegistryModeMakesHooksEditableWhenConfigResolveFails(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	// Start in registry mode with the pane read-only.
	h.repoRoot = ""
	h.repoID = ""
	h.hooksPane.SetReadOnly(registryHooksReadOnlyReason())

	// Incoming project is a real repo with a corrupt in-repo config.
	projectRoot := initTestGitRepo(t)
	writeInRepoConfig(t, projectRoot, "invalid toml {{{\n[broken")
	repo := &config.RepoContext{Root: projectRoot, ID: config.RepoIDFromRoot(projectRoot)}
	_, err := config.ResolveConfig(projectRoot)
	require.Error(t, err, "precondition: the incoming project's config must fail to resolve")

	_, _ = h.switchProject(repo)

	require.Equal(t, projectRoot, h.repoRoot, "switched to the new project")
	require.Empty(t, h.hooksPane.GetCommands(), "hooks are cleared on a failed resolve (#1686)")
	require.True(t, hooksPaneAcceptsEditKey(h.hooksPane),
		"the pane must still be editable after a failed resolve — the registry read-only reason must be cleared")
}
