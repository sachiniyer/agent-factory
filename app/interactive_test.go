package app

import (
	"fmt"
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// ----------------------------------------------------------------------------
// Interactive mode (#1089 PR 2, RFC §2.3): Enter on a live pane forwards ALL
// keystrokes (including Tab) to the pane's attachment; Ctrl-] — and only
// Ctrl-] — returns to nav mode; `o` keeps the full-screen attach; bindings
// that cannot embed fall back to full-screen.
// ----------------------------------------------------------------------------

// interactiveTestHome is liveTestHome with the first-time interactive help
// screen marked seen, so Enter activates synchronously through the deferred
// enterInteractiveMsg instead of parking on the overlay.
func interactiveTestHome(t *testing.T) (*home, *session.Instance, *[]*fakeLiveTerm) {
	t.Helper()
	h, inst := liveTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	fakes, _ := stubLiveTermFactory(t)
	return h, inst, fakes
}

// runHermeticCmd executes a cmd tree (unpacking batch/sequence slices
// reflectively — both are unexported []tea.Cmd types) and feeds every
// produced msg back into Update. Only for cmds known to be hermetic (the
// deferred interactive activation, WindowSize) — never selectionChanged's
// capture/PR-fetch dispatches.
func runHermeticCmd(t *testing.T, h *home, cmd tea.Cmd, depth int) {
	t.Helper()
	require.Less(t, depth, 8, "cmd tree too deep — unexpected recursion")
	if cmd == nil {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}
	v := reflect.ValueOf(msg)
	if v.Kind() == reflect.Slice {
		for i := 0; i < v.Len(); i++ {
			if c, ok := v.Index(i).Interface().(tea.Cmd); ok {
				runHermeticCmd(t, h, c, depth+1)
			}
		}
		return
	}
	_, next := h.Update(msg)
	runHermeticCmd(t, h, next, depth+1)
}

// enterInteractive presses Enter on the focused pane and drives the deferred
// activation to completion. That entry Enter is itself forwarded into the pane
// (#1576) — TestFirstEnterOnFocusedPaneForwardsIntoPane pins that behavior. The
// recorded keys are reset here so callers can assert on the keystrokes they
// send NEXT without the entry Enter sitting in front of them.
func enterInteractive(t *testing.T, h *home) {
	t.Helper()
	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	runHermeticCmd(t, h, cmd, 0)
	if lt, ok := h.liveTerm.(*fakeLiveTerm); ok {
		lt.keys = nil
	}
}

func TestEnterOnFocusedLivePaneEntersInteractive(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)

	enterInteractive(t, h)

	assert.True(t, h.interactive, "Enter on a live-eligible focused pane must enter interactive mode")
	require.Len(t, *fakes, 1, "activation must bind the live attachment immediately, not on the next tick")
	p := h.focusedOpenPane()
	require.NotNil(t, p)
	assert.Equal(t, p, h.livePane)
	assert.True(t, h.paneWindows[p.ID()].Interactive(), "the pane must carry the interactive visual cue")
	assert.Contains(t, h.menu.String(), "ctrl+]", "the status bar must show the escape hatch")
}

// TestFirstEnterOnFocusedPaneForwardsIntoPane is the #1576 regression: with the
// interactive help already seen (every established user), pressing Enter on an
// already-focused pane must both enter interactive mode AND forward that first
// Enter into the pane — the transition key is the pane's first in-pane
// keystroke and must not be swallowed. Before the fix only the once-ever
// first-run help path replayed the entry key, so every later entry dropped it.
func TestFirstEnterOnFocusedPaneForwardsIntoPane(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)

	// Drive the entry WITHOUT the key-resetting enterInteractive helper so the
	// forwarded entry keystroke is observable.
	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	runHermeticCmd(t, h, cmd, 0)

	require.True(t, h.interactive, "Enter on a focused pane must enter interactive mode")
	require.Len(t, *fakes, 1)
	assert.Equal(t, []string{"enter"}, (*fakes)[0].keys,
		"the Enter that enters a focused pane must forward into the pane, not be swallowed (#1576)")

	// And the pane keeps forwarding normally afterwards.
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	assert.Equal(t, []string{"enter", "z"}, (*fakes)[0].keys,
		"subsequent keystrokes must forward on top of the entry Enter")
}

