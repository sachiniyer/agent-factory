package app

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// ----------------------------------------------------------------------------
// #7015: keys that land in one terminal read are still separate keys.
//
// Bubble Tea delivers printable bytes that arrive together as ONE KeyRunes
// message ("kk"), and the nav key map resolves keys by exact String(), so a
// burst of bound keys matched nothing. Since #4722 Option 3, an unmatched
// printable key on a focused pane is an implicit switch into keyboard mode that
// types itself. So a user who pressed `k` twice to move the cursor, quickly
// or on a busy box, could have both presses typed into the agent. The TUI
// driver selftest hit exactly that: a plain rail move landed on an already-open
// tab, #1493 handed its pane nav focus, and the next burst of `k` switched to
// keyboard mode and typed every remaining driver key into the shell.
//
// The property: in nav mode, a key sequence behaves the same however the
// terminal chunked it into reads. keys/bubbletea_input_test.go pins the parser
// shapes these tests feed in.
// ----------------------------------------------------------------------------

// pressRunes delivers one KeyRunes message carrying every rune of s, which is
// what Bubble Tea produces when those keys arrive in a single read. Cmds are not
// drained: selectionChanged's refresh dispatches are not hermetic, and every
// effect asserted below happens synchronously inside Update.
func pressRunes(h *home, s string) {
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

// typedBytes is everything forwarded into any pane's PTY, across every live
// attachment the test created, in order.
func typedBytes(fakes []*fakeLiveTerm) string {
	var b strings.Builder
	for _, f := range fakes {
		b.WriteString(strings.Join(f.keys, ""))
	}
	return b.String()
}

// focusKind names the focused region independently of pane ids, so the
// focus that results from two separately built fixtures can be compared.
func focusKind(h *home) string {
	if p := h.focusedOpenPane(); p != nil {
		return fmt.Sprintf("pane on tab %d", p.Tab())
	}
	return h.ring.Active()
}

// burstOnLandedPaneHome builds the shape the TUI driver selftest failed in
// (#7015): one live session with two open panes, the agent pane the preview
// owner, and the tree cursor moved by plain `j` presses onto the shell tab.
// That tab is already open in a pane, so the #1493 open-or-focus contract gives
// the pane nav focus. A rail move is all that put focus there; no Enter, click,
// or focus key was pressed.
func burstOnLandedPaneHome(t *testing.T) (*home, *[]*fakeLiveTerm) {
	t.Helper()
	h, inst, fakes := interactiveTestHome(t)
	agentPane := h.focusedOpenPane()
	require.NotNil(t, agentPane)
	shellPane := openTestPane(t, h, inst, 1)
	h.focusRegion(layout.PaneRegion(agentPane.ID())) // the preview owner
	h.focusRegion(layout.RegionTree)

	pressRunes(h, "j") // the agent tab row: the owner's own binding, so the tree keeps focus
	pressRunes(h, "j") // the shell tab row: already open, so its pane takes nav focus (#1493)
	require.Equal(t, layout.PaneRegion(shellPane.ID()), h.ring.Active(),
		"precondition: a plain rail move onto an open tab gives that tab's pane nav focus (#1493)")
	require.False(t, h.interactive, "precondition: nav focus only, not keyboard mode")
	return h, fakes
}

// TestRuneBurstOfNavKeysOnLandedPaneNeverTypes is the #7015 regression. On the
// pane a rail move landed on, two `k` presses that arrived in one read must do
// what two separate `k` presses do: move the cursor. They must never switch
// into keyboard mode or reach the agent.
func TestRuneBurstOfNavKeysOnLandedPaneNeverTypes(t *testing.T) {
	// Control: what pressing k, then k, does from this exact state.
	want, wantFakes := burstOnLandedPaneHome(t)
	pressRunes(want, "k")
	pressRunes(want, "k")
	require.False(t, want.interactive, "control: separate k presses are rail moves")
	require.Empty(t, typedBytes(*wantFakes), "control: separate k presses type nothing")
	wantSel, wantFocus := want.sidebar.GetSelection(), focusKind(want)

	h, fakes := burstOnLandedPaneHome(t)
	pressRunes(h, "kk") // both presses landed in one terminal read

	assert.False(t, h.interactive,
		"a burst of the rail's own up key must never switch into keyboard mode (#7015)")
	assert.Empty(t, typedBytes(*fakes),
		"a burst of the rail's own up key must never be typed into the agent (#7015)")
	assert.Equal(t, wantSel, h.sidebar.GetSelection(),
		"the burst must move the cursor exactly as two separate k presses do")
	assert.Equal(t, wantFocus, focusKind(h),
		"the burst must leave focus where two separate k presses leave it")
}

// TestRuneBurstKeepsEachBoundKeysNavMeaning pins #4722 Option 3's
// no-stolen-shortcuts rule for bursts: a bound key keeps its nav action even when
// it shares a read with another key. `x` still hides the focused pane, and the
// `k` after it still moves the rail. Nothing switches modes or is typed.
func TestRuneBurstKeepsEachBoundKeysNavMeaning(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	require.NotNil(t, h.focusedOpenPane())

	pressRunes(h, "xk")

	assert.False(t, h.interactive, "x and k are nav keys, not pane input")
	assert.Zero(t, h.store.NumOpenPanes(), "the x in the burst must hide the focused pane")
	assert.Empty(t, typedBytes(*fakes), "nothing may be typed into the pane")
}

// TestRuneBurstOfUnboundLettersStillSwitchesAndTypes pins the half of #4722
// Option 3 the split must keep: on a focused pane, an unbound letter switches
// into keyboard mode and types itself. Once the mode owns the keyboard, the rest
// of the burst is pane input. So the `x` after `z` is typed, not run as
// hide-pane, exactly as when the two keys arrive separately
// (TestLetterSwitchOwnsTheNextKeypress).
func TestRuneBurstOfUnboundLettersStillSwitchesAndTypes(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)

	pressRunes(h, "zx")

	require.True(t, h.interactive, "an unbound letter on a focused pane still switches into keyboard mode")
	assert.Equal(t, 1, h.store.NumOpenPanes(), "x after the switch is pane input, not the hide-pane verb")
	assert.Equal(t, "zx", typedBytes(*fakes), "the whole burst is typed, in order, once the mode owns the keyboard")
}

