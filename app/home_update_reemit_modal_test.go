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
// request for pane B can be issued while pane A's enterInteractiveMsg is still
// in flight. Only the latest request's pane owns the gate; A's activation
// landing first must not drain keys meant for B.
func TestStaleInteractiveRequestDoesNotDrain(t *testing.T) {
	h, _, _ := interactiveTestHome(t)
	resizeHome(h, 200, 40)
	paneA := h.focusedOpenPane()
	require.NotNil(t, paneA)
	paneB := openTestPane(t, h, startedLocalInstance(t, "bravo"), 0)
	require.NotSame(t, paneA, paneB)

	_, toA := h.requestInteractive(paneA, nil)
	_, toB := h.requestInteractive(paneB, nil)
	_, _ = h.handleKeyPress(runeKey('x'))
	require.Len(t, h.deferredKeys, 1)

	_, _ = h.Update(toA())
	require.Len(t, h.deferredKeys, 1,
		"an older request's activation must not drain input queued behind the newer request")

	_, _ = h.Update(toB())
	require.True(t, h.interactive)
	require.Same(t, paneB, h.focusedOpenPane())
	require.Empty(t, h.deferredKeys)
	fake := focusedFake(h)
	require.NotNil(t, fake)
	require.Equal(t, []string{"x"}, fake.keys, "the queued key lands in the latest target, B")
}

// pendingInteractiveHome arms the interactive gate the way a help-seen Enter
// does and queues one pane-bound key behind it. The request's command is
// discarded: these tests cover the exits where its enterInteractiveMsg does not
// release the gate.
func pendingInteractiveHome(t *testing.T) (*home, *session.Instance) {
	t.Helper()
	h, inst, _ := interactiveTestHome(t)
	p := h.focusedOpenPane()
	require.NotNil(t, p)
	_, cmd := h.requestInteractive(p, nil)
	require.NotNil(t, cmd)
	require.True(t, h.awaitingInteractive, "precondition: the request is pending")
	_, _ = h.handleKeyPress(runeKey('x'))
	require.Len(t, h.deferredKeys, 1, "precondition: pane input is queued")
	return h, inst
}

// requireNavKeyHandled is the property every exit from a pending interactive
// request must restore: a plain nav key is handled at once, and the queued pane
// input is dropped rather than run as host commands.
func requireNavKeyHandled(t *testing.T, h *home) {
	t.Helper()
	require.False(t, h.awaitingInteractive, "the pending request must be released")
	_, _ = h.handleKeyPress(runeKey('j'))
	require.Empty(t, h.deferredKeys, "the nav key must not be parked, and the pane input must be dropped")
	require.True(t, h.keySent, "the nav key must reach its highlight pass at once")
}

// TestFailedActivationDropsPaneBoundKeys: the enterInteractiveMsg lands but
// activation refuses (the session went lost first). The message's own release
// runs, and the pane-bound keys are dropped rather than run as host commands (a
// queued D would start a kill of whatever the tree has selected).
func TestFailedActivationDropsPaneBoundKeys(t *testing.T) {
	h, inst, _ := interactiveTestHome(t)
	p := h.focusedOpenPane()
	require.NotNil(t, p)

	_, cmd := h.requestInteractive(p, nil)
	_, _ = h.handleKeyPress(runeKey('D'))
	require.Len(t, h.deferredKeys, 1)
	_ = inst.Transition(session.ObserveLiveness(session.LiveLost))

	_, _ = h.Update(cmd())
	require.False(t, h.interactive, "activation of a lost session fails")
	require.Empty(t, h.deferredKeys, "pane-bound input with no pane is dropped")
	require.False(t, h.keySent, "the dropped D must not have started its host action")
	require.Equal(t, stateDefault, h.state)
	requireNavKeyHandled(t, h)
}

// TestPendingInteractiveReleasedWhenPaneCloses: the awaited pane is closed
// while the request is pending (closePaneWindow's release).
func TestPendingInteractiveReleasedWhenPaneCloses(t *testing.T) {
	h, _ := pendingInteractiveHome(t)
	h.hidePane(h.awaitingPane)
	requireNavKeyHandled(t, h)
}