// TestEnterFromTreeDoesNotForwardIntoPane guards the other half of #1576: a
// navigation Enter (tree/sidebar selection) opens+enters the pane but must NOT
// type into the agent — only an already-focused-pane Enter forwards.
func TestEnterFromTreeDoesNotForwardIntoPane(t *testing.T) {
	h := newTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	inst := startedLocalInstance(t, "tree-noforward")
	selectInstance(h, inst)
	resizeHome(h, 120, 40)
	fakes, _ := stubLiveTermFactory(t)
	require.Zero(t, h.store.NumOpenPanes())

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	p := h.store.FindOpenPane(inst, 0)
	require.NotNil(t, p, "Enter from the tree must open the selection's pane")
	_, cmd := h.Update(enterInteractiveMsg{pane: p})
	runHermeticCmd(t, h, cmd, 0)

	require.True(t, h.interactive)
	require.Len(t, *fakes, 1)
	assert.Empty(t, (*fakes)[0].keys,
		"a tree/nav select Enter opens the pane but must not type into the agent (#1576)")
}

func TestFirstRunInteractiveHelpForwardsDismissKey(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	require.Nil(t, cmd, "first-time interactive entry waits on the help overlay")
	require.Equal(t, stateHelp, h.state)
	require.NotNil(t, h.textOverlay)
	require.Empty(t, *fakes, "the live terminal must not bind until help is dismissed")

	_, cmd = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	runHermeticCmd(t, h, cmd, 0)

	require.True(t, h.interactive, "dismissing first-run help should enter interactive mode")
	require.Len(t, *fakes, 1)
	assert.Equal(t, []string{"q"}, (*fakes)[0].keys,
		"the key that dismisses the first-run interactive help must reach the pane")
}

func TestInteractiveForwardsAllKeysIncludingTab(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	enterInteractive(t, h)
	require.Len(t, *fakes, 1)
	fake := (*fakes)[0]

	ringBefore := h.ring.Active()
	for _, msg := range []tea.KeyMsg{
		{Type: tea.KeyTab},                       // host focus key in nav — forwards here
		{Type: tea.KeyRunes, Runes: []rune("q")}, // nav quit key — forwards here
		{Type: tea.KeyRunes, Runes: []rune("1")}, // nav tab-jump digit — forwards here
		{Type: tea.KeyCtrlC},                     // forwards (quit still reachable via nav)
		{Type: tea.KeyRunes, Runes: []rune("x")}, // nav hide-pane key — forwards here
		{Type: tea.KeyLeft},                      // nav pane-switch key — forwards here
		{Type: tea.KeyRight},                     // nav pane-switch key — forwards here
		{Type: tea.KeyEnter},                     //
		{Type: tea.KeyCtrlW},                     // the full-screen detach key — not host-reserved here
	} {
		_, cmd := h.handleKeyPress(msg)
		assert.Nil(t, cmd, "forwarded keys must not trigger host actions (%s)", msg.String())
	}

	assert.Equal(t, []string{"tab", "q", "1", "ctrl+c", "x", "left", "right", "enter", "ctrl+w"}, fake.keys,
		"every keystroke must forward to the pane's attachment")
	assert.True(t, h.interactive, "forwarding must not leave interactive mode")
	assert.Equal(t, ringBefore, h.ring.Active(), "Tab must not cycle host focus while interactive")
	assert.Equal(t, 1, h.store.NumOpenPanes(), "x must not hide the pane while interactive")
}

func TestCtrlCloseBracketReturnsToNav(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	enterInteractive(t, h)
	fake := (*fakes)[0]

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})

	assert.False(t, h.interactive, "Ctrl-] must return to nav mode")
	assert.Empty(t, fake.keys, "Ctrl-] is host-reserved: it must never forward")
	p := h.focusedOpenPane()
	require.NotNil(t, p, "focus stays on the pane after leaving interactive (RFC §2.3)")
	assert.False(t, h.paneWindows[p.ID()].Interactive(), "the visual cue must clear")
	assert.False(t, (*fakes)[0].closed, "leaving interactive keeps the live attachment (render continues)")

	// Nav keys work again: Tab cycles the focus ring instead of forwarding.
	// (Direct dispatch, like pressTab — handleKeyPress would take the menu
	// highlight re-emit detour first.)
	before := h.ring.Active()
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.NotEqual(t, before, h.ring.Active(), "Tab must cycle focus again in nav mode")
	assert.Empty(t, fake.keys)
}

