package app

import (
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
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
// activation to completion.
func enterInteractive(t *testing.T, h *home) {
	t.Helper()
	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	runHermeticCmd(t, h, cmd, 0)
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
		{Type: tea.KeyEnter},                     //
		{Type: tea.KeyCtrlW},                     // the full-screen detach key — not host-reserved here
	} {
		_, cmd := h.handleKeyPress(msg)
		assert.Nil(t, cmd, "forwarded keys must not trigger host actions (%s)", msg.String())
	}

	assert.Equal(t, []string{"tab", "q", "1", "ctrl+c", "x", "enter", "ctrl+w"}, fake.keys,
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

func TestEnterOnRemotePaneFallsBackToFullScreenAttach(t *testing.T) {
	h := newTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(
		helpTypeInteractive{}.mask()|helpTypeInstanceAttach{}.mask()))
	inst := instanceWithFakeBackend(t, "remote-inst")
	inst.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
	inst.SetStatus(session.Running)
	require.True(t, inst.IsRemote())
	selectInstance(h, inst)
	resizeHome(h, 120, 40)
	openTestPane(t, h, inst, 0)

	attached := 0
	swapAttachOverlayCallbackFn(t, func(m *home, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		attached++
		assert.True(t, rem, "the remote flag must reach the attach path")
		return nil
	})

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)

	assert.Equal(t, 1, attached, "a remote pane cannot embed: Enter must fall back to full-screen attach")
	assert.False(t, h.interactive)
}

func TestAttachKeyKeepsFullScreenAttach(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(
		helpTypeInteractive{}.mask()|helpTypeInstanceAttach{}.mask()))

	attached := 0
	swapAttachOverlayCallbackFn(t, func(m *home, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		attached++
		return nil
	})

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")}, keys.KeyAttach)

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
