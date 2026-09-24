package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/configagent"
	"github.com/sachiniyer/agent-factory/session"
)

// These tests drive the highlight passes by hand — pass-1 through
// handleKeyPress, the replay as the reemitKeyMsg pass-1 scheduled — so the
// interleavings the event loop produces under coalesced input are pinned
// deterministically, with no timing. home_update_reemit_test.go covers the same
// race through the real tea.Program loop.

// TestReemitDefersRacingKeyToPostActionState: "m" opens the task manager on
// pass-2 (KeyTaskList -> stateTasks). "n" races it: in stateDefault it would
// be KeyNew (the naming flow); in stateTasks it enters task-create mode. The
// racer must be buffered until "m"'s action has run, then land in the
// post-action state.
func TestReemitDefersRacingKeyToPostActionState(t *testing.T) {
	h := newTestHome(t)

	_, cmd := h.handleKeyPress(runeKey('m'))
	require.NotNil(t, cmd, "pass-1 must schedule the replay")
	require.True(t, h.keySent, "pass-1 arms keySent")
	require.Equal(t, stateDefault, h.state, "the opener's action runs on pass-2, not pass-1")

	_, racingCmd := h.handleKeyPress(runeKey('n'))
	require.Nil(t, racingCmd, "a racing key is buffered, not dispatched or re-emitted")
	require.Len(t, h.deferredKeys, 1)
	require.Equal(t, stateDefault, h.state, "neither key's action has run yet")

	_, _ = h.Update(reemitKeyMsg{runeKey('m')})
	require.False(t, h.keySent, "pass-2 disarms")
	require.Empty(t, h.deferredKeys, "pass-2 drains the buffer")
	require.Equal(t, stateTasks, h.state, "the opener's action opened the task overlay")
	require.True(t, h.automations.TaskPane().IsCreating(),
		"the racing 'n' must reach the task overlay's create mode, not reopen the naming flow")
}

// TestRepeatedPhysicalKeyIsNotTakenForReplay is the Codex #5 case on #4836:
// coalesced "/", "p", "/". The second "/" is a physical press of the same key
// whose replay is still in flight. Matched by key string it would pass for
// that replay, open search, and let the real replay land afterwards as query
// text racing the drained "p" — "/p" instead of the typed "p/". Tagging the
// replay makes the second press an ordinary racing key.
func TestRepeatedPhysicalKeyIsNotTakenForReplay(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(freshLocalInstance(t, "p-slash"))

	_, _ = h.handleKeyPress(runeKey('/'))
	require.True(t, h.keySent)
	_, _ = h.handleKeyPress(runeKey('p'))
	_, _ = h.handleKeyPress(runeKey('/'))
	require.Equal(t, stateDefault, h.state,
		"a physical press of the pending key must not stand in for its replay")
	require.Len(t, h.deferredKeys, 2, "both physical keys wait behind the replay")

	_, _ = h.Update(reemitKeyMsg{runeKey('/')})
	require.Equal(t, stateSearch, h.state, "the replay opens search")
	require.NotNil(t, h.searchOverlay)
	require.Equal(t, "p/", h.searchOverlay.Query(),
		"the racing keys must land in the order they were typed")
	require.False(t, h.keySent, "no stale arm may outlive the replay")
	require.Empty(t, h.deferredKeys)
}

// TestDeferredKeyDoesNotWaitForSlowActionCommand is the Codex #4 case on
// #4836: "C" returns a command that waits out the config agent's readiness —
// up to 60s. A key that raced "C" must dispatch as soon as "C"'s action has
// run, not after that command returns. The spawn never runs here at all, which
// is the point: the racing "2" has already jumped tabs before anyone executes
// the command "C" returned.
func TestDeferredKeyDoesNotWaitForSlowActionCommand(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "slow-cmd")
	selectInstance(h, inst)
	stubTabDaemonSeams(t, inst)
	_, _ = h.createNewTab(h.sidebar.GetSelectedInstance(), session.TabKindShell)
	_, _ = h.handleTabJump(1)
	require.Equal(t, 0, h.store.ActiveTab())

	spawned := 0
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) (string, string, error) {
		spawned++
		return "af-config-1", "", nil
	}))

	_, _ = h.handleKeyPress(runeKey('C'))
	require.True(t, h.keySent)
	_, _ = h.handleKeyPress(runeKey('2'))
	require.Equal(t, 0, h.store.ActiveTab(), "the racing key waits behind C's replay")

	_, cmd := h.Update(reemitKeyMsg{runeKey('C')})
	require.NotNil(t, cmd, "C's action returns its spawn command")
	require.True(t, h.configAgentSpawning, "C's action ran")
	require.Equal(t, 1, h.store.ActiveTab(),
		"the racing '2' must dispatch once C's action has run, without waiting for its spawn command")
	require.Empty(t, h.deferredKeys)
	require.Zero(t, spawned, "nothing may have executed C's command to get here")
}