func TestInteractiveEndsWhenClientDies(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	enterInteractive(t, h)
	fake := (*fakes)[0]

	close(fake.done)
	h.syncLiveTermPane()

	assert.False(t, h.interactive, "a dead attach client must drop the TUI back to nav mode")

	// A keystroke racing the death (before the next tick) is swallowed, not
	// mistyped: re-enter interactive, kill the client, then type.
	h.liveBindFailedAt = h.liveBindFailedAt.Add(-2 * liveBindRetryInterval)
	enterInteractive(t, h)
	require.Len(t, *fakes, 2)
	second := (*fakes)[1]
	close(second.done)
	// The tick hasn't run yet — the very next key must detect the breakage.
	h.reconcileLiveTermPane()
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	assert.False(t, h.interactive)
	assert.Empty(t, second.keys, "keys must never forward into a dead attachment")
}

func TestInteractiveEndsWhenPaneCloses(t *testing.T) {
	h, _, _ := interactiveTestHome(t)
	enterInteractive(t, h)
	p := h.focusedOpenPane()
	require.NotNil(t, p)

	h.hidePane(p)

	assert.False(t, h.interactive, "closing/hiding the pane must end interactive mode")
}

func TestEnterFromTreeOpensPaneAndEntersInteractive(t *testing.T) {
	h := newTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	inst := startedLocalInstance(t, "tree-live")
	selectInstance(h, inst)
	resizeHome(h, 120, 40)
	fakes, _ := stubLiveTermFactory(t)
	require.Zero(t, h.store.NumOpenPanes())

	// Enter on the tree row: opens the selection's pane (the `s` semantics),
	// then enters it. The activation itself arrives as the deferred
	// enterInteractiveMsg; drive it directly — the batched cmd also carries
	// selectionChanged's capture dispatches, which are not hermetic.
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	p := h.store.FindOpenPane(inst, 0)
	require.NotNil(t, p, "Enter from the tree must open the selection's pane")
	assert.Equal(t, p, h.focusedOpenPane(), "the opened pane must take focus")

	_, cmd := h.Update(enterInteractiveMsg{pane: p})
	runHermeticCmd(t, h, cmd, 0)
	assert.True(t, h.interactive)
	require.Len(t, *fakes, 1)
}

func TestEnterOnPreviewedTabRowCommitsAndEntersInteractive(t *testing.T) {
	h := newTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	inst := startedLocalInstance(t, "tab-row-live")
	selectInstance(h, inst)
	resizeHome(h, 120, 40)
	fakes, sessions := stubLiveTermFactory(t)

	pane := openTestPane(t, h, inst, 0)
	require.Equal(t, 0, pane.Tab(), "precondition: the Agent tab is open")

	h.focusRegion(layout.RegionTree)
	pressNav(t, h, "j") // Agent tab row.
	pressNav(t, h, "j") // Terminal tab row.
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.NotNil(t, h.panePreviewTxn, "the terminal row must be a preview before Enter")
	require.Equal(t, 0, pane.Tab(), "preview remains transient until Enter")

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	require.Nil(t, h.panePreviewTxn)
	require.Equal(t, 1, pane.Tab(), "Enter commits the Terminal tab into the pane")
	require.Equal(t, pane, h.focusedOpenPane(), "the committed pane keeps focus")
	runHermeticCmd(t, h, cmd, 0)

	require.True(t, h.interactive, "the same Enter must enter the committed tab")
	require.Equal(t, pane, h.livePane)
	require.Len(t, *fakes, 1)
	require.Equal(t, inst.TabTmuxName(1), (*sessions)[0])

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Z")})
	assert.Equal(t, []string{"Z"}, (*fakes)[0].keys, "typing after one Enter must route into the tab")
}

