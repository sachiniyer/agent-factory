package app

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
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
	if lt := focusedFake(h); lt != nil {
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
	assert.Equal(t, p, h.focusedOpenPane())
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

// TestInteractiveEndsWhenFocusLeavesPane pins the invariant that interactive mode
// cannot outlive its premise (the focused pane owns the attachment). Unlike the
// old tmux client there is no "client died" event — a WS drop self-heals — so the
// premise breaks instead when focus leaves the pane. The very next keystroke must
// detect the breakage, drop to nav, and be swallowed rather than mistyped.
func TestInteractiveEndsWhenFocusLeavesPane(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	enterInteractive(t, h)
	fake := (*fakes)[0]

	// Focus moves off the pane (to the tree). The tick hasn't run yet, so the
	// next key funnels through enforceInteractiveInvariant, which drops the mode.
	h.focusRegion("tree")
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})

	assert.False(t, h.interactive, "focus leaving the interactive pane must drop back to nav mode")
	assert.Empty(t, fake.keys, "the racing keystroke must be swallowed, not forwarded off-pane")
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
	require.Equal(t, pane, h.focusedOpenPane())
	require.Len(t, *fakes, 1)
	require.Equal(t, inst.Title, (*sessions)[0])

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
	require.Equal(t, instA, h.focusedOpenPane().Instance(), "first Enter binds A")
	aFake := focusedFake(h)

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
	assert.Equal(t, instB, h.focusedOpenPane().Instance(),
		"input must bind to the committed instance B, not the previously-interacted A")
	assert.Equal(t, paneB, h.focusedOpenPane(), "the live pane must be B's pane")

	// A forwarded keystroke must land in B's attachment, never A's stale one.
	bFake := focusedFake(h)
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
	require.Equal(t, paneA, h.focusedOpenPane(), "input must bind to focused pane A")
	assert.Equal(t, instA, h.focusedOpenPane().Instance(), "input must target A, not sidebar-selected B")
	assert.Equal(t, instB, h.sidebar.GetSelectedInstance(), "sidebar selection is unchanged")
	// Both visible panes bind their own stream (per-pane, #1592 PR6), but only the
	// FOCUSED pane routes keystrokes.
	require.Len(t, *fakes, 2)
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Z")})
	// The entry Enter forwards into focused pane A (#1576), then Z on top — both
	// land on A's attachment, never sidebar-selected B's.
	assert.Equal(t, []string{"enter", "Z"}, focusedFake(h).keys,
		"keystrokes route to the focused pane's attachment")
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
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, _ func() (chan struct{}, error)) tea.Cmd {
		attached++
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
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, _ func() (chan struct{}, error)) tea.Cmd {
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
	for _, size := range []struct {
		name   string
		width  int
		height int
	}{
		{name: "80x24", width: 80, height: 24},
		{name: "72x20", width: 72, height: 20},
	} {
		t.Run(size.name, func(t *testing.T) {
			h, _ := liveTestHome(t)
			_, _ = stubLiveTermFactory(t)
			resizeHome(h, size.width, size.height)

			_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)

			require.Equal(t, stateHelp, h.state, "first Enter must show the interactive help screen")
			require.NotNil(t, h.textOverlay)
			view := h.View()
			requireViewSized(t, view, size.width, size.height)
			copy := flatten(view)
			assert.Contains(t, copy, "You are typing into this pane's",
				"the help screen follows the TUI sentence-case convention")
			assert.Contains(t, copy, "terminal: every key",
				"the wrapped sentence remains visible at compact widths")
			assert.NotContains(t, copy, "typing INTO", "the help screen must not caps-shout")
			assert.Contains(t, copy, "ctrl+]",
				"the help screen must lead with the escape hatch (RFC §5.7)")
			assert.False(t, h.interactive, "activation waits for the overlay dismissal")

			// Any key dismisses the overlay; the deferred activation then runs.
			_, cmd := h.handleHelpState(tea.KeyMsg{Type: tea.KeyEnter})
			require.Equal(t, stateDefault, h.state)
			runHermeticCmd(t, h, cmd, 0)
			assert.True(t, h.interactive, "dismissing the help screen must complete the activation")
		})
	}
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

