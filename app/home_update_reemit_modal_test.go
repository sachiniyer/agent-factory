package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// TestReemitDefersRacingKeyToPostActionState is a deterministic guard of the
// fix's mechanism — the keySent guard distinguishes pass-2 (the re-emitted
// opener, same identity) from a key that raced the re-emit onto p.msgs
// (different identity), and re-emits the racer so it is handled in the
// post-action state rather than swallowed in the pre-action state.
//
// "m" opens the task manager on pass-2 (KeyTaskList -> showTasksOverlay ->
// stateTasks). "n" is the racing follow-up: in stateDefault it is KeyNew
// (opens the naming flow); in stateTasks it enters task-create mode. This
// drives the passes directly (no event loop, no timing) for a DIFFERENT modal
// than the search case in home_update_reemit_test.go, so a regression in the
// guard's identity check is caught regardless of which modal triggers it.
func TestReemitDefersRacingKeyToPostActionState(t *testing.T) {
	h := newTestHome(t)

	// pass-1 of the mapped opener "m": paint highlight + arm the pass-2 re-emit.
	_, cmd := h.handleKeyPress(runeKey('m'))
	require.NotNil(t, cmd)
	require.True(t, h.keySent, "pass-1 arms keySent")
	require.Equal(t, "m", h.pendingKey, "pass-1 records the pending key identity")
	require.Equal(t, stateDefault, h.state, "the opener's action runs on pass-2, not pass-1")

	// Racing follow-up "n" arrives before "m"'s pass-2 re-emit resolves. The
	// guard re-emits it (returnEarly) and leaves the arm in place so "m"'s
	// pass-2 still completes; the pre-fix drop-through would clear keySent and
	// dispatch "n" in stateDefault (opening the naming flow instead of
	// deferring it).
	_, racingCmd := h.handleKeyPress(runeKey('n'))
	require.NotNil(t, racingCmd, "racing key must be re-emitted, not dropped into the pre-action state")
	require.IsType(t, tea.KeyMsg{}, racingCmd(), "the re-emit must reproduce the racing KeyMsg")
	require.True(t, h.keySent, "the pending pass-2 arm must stay armed so the opener's action still runs")
	require.Equal(t, "m", h.pendingKey, "the pending identity must stay the opener's, not the racer's")
	require.Equal(t, stateDefault, h.state, "neither key's action has run yet")

	// "m"'s pass-2 re-emit lands: same identity as pending -> dispatch the opener.
	_, _ = h.handleKeyPress(runeKey('m'))
	require.False(t, h.keySent, "pass-2 clears the arm")
	require.Equal(t, "", h.pendingKey)
	require.Equal(t, stateTasks, h.state, "the opener's action opened the task overlay")

	// "n"'s deferred re-emit now lands in stateTasks (not stateDefault):
	// entering create mode is the proof it was handled inside the modal.
	_, _ = h.handleKeyPress(runeKey('n'))
	require.Equal(t, stateTasks, h.state)
	require.True(t, h.automations.TaskPane().IsCreating(),
		"deferred 'n' must reach the task overlay's create mode, not be swallowed in stateDefault before 'm' opened it")
}