// TestSecondEnterTargetsCurrentSelectionNotStalePane is the #1233/#1236 P1
// wrong-target-input regression: enter interactive on instance A, Ctrl-] to
// nav, select a DIFFERENT instance B through the real tree-nav keys, Enter
// again. Tree navigation must re-home focus to the tree, so Enter routes to B
// — the current selection — never the pane focus was previously left on (A).
func TestSecondEnterTargetsCurrentSelectionNotStalePane(t *testing.T) {
	h := newTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	instA := startedLocalInstance(t, "alpha")
	instB := startedLocalInstance(t, "bravo")
	h.store.AddInstance(instA)
	h.store.AddInstance(instB)
	h.sidebar.SetSelectedInstance(0) // land on A
	h.store.SetSelectedInstance(instA)
	h.clampSelectionTab()
	resizeHome(h, 200, 40) // wide enough for both panes side by side
	_, _ = stubLiveTermFactory(t)

	// 1. Enter interactive on A (from the tree): opens A's pane, then enters it.
	// openOrFocusPane focuses the pane it opened, so read it back off the focus
	// ring (its tab index tracks the tree cursor, not necessarily 0).
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	paneA := h.focusedOpenPane()
	require.NotNil(t, paneA, "Enter must open A's pane")
	require.Equal(t, instA, paneA.Instance())
	_, cmd := h.Update(enterInteractiveMsg{pane: paneA})
	runHermeticCmd(t, h, cmd, 0)
	require.True(t, h.interactive, "first Enter must enter interactive mode")
	require.Equal(t, instA, h.livePane.Instance(), "first Enter binds A")
	aFake := h.liveTerm.(*fakeLiveTerm)

	// 2. Ctrl-] back to nav. Focus stays on A's pane — the bug precondition.
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	require.False(t, h.interactive, "Ctrl-] must return to nav mode")

	// 3. Navigate to a DIFFERENT instance B through the real tree-nav keys.
	for i := 0; i < 20 && h.sidebar.GetSelectedInstance() != instB; i++ {
		_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyDown}, keys.KeyDown)
	}
	require.Equal(t, instB, h.sidebar.GetSelectedInstance(), "navigation must land on B")

	// 4. Enter again commits the active sidebar preview: the current
	// selection B replaces the focused pane binding, rather than re-entering
	// stale A (#1321 commit-replace preserving the #1233/#1236 target rule).
	// The same Enter also enters the committed pane (#1455).
	_, cmd = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	paneB := h.focusedOpenPane()
	require.NotNil(t, paneB, "second Enter must leave a focused pane")
	require.Same(t, paneA, paneB, "commit-replace keeps the owner pane")
	require.Equal(t, instB, paneB.Instance(), "the committed pane must belong to selected B")
	runHermeticCmd(t, h, cmd, 0)

	require.True(t, h.interactive, "second Enter must enter interactive mode on committed B")
	assert.Equal(t, instB, h.livePane.Instance(),
		"input must bind to the committed instance B, not the previously-interacted A")
	assert.Equal(t, paneB, h.livePane, "the live pane must be B's pane")

	// A forwarded keystroke must land in B's attachment, never A's stale one.
	bFake := h.liveTerm.(*fakeLiveTerm)
	require.NotSame(t, aFake, bFake, "B must have its own attachment")
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Z")})
	assert.Equal(t, []string{"Z"}, bFake.keys, "keystrokes must forward to the selected instance B")
	assert.Empty(t, aFake.keys, "the previously-interacted instance A must receive nothing")
}

// TestEnterWithFocusedPaneTargetsFocusNotSidebarSelection pins #1253: with two
// panes open, a focused pane owns Enter even if the sidebar selection points at
// another instance. Switching the sidebar selection while a pane is focused
// must not silently retarget input to the selected row.
func TestEnterWithFocusedPaneTargetsFocusNotSidebarSelection(t *testing.T) {
	h := newTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	instA := startedLocalInstance(t, "alpha")
	instB := startedLocalInstance(t, "bravo")
	h.store.AddInstance(instA)
	h.store.AddInstance(instB)
	resizeHome(h, 200, 40)
	fakes, _ := stubLiveTermFactory(t)

	// Open both panes, then focus A while the sidebar selection points at B.
	paneA := openTestPane(t, h, instA, 0)
	paneB := openTestPane(t, h, instB, 0)
	require.Equal(t, paneB, h.focusedOpenPane(), "opening B focuses B")
	h.focusRegion(layout.PaneRegion(paneA.ID()))
	require.Equal(t, paneA, h.focusedOpenPane(), "A's pane must hold the focus ring")

	h.sidebar.SetSelectedInstance(1)
	h.store.SetSelectedInstance(instB)
	h.clampSelectionTab()
	require.Equal(t, instB, h.sidebar.GetSelectedInstance())

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	runHermeticCmd(t, h, cmd, 0)

	require.True(t, h.interactive, "Enter must enter interactive mode")
	require.Equal(t, paneA, h.focusedOpenPane(), "Enter must keep focus on pane A")
	require.Equal(t, paneA, h.livePane, "input must bind to focused pane A")
	assert.Equal(t, instA, h.livePane.Instance(), "input must target A, not sidebar-selected B")
	assert.Equal(t, instB, h.sidebar.GetSelectedInstance(), "sidebar selection is unchanged")
	require.Len(t, *fakes, 1)
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Z")})
	// The entry Enter forwards into focused pane A (#1576), then Z on top —
	// both land on A's attachment, never sidebar-selected B's.
	assert.Equal(t, []string{"enter", "Z"}, (*fakes)[0].keys)
}