// Note: the old #1526 transient-bind-retry tests are gone. The WS attachment is
// created immediately and self-heals via reconnect+replay, so there is no
// first-render bind race to retry — the whole "couldn't open an embedded terminal,
// try again" class is structurally eliminated. The genuine non-embeddable case
// (remote/dead panes) still falls back to full-screen attach; that is covered by
// TestEnterOnRemotePaneFallsBackToFullScreenAttach. What #1526 did NOT cover —
// activation reached while a reconcile GATE was closed — is pinned below (#1819).

// ----------------------------------------------------------------------------
// #1819: activation binds the pane itself. reconcileLiveTermPanes deliberately
// skips panes whose CONTEXT says "no live grid right now" (an open overlay, an
// active #1321 preview). Entering a pane is the act that ends those states, so
// activation must resolve them and bind — never drop an ordinary local, ready,
// visible pane to the `o` fallback.
// ----------------------------------------------------------------------------

// previewTestHome is interactiveTestHome plus a second started session that the
// tree cursor has moved onto, leaving the focused pane rendering a transient
// #1321 preview of it — the state selectionChanged produces on tree navigation,
// which also CLOSES the previewing pane's live attachment.
func previewTestHome(t *testing.T) (h *home, focused, previewed *session.Instance, pane *store.OpenPane) {
	t.Helper()
	h, focused, fakes := interactiveTestHome(t)
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1, "baseline: the focused local ready pane binds")
	pane = h.focusedOpenPane()
	require.NotNil(t, pane)

	previewed = startedLocalInstance(t, "previewed")
	selectInstance(h, previewed)
	h.updatePanePreview(previewed, 0, true, false)
	require.True(t, h.paneIsPreviewing(pane), "tree nav leaves the focused pane previewing")
	require.Nil(t, h.liveTerms[pane.ID()], "the preview closed the pane's live attachment")
	return h, focused, previewed, pane
}

// TestClickToInteractOnPreviewingPaneBindsInsteadOfFallingBack is the #1819
// regression. Clicking the focused pane's body enters it (§2.5) — but when tree
// navigation had left that pane previewing another session, the click reached
// activation with the preview txn still live. reconcileLiveTermPanes refuses to
// bind a previewing pane, so activation found liveTerms[p] == nil and errored
// "couldn't open an embedded terminal … press o", for a perfectly ordinary local,
// Ready, visible pane. Keyboard Enter never hit this because handleEnter commits
// the preview first; the mouse path had no such funnel.
func TestClickToInteractOnPreviewingPaneBindsInsteadOfFallingBack(t *testing.T) {
	h, _, previewed, pane := previewTestHome(t)

	body := zoneRect(t, h, zones.PaneBody(layout.PaneRegion(pane.ID())))
	require.Equal(t, layout.PaneRegion(pane.ID()), h.ring.Active(),
		"the pane is already focused, so a body click enters it rather than focusing it")
	runHermeticCmd(t, h, combineCmds(press(h, body.X+3, body.Y+4), release(h, body.X+3, body.Y+4)), 0)

	require.True(t, h.interactive, "clicking an eligible local pane must enter it, not fall back to `o`")
	p := h.focusedOpenPane()
	require.NotNil(t, p)
	require.NotNil(t, h.liveTerms[p.ID()], "activation must install the live attachment")
	// The click commits the preview, exactly like keyboard Enter: the user acts on
	// the content they can SEE, so keystrokes go to the previewed session — never
	// to the session the pane used to show.
	assert.Equal(t, previewed, p.Instance(), "the click enters the previewed target")
	assert.Nil(t, h.panePreviewTxn, "entering a pane resolves its preview")
}

