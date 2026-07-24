package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
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

func (b *deadBackend) IsAlive(*session.Instance) (bool, error) { return false, nil }

// newDeadInstance returns a started instance backed by deadBackend with the
// given starting status. It does not spin up tmux/git, so it is hermetic.
func newDeadInstance(t *testing.T, title string, status session.Status) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(&deadBackend{FakeBackend: session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(status)
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

// TestHandleEnter_RestingRowRestores pins #2489: Enter on a resting
// (Lost/Dead/archived) row restores it in place — the intuitive off-ramp — rather
// than surfacing interactiveGuard's "press <key> to restore" message. It raises
// the optimistic OpRestoring and dispatches the restore RPC, exactly as the `r`
// verb does, with no confirmation and no interactive-mode entry on a dead binding.
//
// Distinct from the #935 case above: there the liveness is still live (Ready) and
// only tmux vanished, so LifecycleAction()==Archive and Enter correctly errors;
// here the daemon has settled the row into a resting liveness, which IS restorable.
func TestHandleEnter_RestingRowRestores(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status session.Status
	}{
		{"lost", session.Lost},
		{"dead", session.Dead},
		{"archived", session.Archived},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := newDeadInstance(t, "resting-session", tc.status)
			h.store.AddInstance(inst)
			h.sidebar.SetSelectedInstance(0)

			var gotRequest daemon.RestoreSessionRequest
			prev := restoreSessionThroughDaemon
			restoreSessionThroughDaemon = func(request daemon.RestoreSessionRequest) (string, error) {
				gotRequest = request
				return "/worktree/path", nil
			}
			t.Cleanup(func() { restoreSessionThroughDaemon = prev })

			model, cmd := h.handleEnter()
			h = model.(*home)

			require.Equal(t, stateDefault, h.state, "restoring a resting row must not open any overlay")
			require.Nil(t, h.textOverlay, "no help overlay should be installed")
			require.False(t, h.interactive, "Enter on a resting row must restore, not enter interactive mode")
			require.Equal(t, session.OpRestoring, inst.GetInFlightOp(),
				"Enter restores the row (#2489), raising the optimistic restore op")

			// No "press r" guard message: the gesture ACTS on the restore.
			h.errBox.SetSize(200, 1)
			require.NotContains(t, h.errBox.String(), "press",
				"Enter must act on the restore, not name the restore key")

			require.NotNil(t, cmd, "the restore must dispatch its daemon command")
			done, ok := cmd().(instanceRestoredMsg)
			require.True(t, ok, "the command must emit instanceRestoredMsg")
			require.NoError(t, done.err)
			require.Equal(t, daemon.RestoreSessionRequest{ID: inst.ID, Title: "resting-session", RepoID: h.repoID}, gotRequest,
				"the restore RPC carries the row's stable identity")
		})
	}
}

// TestHandleAttach_RestingRowRestores is the #2489 attach twin: `o` on a resting
// row restores it too, not just Enter — the two verbs share restoreIfResting.
func TestHandleAttach_RestingRowRestores(t *testing.T) {
	h := newTestHome(t)
	inst := newDeadInstance(t, "resting-session", session.Archived)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	called := false
	prev := restoreSessionThroughDaemon
	restoreSessionThroughDaemon = func(daemon.RestoreSessionRequest) (string, error) { called = true; return "/p", nil }
	t.Cleanup(func() { restoreSessionThroughDaemon = prev })

	model, cmd := h.handleAttach()
	h = model.(*home)

	require.Equal(t, session.OpRestoring, inst.GetInFlightOp(), "`o` on a resting row restores it (#2489)")
	require.False(t, h.attached.Load(), "`o` on a resting row must restore, not start an attach")
	require.NotNil(t, cmd, "the restore must dispatch its daemon command")
	_, ok := cmd().(instanceRestoredMsg)
	require.True(t, ok, "the command must emit instanceRestoredMsg")
	require.True(t, called, "the restore RPC must fire")
}