func TestEnterOnRemotePaneFallsBackToFullScreenAttach(t *testing.T) {
	h := newTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(
		helpTypeInteractive{}.mask()|helpTypeInstanceAttach{}.mask()))
	inst := instanceWithFakeBackend(t, "remote-inst")
	inst.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
	inst.SetStatusForTest(session.Running)
	require.True(t, inst.Capabilities().Workspace == session.WorkspaceRemote)
	selectInstance(h, inst)
	resizeHome(h, 120, 40)
	openTestPane(t, h, inst, 0)

	attached := 0
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		attached++
		assert.True(t, rem, "the remote flag must reach the attach path")
		return nil
	})

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	_ = runAttachTransitionCmd(t, h, cmd)

	assert.Equal(t, 1, attached, "a remote pane cannot embed: Enter must fall back to full-screen attach")
	assert.False(t, h.interactive)
}

func TestAttachKeyKeepsFullScreenAttach(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(
		helpTypeInteractive{}.mask()|helpTypeInstanceAttach{}.mask()))

	attached := 0
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		attached++
		return nil
	})

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")}, keys.KeyAttach)
	_ = runAttachTransitionCmd(t, h, cmd)

	assert.Equal(t, 1, attached, "`o` must run the full-screen attach flow")
	assert.False(t, h.interactive, "`o` must not enter interactive mode")
	assert.Empty(t, *fakes, "no live bind may happen on the way into a full-screen attach")
}

func TestFirstInteractiveEntryShowsHelpScreenOnce(t *testing.T) {
	h, _ := liveTestHome(t)
	_, _ = stubLiveTermFactory(t)

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)

	require.Equal(t, stateHelp, h.state, "first Enter must show the interactive help screen")
	require.NotNil(t, h.textOverlay)
	assert.Contains(t, h.textOverlay.Render(), "ctrl+]",
		"the help screen must lead with the escape hatch (RFC §5.7)")
	assert.False(t, h.interactive, "activation waits for the overlay dismissal")

	// Any key dismisses the overlay; the deferred activation then runs.
	_, cmd := h.handleHelpState(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateDefault, h.state)
	runHermeticCmd(t, h, cmd, 0)
	assert.True(t, h.interactive, "dismissing the help screen must complete the activation")
}

func TestWheelIsInertWhileInteractive(t *testing.T) {
	h, _, _ := interactiveTestHome(t)
	enterInteractive(t, h)
	p := h.focusedOpenPane()
	require.NotNil(t, p)

	_, _ = h.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})

	assert.False(t, h.paneWindows[p.ID()].IsInScrollMode(),
		"host wheel-scroll must not flip the live pane into capture scroll mode mid-typing")
}

// zeroInteractiveBindRetryDelay removes the inter-attempt sleep so the retry
// tests don't burn wall-clock. Event-loop-only var, restored on cleanup.
func zeroInteractiveBindRetryDelay(t *testing.T) {
	t.Helper()
	orig := interactiveBindRetryDelay
	interactiveBindRetryDelay = 0
	t.Cleanup(func() { interactiveBindRetryDelay = orig })
}

