package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// deadBackend is a FakeBackend whose IsAlive reports false, simulating a tmux
// (or remote) session that has vanished out from under the TUI — the #935
// scenario. HasUpdated inherits FakeBackend's (false,false), the same value a
// healthy idle session returns, so it exercises the exact ambiguity the fix
// has to resolve via the liveness probe.
type deadBackend struct {
	*session.FakeBackend
}

func (b *deadBackend) IsAlive(*session.Instance) bool { return false }

// newDeadInstance returns a started instance backed by deadBackend with the
// given starting status. It does not spin up tmux/git, so it is hermetic.
func newDeadInstance(t *testing.T, title string, status session.Status) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(&deadBackend{FakeBackend: session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	inst.SetStatus(status)
	return inst
}

// TestHandleEnter_DeadSessionShowsError is the primary #935 guard: pressing
// Enter on a session whose backing tmux session is gone must surface an
// actionable error rather than silently swallowing the keypress (which left the
// user unsure whether Enter registered while the sidebar still showed a green
// Ready dot).
func TestHandleEnter_DeadSessionShowsError(t *testing.T) {
	h := newTestHome(t)

	// Ready is the deceptive starting status the bug report describes — the
	// sidebar still paints it green even though the session is gone.
	inst := newDeadInstance(t, "dead-session", session.Ready)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	model, cmd := h.handleEnter()
	h = model.(*home)

	// The attach must NOT proceed: no help/attach overlay is installed and the
	// state stays default. The Deleting path in handleEnter behaves the same
	// way — error, no attach.
	require.Equal(t, stateDefault, h.state, "a dead session must not open the attach help overlay")
	require.Nil(t, h.textOverlay, "no help overlay should be installed for a dead session")

	// handleError returns the hide-error timer command and records the message.
	require.NotNil(t, cmd, "handleEnter must return the error-hide command, not a silent nil")
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "no longer running",
		"the error must explain why Enter did nothing")
	require.Contains(t, h.errBox.String(), "dead-session",
		"the error must name the offending session")
}

// TestHandleEnter_LostSessionShowsError pins the #1108 × #1089-PR-2
// interaction: Enter on a Lost session must take the explicit lost-error path
// — never enter interactive mode on a dead binding, and never the generic
// dead-tmux message (Lost is checked before TmuxAlive in interactiveGuard so
// the specific "expected back" wording wins).
func TestHandleEnter_LostSessionShowsError(t *testing.T) {
	h := newTestHome(t)

	inst := newDeadInstance(t, "lost-session", session.Lost)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	model, cmd := h.handleEnter()
	h = model.(*home)

	require.Equal(t, stateDefault, h.state, "a lost session must not open any overlay")
	require.Nil(t, h.textOverlay, "no help overlay should be installed for a lost session")
	require.False(t, h.interactive, "Enter on a lost session must not enter interactive mode")

	require.NotNil(t, cmd, "handleEnter must return the error-hide command, not a silent nil")
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "was lost",
		"the error must say the session was lost, not the generic dead message")
	require.Contains(t, h.errBox.String(), "lost-session",
		"the error must name the offending session")
}

// TestHandleEnter_ArchivedSessionShowsError (#1028): Enter on an archived
// session must take the explicit archived-error path — not interactive mode,
// not the generic dead-tmux message — and must point the user at `restore` as
// the off-ramp. Archived is checked before TmuxAlive in interactiveGuard so the
// specific wording wins.
func TestHandleEnter_ArchivedSessionShowsError(t *testing.T) {
	h := newTestHome(t)

	inst := newDeadInstance(t, "archived-session", session.Archived)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	model, cmd := h.handleEnter()
	h = model.(*home)

	require.Equal(t, stateDefault, h.state, "an archived session must not open any overlay")
	require.Nil(t, h.textOverlay, "no help overlay should be installed for an archived session")
	require.False(t, h.interactive, "Enter on an archived session must not enter interactive mode")

	require.NotNil(t, cmd, "handleEnter must return the error-hide command, not a silent nil")
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "is archived",
		"the error must say the session is archived, not the generic dead message")
	require.Contains(t, h.errBox.String(), "restore",
		"the error must point at restore as the off-ramp")
	require.Contains(t, h.errBox.String(), "archived-session",
		"the error must name the offending session")
}