// TestActivateInteractiveDropsStaleActivationUnderAttachHelp is the #598 fence on
// the activation path (codex review of #1819). Enter queues an enterInteractiveMsg;
// if the user presses `o` before it is delivered, showHelpScreen(helpTypeInstanceAttach)
// closes the live panes to make room for the full-screen attach it runs on dismiss —
// but attachTransitioning stays false until then. That is the ONE window where an
// attach is pending yet neither attach flag is set, so an activation that bound here
// would hand the coming attach a live embedded client to fight over the session size.
// The stale activation must be dropped: no bind, no interactive mode, and no spurious
// error toast under the overlay either.
func TestActivateInteractiveDropsStaleActivationUnderAttachHelp(t *testing.T) {
	h, inst, fakes := interactiveTestHome(t)
	h.syncLiveTermPane()
	p := h.focusedOpenPane()
	require.NotNil(t, p)
	require.Len(t, *fakes, 1, "baseline: the pane is bound before the attach help opens")

	// `o` on a first-time attacher: the help closes the live panes for the deferred
	// attach, and attachTransitioning is still false until dismiss.
	_, _ = h.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd { return nil })
	require.Equal(t, stateHelp, h.state)
	require.False(t, h.attachTransitioning, "the attach is deferred to dismiss, not armed yet")
	require.False(t, h.attached.Load())
	require.Nil(t, h.liveTerms[p.ID()], "the attach help closed the embedded attachment (#598)")

	// The Enter queued before `o` now lands.
	require.Nil(t, h.activateInteractive(p), "a stale activation must be dropped silently")

	assert.Nil(t, h.liveTerms[p.ID()], "must not resurrect the embedded client the attach help closed")
	assert.False(t, h.interactive, "interactive mode under an overlay is incoherent — the overlay owns the keyboard")
	assert.Len(t, *fakes, 1, "no second attachment was created for %s", inst.Title)
}

// TestEnterPaneIgnoredDuringAttachTransition pins the mouse half of the #1530
// fence (codex review of #1819). handleEnter bails on attachTransitioning at the
// key handler, but a click routes straight to enterPane — so during the ~20ms
// beginAttachTransition window (stateDefault, attachTransitioning true) a click
// would reach the preview commit and REBIND the pane, even though the activation
// that follows is refused. The user would detach to find the pane showing a
// different session than the one they attached from.
func TestEnterPaneIgnoredDuringAttachTransition(t *testing.T) {
	h, focused, _, pane := previewTestHome(t)

	h.attachTransitioning = true // the 20ms beginAttachTransition window

	_, cmd := h.enterPane(pane, nil)
	runHermeticCmd(t, h, cmd, 0)

	assert.False(t, h.interactive, "the attach owns the screen: no interactive entry")
	assert.NotNil(t, h.panePreviewTxn, "the preview must not be committed mid-attach-transition")
	assert.Equal(t, focused, pane.Instance(), "the pane must not be rebound out from under the attach")
}

// TestActivateInteractiveFallsBackWhileFullScreenAttached pins the fallback that
// must SURVIVE #1819: a full-screen attach owns the session's tmux client, so a
// second embedded stream would fight it over the window size (#598). That pane is
// genuinely non-embeddable and must still refuse to bind.
func TestActivateInteractiveFallsBackWhileFullScreenAttached(t *testing.T) {
	h, _, _ := interactiveTestHome(t)
	p := h.focusedOpenPane()
	require.NotNil(t, p)

	h.attached.Store(true)

	require.NotNil(t, h.activateInteractive(p), "an attached session must not also bind an embedded stream")
	assert.False(t, h.interactive)
	assert.Nil(t, h.liveTerms[p.ID()])
}

