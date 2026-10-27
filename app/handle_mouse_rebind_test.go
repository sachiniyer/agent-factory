package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/stretchr/testify/require"

	tea "github.com/charmbracelet/bubbletea"
)

// restoreDefaultKeymap is the [keys] override cleanup the app-package rebind
// tests share with the keys/ui ones: ApplyOverrides mutates package globals, so
// every test that rebinds restores the defaults before the next test runs.
func restoreDefaultKeymap(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := keys.ApplyOverrides(nil); err != nil {
			t.Fatalf("restoring default keymap: %v", err)
		}
	})
}

// TestKeyMsgFromStringHonorsRebinds reproduces the report: a [keys] rebind to a
// multi-character key the click dispatcher's closed switch never enumerated
// (ctrl+b) IS in the override-aware GlobalKeyStringsMap the keyboard path walks
// (so pressing it works), and the click path now resolves it too — instead of
// silently swallowing the click as it did before the fix. The synthetic event's
// String() is the rebound key, so handleKeyPress re-derives the same action
// from GlobalKeyStringsMap the keyboard press does.
func TestKeyMsgFromStringHonorsRebinds(t *testing.T) {
	restoreDefaultKeymap(t)
	require.NoError(t, keys.ApplyOverrides(map[string][]string{
		"scroll_up":   {"ctrl+b"},
		"scroll_down": {"ctrl+f"},
	}))

	// Keyboard path: the override-aware map resolves ctrl+b → KeyShiftUp.
	name, ok := keys.GlobalKeyStringsMap["ctrl+b"]
	require.True(t, ok, "the rebind must be in the keyboard dispatch map")
	require.Equal(t, keys.KeyShiftUp, name)

	// Click path: keyMsgFromString now translates ctrl+b instead of returning
	// false (the silent no-op), and synthesizes the same event the key would.
	msg, ok := keyMsgFromString("ctrl+b")
	require.True(t, ok,
		`rebound primary "ctrl+b" must synthesize a tea.KeyMsg (clicks would be swallowed)`)
	require.Equal(t, tea.KeyCtrlB, msg.Type, "the click presses ctrl+b, the key the chip shows")
	require.Equal(t, "ctrl+b", msg.String(),
		"the synthetic event round-trips to the rebound key")

	_, ok = keyMsgFromString("ctrl+f")
	require.True(t, ok, `rebound primary "ctrl+f" must synthesize a tea.KeyMsg`)
}

// TestKeyMsgFromStringStillRecognizesUnboundSpecials pins the fallback switch:
// hints a status bar can advertise WITHOUT being in GlobalKeyStringsMap — the
// interactive-mode exit ctrl+], the naming form's ctrl+r/ctrl+o, and the
// diff-only shift+up/shift+down — still synthesize their KeyMsg after the
// override-aware branch was layered on top. Each result is identical whether
// it resolves through the branch (if rebound) or the switch, so the behavior is
// stable for both default and rebound tables.
func TestKeyMsgFromStringStillRecognizesUnboundSpecials(t *testing.T) {
	cases := map[string]tea.KeyType{
		"ctrl+]":     tea.KeyCtrlCloseBracket,
		"ctrl+r":     tea.KeyCtrlR,
		"ctrl+o":     tea.KeyCtrlO,
		"shift+up":   tea.KeyShiftUp,
		"shift+down": tea.KeyShiftDown,
		"enter":      tea.KeyEnter,
		"esc":        tea.KeyEsc,
	}
	for s, want := range cases {
		msg, ok := keyMsgFromString(s)
		require.True(t, ok, "%q must still synthesize a KeyMsg", s)
		require.Equal(t, want, msg.Type, "%q Type", s)
	}
	// A single-rune binding (the common case) still works via the rune fallback.
	msg, ok := keyMsgFromString("q")
	require.True(t, ok, `single-rune primary "q" must synthesize a KeyMsg`)
	require.Equal(t, tea.KeyRunes, msg.Type)
	require.Equal(t, "q", msg.String())
}

// TestMouse_HintClickMatchesKeyGatesAfterRebind runs the team's existing
// invariant — every advertised primary key must synthesize a tea.KeyMsg, so a
// rendered hint is always clickable — after a [keys] rebind. Before the fix
// this went red the moment a rebind was in play: every rebound multi-character
// key the dispatcher's closed switch never enumerated landed in the !ok branch
// and the click was swallowed. It also asserts the synthetic event round-trips
// to the primary, so handleKeyPress re-derives the same action the keyboard
// press would.
func TestMouse_HintClickMatchesKeyGatesAfterRebind(t *testing.T) {
	restoreDefaultKeymap(t)
	// Rebind several actions to multi-character targets the old switch never
	// enumerated: ctrl+<letter>, function keys, named keys, and an alt combo.
	require.NoError(t, keys.ApplyOverrides(map[string][]string{
		"scroll_up":   {"ctrl+b"},
		"scroll_down": {"ctrl+f"},
		"up":          {"f1"},
		"down":        {"pgup"},
		"help":        {"alt+?"},
	}))
	for name, binding := range keys.GlobalKeyBindings {
		if name == keys.KeyNewRemote {
			require.Empty(t, binding.Keys(), "retired new_remote must not advertise a default hint")
			continue
		}
		require.NotEmpty(t, binding.Keys(), "advertised binding %v must have a primary key", name)
		primary := binding.Keys()[0]
		if primary == "1" { // KeyJumpTab renders no zone by design
			continue
		}
		msg, ok := keyMsgFromString(primary)
		require.True(t, ok, "primary key %q must synthesize a tea.KeyMsg after rebinding", primary)
		require.Equal(t, primary, msg.String(),
			"primary key %q must round-trip through String() so the click presses the same key", primary)
	}
}

// TestReboundHintClickDispatchesLikeKeyPress is the end-to-end contract: after
// rebinding help to the multi-character ctrl+b (which the click dispatcher's
// old switch never enumerated), clicking the "ctrl+b" hint chip opens the
// help overlay — exactly like pressing ctrl+b on the keyboard. Before the fix
// the click was a silent no-op while the key worked, breaking the documented
// "clicking is equivalent to pressing it" (zones.StatusHint doc).
func TestReboundHintClickDispatchesLikeKeyPress(t *testing.T) {
	restoreDefaultKeymap(t)
	require.NoError(t, keys.ApplyOverrides(map[string][]string{"help": {"ctrl+b"}}))

	t.Run("pressing the rebound key opens help", func(t *testing.T) {
		h, _, _ := mouseTestHome(t)
		newFakeClock(h)
		h.focusRegion(layout.RegionAutomations)
		_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlB})
		require.Equal(t, stateHelp, h.state, "pressing the rebound ctrl+b key opens help")
	})

	t.Run("clicking the rebound hint opens help", func(t *testing.T) {
		h, _, _ := mouseTestHome(t)
		newFakeClock(h)
		h.focusRegion(layout.RegionAutomations)
		clickZone(t, h, zones.StatusHint("ctrl+b"))
		require.Equal(t, stateHelp, h.state,
			"clicking the rebound ctrl+b hint opens help, like the key — before the fix this was a silent no-op")
	})
}