// TestPendingInteractiveReleasedWhenSessionKilled: the kill finishes while the
// request is pending; the finalize removes the row and prunes its pane
// (closePaneWindow's release).
func TestPendingInteractiveReleasedWhenSessionKilled(t *testing.T) {
	h, inst := pendingInteractiveHome(t)
	_, _ = h.handleInstanceKilled(instanceKilledMsg{target: captureSessionActionTarget(inst, h.repoID)})
	require.Zero(t, h.store.NumOpenPanes(), "precondition: the kill closed the pane")
	requireNavKeyHandled(t, h)
}

// TestPendingInteractiveReleasedWhenSessionArchived: the archive finishes while
// the request is pending; archiving closes the session's panes
// (closePaneWindow's release).
func TestPendingInteractiveReleasedWhenSessionArchived(t *testing.T) {
	h, inst := pendingInteractiveHome(t)
	h.handleInstanceArchived(instanceArchivedMsg{target: captureSessionActionTarget(inst, h.repoID)})
	require.Zero(t, h.store.NumOpenPanes(), "precondition: the archive closed the pane")
	requireNavKeyHandled(t, h)
}

// TestPendingInteractiveReleasedWhenSessionLost: the session goes lost while
// its pane stays open (expirePendingInteractive's liveness release).
func TestPendingInteractiveReleasedWhenSessionLost(t *testing.T) {
	h, inst := pendingInteractiveHome(t)
	_ = inst.Transition(session.ObserveLiveness(session.LiveLost))
	require.Equal(t, 1, h.store.NumOpenPanes(), "precondition: a lost session keeps its pane")
	_, _ = h.handleKeyPress(runeKey('j'))
	require.False(t, h.awaitingInteractive, "the pending request must be released")
	require.Empty(t, h.deferredKeys, "the nav key must not be parked, and the pane input must be dropped")
	require.True(t, h.keySent, "the nav key must reach its highlight pass at once")
}

// TestPendingInteractiveReleasedWhenKillStarts: a kill is in flight — the row
// is tearing down but its pane is still open (expirePendingInteractive's
// teardown release).
func TestPendingInteractiveReleasedWhenKillStarts(t *testing.T) {
	h, inst := pendingInteractiveHome(t)
	inst.SetInFlightOpForTest(session.OpKilling)
	_, _ = h.handleKeyPress(runeKey('j'))
	require.False(t, h.awaitingInteractive, "the pending request must be released")
	require.Empty(t, h.deferredKeys)
	require.True(t, h.keySent)
}

// TestPendingInteractiveReleasedWhenAnotherScreenOpens: an async overlay (here
// the general help) takes the keyboard while the request is pending — the same
// state reset a project switch or confirmation produces. The key meant for that
// screen must reach it, not queue behind an activation that would refuse
// (expirePendingInteractive's state release).
func TestPendingInteractiveReleasedWhenAnotherScreenOpens(t *testing.T) {
	h, _ := pendingInteractiveHome(t)
	_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)
	require.Equal(t, stateHelp, h.state)

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	require.False(t, h.awaitingInteractive, "the pending request must be released")
	require.Equal(t, stateDefault, h.state, "Esc reached the help screen and closed it")
	requireNavKeyHandled(t, h)
}

// TestDismissedInteractiveHelpNeverArmsTheGate: with the first-run interactive
// help unseen, the request parks on the help screen instead of arming the gate.
// That screen has no cancel — every dismiss key, Esc included, continues into
// activation through its own enterInteractiveMsg — so dismissing it leaves no
// pending request to release, even before that message lands.
func TestDismissedInteractiveHelpNeverArmsTheGate(t *testing.T) {
	h, _ := liveTestHome(t)
	_, _ = stubLiveTermFactory(t)
	p := h.focusedOpenPane()
	require.NotNil(t, p)

	_, _ = h.requestInteractive(p, nil)
	require.Equal(t, stateHelp, h.state, "precondition: the first-run help is showing")
	require.False(t, h.awaitingInteractive, "the help screen owns the keyboard; no gate is armed")

	_, dismissCmd := h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	require.NotNil(t, dismissCmd, "the dismissal carries the activation")
	require.Equal(t, stateDefault, h.state, "Esc dismisses the help screen")
	requireNavKeyHandled(t, h)
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