// TestEnteredPaneRendersTypedInput covers the user-reported presentation of this
// class: "input is accepted but the display stays stale". A pane is only healthy
// when the SAME attachment both takes the keystrokes and backs what is rendered —
// asserting "no error on entry" would pass even if the window rendered a stale
// capture while SendKey vanished into an attachment nothing draws. So enter a pane
// whose attachment was MISSING at activation (the #1819 setup) and require the
// typed text to come back out of the pane's rendered frame.
func TestEnteredPaneRendersTypedInput(t *testing.T) {
	h, _, previewed, pane := previewTestHome(t)

	body := zoneRect(t, h, zones.PaneBody(layout.PaneRegion(pane.ID())))
	runHermeticCmd(t, h, combineCmds(press(h, body.X+3, body.Y+4), release(h, body.X+3, body.Y+4)), 0)
	require.True(t, h.interactive, "the click must enter the pane")

	p := h.focusedOpenPane()
	require.NotNil(t, p)
	require.Equal(t, previewed, p.Instance())

	for _, r := range "hello" {
		_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	// Input accepted...
	fake := focusedFake(h)
	require.NotNil(t, fake, "the entered pane must own the attachment keystrokes route to")
	assert.Equal(t, []string{"h", "e", "l", "l", "o"}, fake.keys, "keystrokes must reach the pane")
	// ...AND the display follows it. Both must hold: the window renders through the
	// very attachment that took the input, so what was typed is on screen.
	assert.True(t, h.paneWindows[p.ID()].HasLive(), "the window must render through the attachment, not a capture")
	assert.Contains(t, h.View(), "hello", "the pane must render the typed input, not a stale frame")
}

// TestClickCommittingWebTabPreviewShowsGuardNotAttach pins the web-tab guard on
// the pane path (codex review of #1819). A web tab has no PTY, so it can neither
// embed nor attach — the tree Enter path runs webTabAttachGuard before dispatching.
// The pane path did not, so committing a previewed web tab rebound the pane and then
// fell through to liveSessionName == "" → a full-screen attach against a tab the
// daemon cannot stream. It must surface the "view it in the web UI" message instead.
func TestClickCommittingWebTabPreviewShowsGuardNotAttach(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(
		helpTypeInteractive{}.mask()|helpTypeInstanceAttach{}.mask()))
	h.syncLiveTermPane()
	require.Len(t, *fakes, 1)
	pane := h.focusedOpenPane()
	require.NotNil(t, pane)

	// A second session whose tab 1 is a web tab, previewed onto the focused pane.
	webInst := startedLocalInstance(t, "web-host")
	webTab, err := webInst.AddWebTab("http://localhost:3000", "")
	require.NoError(t, err)
	require.Equal(t, session.TabKindWeb, webTab.Kind)
	webIdx := len(webInst.GetTabs()) - 1
	selectInstance(h, webInst)
	h.updatePanePreview(webInst, webIdx, true, false)
	require.True(t, h.paneIsPreviewing(pane), "the pane previews the web tab")

	_, _ = h.enterPane(pane, nil)

	// Every signal here is written SYNCHRONOUSLY by enterPane, so the test asserts
	// without draining cmds. That is deliberate: the returned batch carries
	// handleError's 3s transient-clear timer, and running it would deliver
	// hideErrMsg and wipe the very notice under test — the notice would then
	// survive or not depending on which cmd won, which is exactly the flake this
	// test first shipped with (green locally, red in CI). beginAttachTransition
	// likewise arms attachTransitioning synchronously, so a wrongly-dispatched
	// attach is visible here with no tick to pump.
	assert.Contains(t, h.errBox.FullError(), "web tab",
		"the user must get the 'view it in the web UI' message")
	assert.False(t, h.attachTransitioning,
		"a web tab has no PTY: it must never start a full-screen attach")
	assert.False(t, h.interactive, "a web tab cannot embed either")
}

