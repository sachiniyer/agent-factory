package app

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/ui/layout"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartNewInstanceInRegistryModeRefusesWithoutProject is the #2764
// regression guard.
//
// Registry mode (#2477) launches the TUI outside a git repository, so there is
// no active project until the user picks one — m.repoRoot is empty. Session
// creation substituted the process cwd for it and opened the naming form
// anyway, so the user typed a name, pressed enter, and only THEN learned it
// could not work: the daemon resolved the non-git cwd and answered `failed to
// get git repo root for <path>: exit status 128`.
//
// Both creation keys are covered. `N` already refused early in registry mode,
// but for the wrong reason and with the wrong words — it reported the cwd as a
// repo with no remote_hooks configured, which tells a user with no project
// selected to go configure something.
func TestStartNewInstanceInRegistryModeRefusesWithoutProject(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote bool
	}{
		{name: "local", remote: false},
		{name: "remote", remote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			// Registry mode: launched outside a repo, no project selected yet.
			h.repoRoot = ""
			h.errBox.SetSize(200, 1)

			model, cmd := h.startNewInstance(tc.remote)

			require.Same(t, h, model)
			require.NotNil(t, cmd, "an advertised key must produce a visible outcome, never a swallowed keypress")
			// The dead end is the bug: no form to fill in, so nothing to submit.
			assert.Equal(t, stateDefault, h.state, "the naming form must not open when no project can receive the session")
			assert.Nil(t, h.namingInstance)
			assert.Equal(t, 0, h.store.NumInstances(), "no placeholder row may be left behind")

			full := h.errBox.FullError()
			assert.Contains(t, full, "select a project",
				"the message must name the action that unblocks the user, not a git error from the daemon")
			assert.NotContains(t, full, "exit status 128",
				"the cryptic daemon-side git failure is exactly what this guard exists to prevent")
			// The notice clips to the terminal width and the TAIL is what vanishes
			// (#1973), so the key that unblocks the user has to arrive before the
			// explanation — at 120 columns a trailing "press ctrl+p" is cut, which
			// was the shape the first real-TUI drive of this guard produced.
			action := strings.Index(full, "ctrl+p")
			why := strings.Index(full, "no active project")
			require.NotEqual(t, -1, action, "the message must name the key that picks a project")
			require.NotEqual(t, -1, why)
			assert.Less(t, action, why, "the action must survive width-clipping; the explanation is the part that may be cut")
		})
	}
}

// TestStartNewInstanceWithActiveProjectStillOpensNaming is the non-regression
// half: with a project active — which is every non-registry-mode run, and
// registry mode itself once a project is selected — both keys still open the
// naming form exactly as before.
func TestStartNewInstanceWithActiveProjectStillOpensNaming(t *testing.T) {
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	h.repoRoot = repoDir

	model, cmd := h.startNewInstance(false)

	require.Same(t, h, model)
	require.Nil(t, cmd)
	assert.Equal(t, stateNew, h.state)
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, repoDir, h.namingInstance.Path,
		"the placeholder must target the ACTIVE project's repo root")
}

// TestRegistryModeEmptyWorkspaceDoesNotAdvertiseCreate is #2830, the other half
// of the same dead end. #2764 fixed the path where `n` REACHES creation and now
// refuses with a reason; this covers the path where it never arrives at all.
//
// In registry mode newHome lands focus on the Projects section, which is a
// captive vim-style list that consumes the create verbs on purpose (#1620). So
// `n` produced no form, no notice, and no repaint — while the workspace beside
// it read "No sessions yet — press n to create one." The advertised affordance
// and the live key routing disagreed, and the copy was the half that was wrong.
//
// Asserted on the composed frame, not the renderer: the renderer's own behavior
// is covered in ui, and what regressed here is which renderer the view picks.
func TestRegistryModeEmptyWorkspaceDoesNotAdvertiseCreate(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "" // registry mode: launched outside a repo (#2477)
	resizeHome(h, 120, 30)
	require.Equal(t, 0, h.store.NumInstances(), "precondition: the rail is empty")

	view := flatten(h.View())

	assert.NotContains(t, view, "press n to create one",
		"no focused region in registry mode can honor that promise (#2830)")
	assert.Contains(t, view, "No project selected",
		"the empty state must name the actual blocker")
	assert.Contains(t, view, "press enter to pick one",
		"registry mode focuses the captive Projects section, where Enter on the cursor row "+
			"IS the pick — and where ctrl+p is suppressed by name (#1620)")
	assert.NotContains(t, view, "ctrl+p",
		"advertising the project-switch key from the one section that swallows it would "+
			"reproduce #2830 with a different dead key")
}

// The same copy from a region where ctrl+p actually works must name ctrl+p.
// Which key is live depends on focus, so a single hardcoded hint is wrong on one
// of the two screens whichever key it picks.
func TestRegistryModeEmptyWorkspaceNamesTheKeyThatWorksWhereFocusIs(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = ""
	resizeHome(h, 120, 30)
	h.focusRegion(layout.RegionTree)
	require.False(t, h.projectsFocused(), "precondition: the captive section does not hold the keyboard")

	view := flatten(h.View())

	assert.Contains(t, view, "press ctrl+p to pick one",
		"from the tree, ctrl+p reaches the project picker and Enter does not")
	assert.NotContains(t, view, "press enter to pick one")
}

// The create refusal is reached by pressing `n`, which only arrives from a
// region that is NOT the captive Projects section — so it names ctrl+p. Pinned
// because the two surfaces share a helper and must still be allowed to differ.
func TestNoActiveProjectNoticeNamesTheKeyForItsFocus(t *testing.T) {
	assert.Contains(t, noActiveProjectNotice(false), "press ctrl+p to pick one")
	assert.Contains(t, noActiveProjectNotice(true), "press enter to pick one")
}

// Inside a repo the ordinary onboarding copy is untouched: `n` works there, so
// advertising it is correct and this fix must not reach that state.
func TestNonRegistryModeEmptyWorkspaceStillAdvertisesCreate(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = t.TempDir()
	resizeHome(h, 120, 30)
	require.Equal(t, 0, h.store.NumInstances())

	view := flatten(h.View())

	assert.Contains(t, view, "No sessions yet — press n to create one.")
	assert.NotContains(t, view, "No project selected")
}
