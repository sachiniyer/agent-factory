package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

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