// TestAttachFocusedBrowserOnlyPaneShowsGuardNotAttach pins webTabAttachGuard on
// the FOCUSED-PANE attach path (handleEnterPane, #1817) — the one path into
// attachInstanceTab that nothing else fences.
//
// The other two callers guard on the SELECTED tab before dispatching, and
// enterPane runs its own guard (TestClickCommittingWebTabPreviewShowsGuardNotAttach
// covers that one). But `o` on a focused pane routes handleAttach → handleEnterPane
// directly, and THAT pane's tab — not the tree selection — is what gets attached.
// So the guard inside handleEnterPane is the only thing between the user and a WS
// PTY dial for a browser-only tab that has no PTY to stream; delete it and the
// low-level attach failure surfaces instead of the message saying where to
// actually view the tab. Both browser-only kinds run the same path, so both are
// pinned here.
//
// Two fixture details are load-bearing for this test's ability to FAIL:
//   - Marking helpTypeInstanceAttach seen. Otherwise attachInstanceTab parks on
//     the first-time help overlay and attachTransitioning stays false — the test
//     would pass with the guard removed, for the wrong reason.
//   - Opening the browser-only tab as the focused pane, so focusedOpenPane wins
//     the handleAttach branch and the selection path is never consulted.
func TestAttachFocusedBrowserOnlyPaneShowsGuardNotAttach(t *testing.T) {
	for _, tc := range []struct {
		name    string
		addTab  func(t *testing.T, inst *session.Instance) *session.Tab
		kind    session.TabKind
		wantErr string
	}{
		{
			name: "vscode",
			addTab: func(t *testing.T, inst *session.Instance) *session.Tab {
				tab, err := inst.AddVSCodeTab("")
				require.NoError(t, err)
				return tab
			},
			kind:    session.TabKindVSCode,
			wantErr: "VS Code tab",
		},
		{
			name: "web",
			addTab: func(t *testing.T, inst *session.Instance) *session.Tab {
				tab, err := inst.AddWebTab("http://localhost:3000", "")
				require.NoError(t, err)
				return tab
			},
			kind:    session.TabKindWeb,
			wantErr: "web tab",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, inst, _ := interactiveTestHome(t)
			require.NoError(t, h.appState.SetHelpScreensSeen(
				helpTypeInteractive{}.mask()|helpTypeInstanceAttach{}.mask()))
			// Safety net on a shared box: a full-screen attach that escapes the guard
			// must never dial the real daemon socket. The 20ms beginAttachTransition
			// tick is not pumped below, so this should stay uncalled either way.
			t.Cleanup(SetAttachStreamFnForTest(
				func(context.Context, string, string, string, int) (chan struct{}, error) {
					t.Errorf("a browser-only tab must never dial a WS PTY stream")
					return nil, fmt.Errorf("unexpected dial")
				}))

			tab := tc.addTab(t, inst)
			require.Equal(t, tc.kind, tab.Kind)
			tabIdx := len(inst.GetTabs()) - 1

			// The browser-only tab IS the focused pane — the handleAttach fast path.
			p := openTestPane(t, h, inst, tabIdx)
			require.Same(t, p, h.focusedOpenPane(), "the browser-only tab must own focus")
			require.Equal(t, tabIdx, p.Tab())

			_, _ = h.handleAttach() // `o`

			// Asserted synchronously, without draining cmds — same discipline as
			// TestClickCommittingWebTabPreviewShowsGuardNotAttach: the returned batch
			// carries handleError's 3s transient-clear timer, and running it would
			// deliver hideErrMsg and wipe the very notice under test.
			// beginAttachTransition arms attachTransitioning synchronously, so a
			// wrongly-dispatched attach is visible here with no tick to pump.
			assert.Contains(t, h.errBox.FullError(), tc.wantErr,
				"the user must be pointed at the web UI, not handed a PTY-attach failure")
			assert.False(t, h.attachTransitioning,
				"a %s tab has no PTY: `o` must never start a full-screen attach", tc.name)
			assert.False(t, h.attached.Load())
			assert.False(t, h.interactive, "a browser-only tab cannot embed either")
		})
	}
}

// TestPaneErrorLabelNamesSessionOrFallsBack covers the cosmetic half of #1819:
// the fallback error logged the name as an empty pair of quotes whenever the
// title was unknown, which read as a formatting bug rather than a message.
func TestPaneErrorLabelNamesSessionOrFallsBack(t *testing.T) {
	h, inst := liveTestHome(t)
	p := h.focusedOpenPane()
	require.NotNil(t, p)
	assert.Equal(t, "'"+inst.Title+"'", paneErrorLabel(p), "a titled session is named in quotes")
	assert.Equal(t, "this pane", paneErrorLabel(nil), "an unknown pane never renders empty quotes")
}