// TestInteractiveRetriesTransientBindThenSucceeds pins #1526: the embedded
// terminal can miss the FIRST bind when the tmux pane isn't ready yet (a
// first-render race). Entering interactive must retry that transient miss and
// succeed without ever surfacing the "couldn't open an embedded terminal" line.
func TestInteractiveRetriesTransientBindThenSucceeds(t *testing.T) {
	h, _ := liveTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	zeroInteractiveBindRetryDelay(t)

	var calls int
	orig := newLiveTermPaneFn
	newLiveTermPaneFn = func(sessionName string, width, height int) (liveTermAttachment, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("tmux pane not ready")
		}
		return newFakeLiveTerm(), nil
	}
	t.Cleanup(func() { newLiveTermPaneFn = orig })

	enterInteractive(t, h)

	assert.True(t, h.interactive, "a transient first-attempt miss must be retried into interactive mode")
	assert.GreaterOrEqual(t, calls, 2, "the failed first bind must be retried")
	assert.Empty(t, h.errBox.FullError(), "a recovered transient miss must not surface the open error")
	assert.NotNil(t, h.liveTerm, "the retried bind must leave a live attachment")
}

// TestInteractivePersistentBindFailureSurfacesError is the other half of #1526:
// once the bounded retries are exhausted, a genuine failure must still surface
// the "press o to attach full-screen" guidance and stay in nav mode.
func TestInteractivePersistentBindFailureSurfacesError(t *testing.T) {
	h, inst := liveTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	zeroInteractiveBindRetryDelay(t)

	var calls int
	orig := newLiveTermPaneFn
	newLiveTermPaneFn = func(sessionName string, width, height int) (liveTermAttachment, error) {
		calls++
		return nil, fmt.Errorf("attach failed")
	}
	t.Cleanup(func() { newLiveTermPaneFn = orig })

	// Drive the activation by hand (help is marked seen, so Enter yields the
	// deferred enterInteractiveMsg directly) and stop before running the
	// returned transient-clear timer, so the surfaced error is still in the box
	// when we assert.
	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	require.NotNil(t, cmd)
	_, _ = h.Update(cmd())

	assert.False(t, h.interactive, "a persistent bind failure must not enter interactive mode")
	assert.Equal(t, interactiveBindAttempts, calls, "all bounded attempts run before surfacing the error")
	assert.Contains(t, h.errBox.FullError(), "couldn't open an embedded terminal")
	assert.Contains(t, h.errBox.FullError(), inst.Title, "the error names the session")
	assert.Contains(t, h.errBox.FullError(), "press o", "the error keeps the full-screen fallback guidance")
}

// TestInteractiveRetryZeroSizeExitStillSetsBackoff pins the #1526 review
// finding: a bind retry that exits at the pre-bind zero-size guard must still
// record the passive 5s backoff. Otherwise, after surfacing the interactive
// error, the preview tick would re-attempt the same unavailable pane every
// 100ms with no backoff (a busy loop that defeats the backoff).
func TestInteractiveRetryZeroSizeExitStillSetsBackoff(t *testing.T) {
	h, _ := liveTestHome(t)
	zeroInteractiveBindRetryDelay(t)

	var calls int
	orig := newLiveTermPaneFn
	newLiveTermPaneFn = func(string, int, int) (liveTermAttachment, error) {
		calls++
		return newFakeLiveTerm(), nil
	}
	t.Cleanup(func() { newLiveTermPaneFn = orig })

	p := h.focusedOpenPane()
	require.NotNil(t, p)
	// Force every bind attempt to exit at the pre-bind zero-size guard.
	h.paneWindows[p.ID()].SetRect(layout.Rect{})

	require.False(t, h.bindLiveTermPaneWithRetry(p), "a zero-size pane cannot bind")
	require.Zero(t, calls, "the zero-size guard exits before spawning an attachment")
	require.False(t, h.liveBindFailedAt.IsZero(),
		"a retry that exits at the zero-size guard must record a failure time for the passive backoff")

	// The pane becomes laid out again, but the passive tick must honor the 5s
	// backoff the failed retry set — no immediate re-attempt every tick.
	resizeHome(h, 120, 40)
	h.reconcileLiveTermPane()
	assert.Nil(t, h.liveTerm, "the passive backoff must hold: no immediate re-attempt after a zero-size retry")
	assert.Zero(t, calls, "no attachment spawned while the backoff is active")
}
