package app

import (
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// TestReemitDefersRacingKeyToPostActionState is a deterministic guard of the
// fix's mechanism — the keySent guard distinguishes pass-2 (the re-emitted
// opener, same identity) from a key that raced the re-emit onto p.msgs
// (different identity), and buffers the racer so it is replayed through the
// normal path AFTER the opener's action rather than swallowed in the
// pre-action state or re-emitted through its own goroutine.
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
	require.NotNil(t, cmd, "pass-1 must re-emit the opener for pass-2")
	require.True(t, h.keySent, "pass-1 arms keySent")
	require.Equal(t, "m", h.pendingKey, "pass-1 records the pending key identity")
	require.Equal(t, stateDefault, h.state, "the opener's action runs on pass-2, not pass-1")

	// Racing follow-up "n" arrives before "m"'s pass-2 re-emit resolves. The
	// guard buffers it (rather than re-emitting through its own goroutine,
	// which would race its siblings) and leaves the arm in place so "m"'s
	// pass-2 still dispatches.
	_, racingCmd := h.handleKeyPress(runeKey('n'))
	require.Nil(t, racingCmd, "racing key must be buffered, not re-emitted through its own command")
	require.Len(t, h.deferredKeys, 1, "racing key must be appended to the deferred buffer")
	require.Equal(t, "n", h.deferredKeys[0].String(), "the buffered key must be the racer")
	require.True(t, h.keySent, "the pending pass-2 arm must stay armed so the opener's action still runs")
	require.Equal(t, "m", h.pendingKey, "the pending identity must stay the opener's, not the racer's")
	require.Equal(t, stateDefault, h.state, "neither key's action has run yet")

	// "m"'s pass-2 re-emit lands: same identity as pending -> dispatch the
	// opener. The handleKeyPress wrapper wraps the opener's action cmd with a
	// tea.Sequence that, after the opener's transition, replays the buffered
	// "n" in arrival order. sequenceMsg is unexported, so unpack the Sequence
	// reflectively (see remote_detach_reset_test.go): it is a slice of tea.Cmd,
	// ordered opener-action then drained keys.
	_, drainCmd := h.handleKeyPress(runeKey('m'))
	require.False(t, h.keySent, "pass-2 clears the arm")
	require.Equal(t, "", h.pendingKey, "pass-2 clears the pending identity")
	require.Empty(t, h.deferredKeys, "pass-2 must drain the buffer into the returned Sequence")
	require.Equal(t, stateTasks, h.state, "the opener's action opened the task overlay")

	seq := reflect.ValueOf(drainCmd())
	require.Equal(t, reflect.Slice, seq.Kind(),
		"pass-2 must drain via a tea.Sequence (opener action, then buffered keys in order), got %T", drainCmd())
	require.GreaterOrEqual(t, seq.Len(), 1, "sequence must contain the drained racing key")
	replay, ok := seq.Index(seq.Len() - 1).Interface().(tea.Cmd)
	require.True(t, ok, "the final sequenced entry must be a tea.Cmd")
	replayMsg, ok := replay().(tea.KeyMsg)
	require.True(t, ok, "the drained entry must replay the buffered racing KeyMsg, got %T", replay())
	require.Equal(t, "n", replayMsg.String(), "the drained key must be the buffered racer in order")

	// Drive the replayed "n" through the normal path. It must land in
	// stateTasks (the post-action state) and enter create mode — the proof it
	// was deferred past the opener's transition rather than swallowed in
	// stateDefault before "m" opened the overlay.
	_, _ = h.handleKeyPress(replayMsg)
	require.Equal(t, stateTasks, h.state, "the drained key must land inside the task overlay, not reopen the naming flow")
	require.True(t, h.automations.TaskPane().IsCreating(),
		"deferred 'n' must reach the task overlay's create mode, not be swallowed in stateDefault before 'm' opened it")
}