// TestAltRuneBurstIsOneKey pins the Alt edge of the split. Bubble Tea attaches
// Alt to exactly one rune (keys/bubbletea_input_test.go), so a multi-rune Alt
// message is not a parser burst. It stays one key: it is not spread into Alt
// presses of each rune, and it does not lose its Alt to become plain `k`
// presses that would move the rail. An Alt key is never typing, so it stays
// inert on the focused pane, as it was before the split existed.
func TestAltRuneBurstIsOneKey(t *testing.T) {
	h, _, fakes := interactiveTestHome(t)
	p := h.focusedOpenPane()
	require.NotNil(t, p)

	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("kk"), Alt: true})

	assert.False(t, h.interactive, "an Alt-modified key is not a typing gesture")
	assert.Empty(t, typedBytes(*fakes), "an Alt-modified key must not be typed into the pane")
	assert.Equal(t, layout.PaneRegion(p.ID()), h.ring.Active(),
		"the Alt key must not have been split into plain k presses, which would refocus the tree")
}

// TestRuneBurstOnTreeMovesTheCursorPerKey covers the older half of #7015's root
// cause. On the tree, a burst used to match no binding and was silently dropped,
// so key presses that queued behind a slow frame were lost. Now `jj` moves the
// cursor exactly as `j`, `j` does.
func TestRuneBurstOnTreeMovesTheCursorPerKey(t *testing.T) {
	want := paneTestHome(t)
	want.focusRegion(layout.RegionTree)
	before := want.sidebar.GetSelection()
	pressRunes(want, "j")
	pressRunes(want, "j")
	wantSel := want.sidebar.GetSelection()
	require.NotEqual(t, before, wantSel, "control: two j presses move the cursor")

	h := paneTestHome(t)
	h.focusRegion(layout.RegionTree)
	require.Equal(t, before, h.sidebar.GetSelection(), "both fixtures start on the same row")

	pressRunes(h, "jj")

	assert.Equal(t, wantSel, h.sidebar.GetSelection(),
		"a burst of j must move the cursor as far as separate j presses do, not be dropped")
	assert.Equal(t, layout.RegionTree, h.ring.Active())
}

// TestRuneBurstAcrossTheFirstRunHelpReachesThePane is Codex's first finding on
// #7020. With the first-run help unseen, "zxx" on a focused pane works like
// this: `z` opens the help, and the next key dismisses it, but the switch into
// keyboard mode lands later, as a deferred message. A split that kept running
// each key through nav dispatch ran the second `x` as hide-pane in that window,
// closing the pane the keys were meant for. Once a key leaves nav dispatch, the
// rest of the burst goes over whole: it dismisses the help and is typed.
func TestRuneBurstAcrossTheFirstRunHelpReachesThePane(t *testing.T) {
	h, _ := liveTestHome(t) // first-run interactive help NOT seen
	fakes, _ := stubLiveTermFactory(t)
	require.Equal(t, 1, h.store.NumOpenPanes())

	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zxx")})
	runHermeticCmd(t, h, cmd, 0)

	require.True(t, h.interactive, "dismissing the help completes the switch into keyboard mode")
	assert.Equal(t, 1, h.store.NumOpenPanes(), "no x after the help may run as the hide-pane verb")
	assert.Equal(t, "xx", typedBytes(*fakes),
		"z only surfaced the help; the keys after it dismiss the help and are typed into the pane")
}

// TestRuneBurstDisarmsTheQuitDiscardBetweenKeys is Codex's second finding on
// #7020. A refused quit (a save failed) arms a discard confirmation that any
// input other than another quit disarms (#5898). Separate `j` and `q` presses
// therefore refuse the quit again and keep the edit. A "jq" burst must do the
// same: each split key is its own input event, so the `j` disarms the
// confirmation before the `q` arrives.
func TestRuneBurstDisarmsTheQuitDiscardBetweenKeys(t *testing.T) {
	h := dirtyHooksHome(t)
	stubHooksSaveFailure(t)
	h.state = stateDefault // nav mode, with the hooks edit still unsaved in memory

	pressRunes(h, "q")
	require.False(t, h.quitting, "precondition: the first quit is refused")
	require.NotEmpty(t, h.quitSaveFailure, "precondition: the refusal arms the discard confirmation")

	pressRunes(h, "jq") // j, then q, in one terminal read

	assert.False(t, h.quitting, "the j between the quits disarms the discard, as it does when typed separately")
	assert.True(t, h.hooksPane.IsDirty(), "the unsaved hooks edit must be kept")
}