// TestDeferredKeyWaitsForInteractiveTransition is the other half of Codex #4:
// an action whose state transition itself arrives as a message must still be
// ordered before the keys that raced it. Enter on a focused live pane returns
// the enterInteractiveMsg that moves the keyboard into the pane; an "x" typed
// behind the Enter belongs in the pane, after the forwarded Enter — not to the
// nav-mode handlers, which would run it as a host action.
func TestDeferredKeyWaitsForInteractiveTransition(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	require.NotNil(t, h.focusedOpenPane(), "precondition: the live pane has focus")

	enter := tea.KeyMsg{Type: tea.KeyEnter}
	_, _ = h.handleKeyPress(enter)
	require.True(t, h.keySent)
	_, _ = h.handleKeyPress(runeKey('x'))

	_, cmd := h.Update(reemitKeyMsg{enter})
	require.False(t, h.interactive, "activation arrives as a message, not inside the Enter's dispatch")
	require.Len(t, h.deferredKeys, 1,
		"the racing key must keep waiting until the interactive transition lands")

	runHermeticCmd(t, h, cmd, 0)
	require.True(t, h.interactive)
	require.Len(t, *fakes, 1)
	require.Equal(t, []string{"enter", "x"}, (*fakes)[0].keys,
		"the racing key must reach the pane after the forwarded Enter")
	require.Empty(t, h.deferredKeys)
}

// TestCtrlCBypassesDeferral keeps ctrl+c's always-on hard exit when it races a
// highlighted opener: buffered, it would be consumed by the search overlay "/"
// opens as merely "close".
func TestCtrlCBypassesDeferral(t *testing.T) {
	h := newTestHome(t)

	_, _ = h.handleKeyPress(runeKey('/'))
	_, _ = h.handleKeyPress(runeKey('p'))
	_, cmd := h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC})
	require.True(t, reachesQuit(cmd), "ctrl+c must hard-exit even while a replay is in flight")
	require.Empty(t, h.deferredKeys, "the hard exit discards buffered input")
}

// TestGracefulQuitDropsDeferredKeys: the quit key's action has closed the live
// attachments by the time its replay returns, so an Enter that raced it must
// not be dispatched behind the teardown.
func TestGracefulQuitDropsDeferredKeys(t *testing.T) {
	h := newTestHome(t)

	_, _ = h.handleKeyPress(runeKey('q'))
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	_, cmd := h.Update(reemitKeyMsg{runeKey('q')})
	require.True(t, h.quitting)
	require.True(t, reachesQuit(cmd))
	require.Empty(t, h.deferredKeys, "input buffered behind a graceful quit is dropped")
	require.False(t, h.keySent, "the dropped Enter must not have armed a new pass")
}

// TestStaleInteractiveRequestDoesNotDrain: mouse input is not gated, so a
// second interactive request can be issued while the first one's
// enterInteractiveMsg is still in flight. Only the latest request owns the
// gate; the older message landing first must not drain keys meant for the
// newer target.
func TestStaleInteractiveRequestDoesNotDrain(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	p := h.focusedOpenPane()
	require.NotNil(t, p)

	_, first := h.requestInteractive(p, nil)
	_, second := h.requestInteractive(p, nil)
	_, _ = h.handleKeyPress(runeKey('x'))
	require.Len(t, h.deferredKeys, 1)

	_, _ = h.Update(first())
	require.Len(t, h.deferredKeys, 1,
		"an older request's activation must not drain input queued behind the newer request")

	_, _ = h.Update(second())
	require.True(t, h.interactive)
	require.Empty(t, h.deferredKeys)
	require.Len(t, *fakes, 1)
	require.Equal(t, []string{"x"}, (*fakes)[0].keys)
}

// TestFailedActivationDropsPaneBoundKeys: keys buffered behind an interactive
// entry were typed as pane input. When activation fails — here the pane closes
// before the message lands — they must not run as host commands instead (a
// queued D would start a kill of whatever the tree has selected).
func TestFailedActivationDropsPaneBoundKeys(t *testing.T) {
	h, _, _ := interactiveTestHome(t)
	p := h.focusedOpenPane()
	require.NotNil(t, p)

	_, cmd := h.requestInteractive(p, nil)
	_, _ = h.handleKeyPress(runeKey('D'))
	require.Len(t, h.deferredKeys, 1)
	h.closePaneWindow(p)

	_, _ = h.Update(cmd())
	require.False(t, h.interactive, "activation of a closed pane fails")
	require.Empty(t, h.deferredKeys, "pane-bound input with no pane is dropped")
	require.False(t, h.keySent, "the dropped D must not have started its host action")
	require.Equal(t, stateDefault, h.state)
}

// TestCtrlCRacingNamingActionKeepsOrder: in the naming form ctrl+c is not the
// hard exit — it cancels the draft — so it must not jump the queue. Tab then
// ctrl+c opens the program picker and closes it again, keeping the draft.
func TestCtrlCRacingNamingActionKeepsOrder(t *testing.T) {
	h := activeProjectHome(t)
	resizeHome(h, 80, 24)
	_, _ = h.startNewInstance()
	require.Equal(t, stateNew, h.state)
	naming := h.namingInstance
	require.NotNil(t, naming)

	tab := tea.KeyMsg{Type: tea.KeyTab}
	_, _ = h.handleKeyPress(tab)
	require.True(t, h.keySent)
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC})
	require.Len(t, h.deferredKeys, 1, "a naming-form ctrl+c waits behind the pending Tab")
	require.Same(t, naming, h.namingInstance, "the draft must survive until the Tab has run")

	_, _ = h.Update(reemitKeyMsg{tab})
	require.Equal(t, stateNew, h.state, "Tab opened the picker and the ctrl+c closed it")
	require.Nil(t, h.selectionOverlay)
	require.Same(t, naming, h.namingInstance, "the naming draft is preserved")
	require.Empty(t, h.deferredKeys)
}
