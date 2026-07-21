package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// A confirmation records intent about one session, not whichever row happens
// to carry the same display title when the user eventually confirms. Snapshot
// reconciliation may replace the original row while the modal is open after a
// concurrent kill+recreate (#2358).
func TestHandleKill_ConfirmationDoesNotTargetSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	original := newKillableInstance(t, "worker")
	h.store.AddInstance(original)
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.handleKill()
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state)

	replacement := newKillableInstance(t, original.Title)
	require.NotEqual(t, original.ID, replacement.ID)
	require.True(t, h.store.ReplaceInstance(original, replacement))

	model, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	h = model.(*home)
	require.Nil(t, cmd, "a stale confirmation must not dispatch a kill")
	require.Equal(t, session.OpNone, replacement.GetInFlightOp(),
		"a stale confirmation must not mark the replacement as deleting")
}

func TestHandleArchive_ConfirmationDoesNotTargetSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	original := archiveActionInstance(t, "worker", session.Ready)
	h.store.AddInstance(original)
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.handleArchive()
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state)

	replacement := archiveActionInstance(t, original.Title, session.Ready)
	require.NotEqual(t, original.ID, replacement.ID)
	require.True(t, h.store.ReplaceInstance(original, replacement))

	model, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	h = model.(*home)
	require.Nil(t, cmd, "a stale confirmation must not dispatch an archive")
	require.Equal(t, session.OpNone, replacement.GetInFlightOp(),
		"a stale confirmation must not mark the replacement as archiving")
}

// The RPC completion is another retained-intent boundary. A reconcile may swap
// in a same-title replacement after the daemon call starts but before its result
// reaches the event loop; the old completion must not mutate that new row.
func TestHandleInstanceKilled_CompletionDoesNotRemoveSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	original := newKillableInstance(t, "worker")
	h.store.AddInstance(original)
	target := captureSessionActionTarget(original, h.repoID)

	replacement := newKillableInstance(t, original.Title)
	require.True(t, h.store.ReplaceInstance(original, replacement))

	_, _ = h.handleInstanceKilled(instanceKilledMsg{target: target})
	require.Same(t, replacement, h.store.GetInstanceByTitle(original.Title),
		"the old kill completion must not remove the replacement")
}

func TestHandleInstanceArchived_CompletionDoesNotArchiveSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	original := archiveActionInstance(t, "worker", session.Ready)
	h.store.AddInstance(original)
	target := captureSessionActionTarget(original, h.repoID)

	replacement := archiveActionInstance(t, original.Title, session.Ready)
	require.True(t, h.store.ReplaceInstance(original, replacement))

	_, _ = h.handleInstanceArchived(instanceArchivedMsg{target: target})
	require.Equal(t, session.Ready, replacement.GetStatus(),
		"the old archive completion must not archive the replacement")
}

func TestKillInstanceCmd_PreservesCapturedStableIdentity(t *testing.T) {
	h := newTestHome(t)
	target := sessionActionTarget{id: "worker-id", title: "worker", repoID: h.repoID}

	var got daemon.KillSessionRequest
	previous := killSessionThroughDaemon
	killSessionThroughDaemon = func(request daemon.KillSessionRequest) error {
		got = request
		return nil
	}
	t.Cleanup(func() { killSessionThroughDaemon = previous })

	msg := h.killInstanceCmd(target)()
	done, ok := msg.(instanceKilledMsg)
	require.True(t, ok)
	require.Equal(t, target.killRequest(), got)
	require.Equal(t, target, done.target)
}

func TestFailedActionCompletionDoesNotClearSameTitleReplacementOperation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		begin      session.TransitionEvent
		wantOp     session.InFlightOp
		completion func(*home, sessionActionTarget)
	}{
		{
			name:   "kill",
			begin:  session.BeginKill(),
			wantOp: session.OpKilling,
			completion: func(h *home, target sessionActionTarget) {
				_, _ = h.handleInstanceKilled(instanceKilledMsg{target: target, err: errors.New("old kill failed")})
			},
		},
		{
			name:   "archive",
			begin:  session.BeginArchive(),
			wantOp: session.OpArchiving,
			completion: func(h *home, target sessionActionTarget) {
				_, _ = h.handleInstanceArchived(instanceArchivedMsg{target: target, err: errors.New("old archive failed")})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			original := archiveActionInstance(t, "worker", session.Ready)
			h.store.AddInstance(original)
			target := captureSessionActionTarget(original, h.repoID)

			replacement := archiveActionInstance(t, original.Title, session.Ready)
			require.NoError(t, replacement.Transition(tc.begin))
			require.True(t, h.store.ReplaceInstance(original, replacement))

			tc.completion(h, target)
			require.Equal(t, tc.wantOp, replacement.GetInFlightOp(),
				"the old failure must not clear the replacement's own operation")
		})
	}
}
