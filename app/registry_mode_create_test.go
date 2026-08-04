package app

import (
	"testing"

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
