package app

import (
	"strings"
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

// reprovisioningBackend is a FakeBackend that reports a sandbox runtime type, so
// Instance.RestoreWouldDiscardUnpushedWork() sees a Lost/Dead restore that would
// re-provision a fresh sandbox — the case #2489 fences behind a confirmation.
type reprovisioningBackend struct {
	*session.FakeBackend
}

func (b *reprovisioningBackend) Type() string { return "docker" }

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

			// No guard message: the gesture ACTS on the restore. This fails if the
			// fix regresses to interactiveGuard, whose fence now names the restore key
			// ("press <key> to restore", #2479).
			h.errBox.SetSize(200, 1)
			require.NotContains(t, h.errBox.String(), "to restore",
				"Enter must act on the restore, not surface the guard error")

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

// TestHandleEnter_RemoteLostRowConfirmsBeforeReprovision is the #2489 review's P2
// data-loss guard: a Lost/Dead REMOTE session's restore re-provisions a fresh
// sandbox when the old one can't be reached, discarding any unpushed work (#1794).
// So Enter must CONFIRM before restoring it — never re-provision silently from a
// keystroke (or a mouse double-click, which also routes through handleEnter).
func TestHandleEnter_RemoteLostRowConfirmsBeforeReprovision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status session.Status
	}{
		{"lost", session.Lost},
		{"dead", session.Dead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := newDeadInstance(t, "remote-resting", tc.status)
			inst.SetBackend(&reprovisioningBackend{FakeBackend: session.NewFakeBackend()})
			h.store.AddInstance(inst)
			h.sidebar.SetSelectedInstance(0)

			called := false
			prev := restoreSessionThroughDaemon
			restoreSessionThroughDaemon = func(daemon.RestoreSessionRequest) (string, error) { called = true; return "/p", nil }
			t.Cleanup(func() { restoreSessionThroughDaemon = prev })

			model, _ := h.handleEnter()
			h = model.(*home)

			require.Equal(t, stateConfirm, h.state, "a remote Lost/Dead restore must confirm before re-provisioning")
			require.NotNil(t, h.confirmationOverlay)
			require.Equal(t, session.OpNone, inst.GetInFlightOp(), "no restore starts before the user confirms")
			require.False(t, called, "the restore RPC must not fire before confirmation")

			rendered := strings.Join(strings.Fields(h.confirmationOverlay.Render()), " ")
			require.Contains(t, rendered, "never pushed", "the confirmation names the unpushed-work risk")
			require.Contains(t, rendered, "can't be reached", "the confirmation names the unknown-connectivity case")
			require.Contains(t, rendered, "refuses", "the confirmation says unknown connectivity blocks replacement")
			require.NotContains(t, rendered, "can't be reached, restore provisions",
				"the confirmation must not promise replacement from mere unreachability")

			// Confirming raises the optimistic op and dispatches the restore off the
			// event loop via startRestoreMsg (mirrors handleStateConfirm forwarding).
			h.confirmationOverlay.OnConfirm()
			require.Equal(t, session.OpRestoring, inst.GetInFlightOp(), "confirm raises the optimistic restore op")
			start, ok := h.pendingConfirmMsg.(startRestoreMsg)
			require.True(t, ok, "confirm emits startRestoreMsg")
			_, cmd := h.Update(start)
			require.NotNil(t, cmd, "the forwarded startRestoreMsg dispatches the restore")
			_, ok = cmd().(instanceRestoredMsg)
			require.True(t, ok)
			require.True(t, called, "the confirmed restore fires the RPC")
		})
	}
}

// TestHandleEnter_RemoteArchivedRowRestoresImmediately is the safe-side companion:
// an archived row restores from the branch the archive already pushed, so nothing
// unpushed is at risk and it restores at once — even on a remote backend.
func TestHandleEnter_RemoteArchivedRowRestoresImmediately(t *testing.T) {
	h := newTestHome(t)
	inst := newDeadInstance(t, "remote-archived", session.Archived)
	inst.SetBackend(&reprovisioningBackend{FakeBackend: session.NewFakeBackend()})
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	prev := restoreSessionThroughDaemon
	restoreSessionThroughDaemon = func(daemon.RestoreSessionRequest) (string, error) { return "/p", nil }
	t.Cleanup(func() { restoreSessionThroughDaemon = prev })

	model, cmd := h.handleEnter()
	h = model.(*home)

	require.Equal(t, stateDefault, h.state, "an archived restore needs no confirmation")
	require.Equal(t, session.OpRestoring, inst.GetInFlightOp(), "the archived row restores immediately")
	require.NotNil(t, cmd)
}

// TestInteractiveGuard_RestingRowsNameTheRestoreKey keeps coverage for the guard
// copy after #2489 moved the row verbs off it and #2479 replaced the CLI-command
// off-ramp with the in-TUI action. A Lost/Dead/archived row that does NOT restore
// (an id-less row, an OpReplacing row, the focused-pane guards) is still fenced,
// and the fence now names the restore KEY, not a shell command.
func TestInteractiveGuard_RestingRowsNameTheRestoreKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  session.Status
		wantMsg string
	}{
		{"lost", session.Lost, "was lost"},
		{"dead", session.Dead, "no longer running"},
		{"archived", session.Archived, "is archived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := newDeadInstance(t, "guarded", tc.status)
			err := interactiveGuard(inst)
			require.Error(t, err, "a resting row still fences the non-restoring guard paths")
			require.Contains(t, err.Error(), tc.wantMsg)
			// The off-ramp names the CONFIGURED restore key (via restoreKeyHint, so a
			// rebind names itself and does not break this) — not a CLI command the user
			// would have to leave the interface to run (#2479).
			require.Contains(t, err.Error(), "press "+restoreKeyHint()+" to restore",
				"the guard names the restore key, not a shell command")
			require.NotContains(t, err.Error(), "af sessions restore",
				"the guard must not send the user out to a CLI command (#2479)")
		})
	}
}
