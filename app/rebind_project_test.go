package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// rebindRows is one registry-backed row whose checkout is gone — the row `b`
// rebinds.
func rebindRows() []overlay.Project {
	return []overlay.Project{{Name: "old-project", Root: "/old/project", RegistryID: "prj_A", MissingPath: true}}
}

// openRebindPicker opens a fresh picker the way showProjectPickerOverlay leaves
// it: overlay set and state stateSwitchProject.
func openRebindPicker(h *home) *overlay.ProjectPickerOverlay {
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay(rebindRows(), "")
	h.projectPickerOverlay.SetMaxSize(80, 24)
	h.state = stateSwitchProject
	return h.projectPickerOverlay
}

// submitPickerRebind drives b, a typed path and Enter through the app's picker
// handler and returns the off-loop rebind command it dispatched.
func submitPickerRebind(t *testing.T, h *home, path string) tea.Cmd {
	t.Helper()
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(path)})
	_, cmd := h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "Enter in rebind mode must dispatch the daemon rebind")
	return cmd
}

// stubRebind replaces the daemon seam with one answering err (nil = success,
// echoing the path as the new root).
func stubRebind(t *testing.T, err error) {
	t.Helper()
	old := rebindProjectThroughDaemon
	rebindProjectThroughDaemon = func(projectID, path string) (config.Project, error) {
		if err != nil {
			return config.Project{}, err
		}
		return config.Project{ID: projectID, Root: path}, nil
	}
	t.Cleanup(func() { rebindProjectThroughDaemon = old })
}

// TestStaleRebindSuccessDoesNotCloseAFreshPicker pins the Breken finding on
// #4789: open picker A, submit a rebind, close A, open picker B, then deliver
// A's reply. B is a different picker waiting on nothing — the reply must not
// close it or raise a success toast that reads as B's result.
func TestStaleRebindSuccessDoesNotCloseAFreshPicker(t *testing.T) {
	h := newTestHome(t)
	stubRebind(t, nil)

	openRebindPicker(h)
	cmdA := submitPickerRebind(t, h, "/new/project")
	h.closeProjectPicker()
	pickerB := openRebindPicker(h)
	noticeBefore := h.transientNoticeID

	h.Update(cmdA())

	assert.Equal(t, stateSwitchProject, h.state, "a stale rebind reply must not close a freshly opened picker")
	assert.Same(t, pickerB, h.projectPickerOverlay, "picker B must stay on screen")
	assert.Equal(t, noticeBefore, h.transientNoticeID, "a stale rebind reply must not raise a success toast over picker B")
}

// TestStaleRebindErrorDoesNotLandInAnotherPicker covers the rejection half of
// the same class: picker B has its OWN rebind in flight when A's rejection
// arrives. A's error must not paint into B, nor re-arm B's inert form while B's
// request is still pending.
func TestStaleRebindErrorDoesNotLandInAnotherPicker(t *testing.T) {
	h := newTestHome(t)
	stubRebind(t, errors.New("path is already bound to another project"))

	openRebindPicker(h)
	cmdA := submitPickerRebind(t, h, "/taken")
	h.closeProjectPicker()
	pickerB := openRebindPicker(h)
	submitPickerRebind(t, h, "/other")

	h.Update(cmdA())

	require.Same(t, pickerB, h.projectPickerOverlay)
	assert.Equal(t, stateSwitchProject, h.state)
	assert.True(t, pickerB.RebindPending(), "A's rejection must not settle B's in-flight request")
	assert.NotContains(t, pickerB.Render(), "already bound", "A's rejection must not render inside picker B")
}

// TestOwnedRebindReplyStillReachesItsPicker keeps the happy path the ownership
// check must not break: the picker that submitted still gets its own answer —
// a rejection inline, and a success closes it with a toast.
func TestOwnedRebindReplyStillReachesItsPicker(t *testing.T) {
	t.Run("rejection", func(t *testing.T) {
		h := newTestHome(t)
		stubRebind(t, errors.New("path is already bound to another project"))
		picker := openRebindPicker(h)
		h.Update(submitPickerRebind(t, h, "/taken")())

		require.Same(t, picker, h.projectPickerOverlay)
		assert.False(t, picker.RebindPending(), "an owned rejection re-arms the form")
		assert.Contains(t, picker.Render(), "already bound")
	})
	t.Run("success", func(t *testing.T) {
		h := newTestHome(t)
		stubRebind(t, nil)
		openRebindPicker(h)
		noticeBefore := h.transientNoticeID
		h.Update(submitPickerRebind(t, h, "/new/project")())

		assert.Nil(t, h.projectPickerOverlay, "an owned success closes the picker")
		assert.Equal(t, stateDefault, h.state)
		assert.Greater(t, h.transientNoticeID, noticeBefore, "an owned success announces itself")
	})
}

// TestCtrlCQuitsWhileRebindPending pins the Codex hard-exit finding on #4789:
// the pending form consumes every key, and a stalled daemon call would leave
// the TUI impossible to quit. Ctrl+C must still reach the app's hard exit.
func TestCtrlCQuitsWhileRebindPending(t *testing.T) {
	h := newTestHome(t)
	stubRebind(t, nil)
	openRebindPicker(h)
	submitPickerRebind(t, h, "/new/project") // never delivered: the daemon stalls
	require.True(t, h.projectPickerOverlay.RebindPending())

	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyCtrlC})

	assert.True(t, h.quitting, "ctrl+c must quit while a rebind is pending")
}
