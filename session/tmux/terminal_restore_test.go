package tmux

import (
	"strings"
	"testing"
)

// TestNeutralTerminalRestore_ResetsKeyboardEncoding pins the half of the neutral
// restore that #1833 made reachable. A modern agent CLI negotiates the kitty
// keyboard protocol and/or xterm modifyOtherKeys at startup, and the raw attach
// proxy sets those modes on the REAL terminal. Until #1833 the detach key was
// unrecognizable in those modes — the user could not leave the attach at all —
// so this restore never ran from an upgraded terminal and the omission was
// invisible.
//
// Now that an encoded detach works, restoring everything EXCEPT the keyboard
// encoding hands the user back a terminal that still reports Ctrl keys as escape
// sequences: host shortcuts like Ctrl-] break until a manual reset. The neutral
// state this constant promises includes legacy keyboard reporting.
func TestNeutralTerminalRestore_ResetsKeyboardEncoding(t *testing.T) {
	for _, tc := range []struct{ esc, what string }{
		{"\x1b[=0;1u", "zero the kitty keyboard progressive-enhancement flags"},
		{"\x1b[>4;0m", "turn xterm modifyOtherKeys off"},
	} {
		if !strings.Contains(NeutralTerminalRestore, tc.esc) {
			t.Errorf("neutral restore must %s (%q): a pane program can leave the real "+
				"terminal in that mode, and after #1833 the detach that lands the user "+
				"back here no longer resets it", tc.what, tc.esc)
		}
	}
}

// TestNeutralTerminalRestore_ResetsKeyboardOnBothBuffers is the per-screen half.
// The kitty spec is explicit — "Terminals must maintain separate stacks for the
// main and alternate screens", precisely so an alt-screen editor can change the
// mode "without affecting the mode in the main screen".
//
// So a reset written only before `?1049l` zeroes the buffer we are LEAVING. A
// pane program that turned the protocol on while still on the main screen (before
// its own `?1049h`) leaves the main buffer enhanced, and switching back to it
// restores that state: Ctrl keys keep arriving as CSI sequences on the buffer the
// user is actually left looking at. Resetting the alt screen we are abandoning
// fixes nothing there. The reset has to happen on both sides of the switch.
func TestNeutralTerminalRestore_ResetsKeyboardOnBothBuffers(t *testing.T) {
	switchIdx := strings.Index(NeutralTerminalRestore, "\x1b[?1049l")
	if switchIdx < 0 {
		t.Fatal("restore is missing the main-buffer switch")
	}
	for _, tc := range []struct{ esc, what string }{
		{"\x1b[=0;1u", "kitty keyboard flags"},
		{"\x1b[>4;0m", "modifyOtherKeys"},
	} {
		before := strings.Index(NeutralTerminalRestore, tc.esc)
		after := strings.LastIndex(NeutralTerminalRestore, tc.esc)
		if before < 0 || before > switchIdx {
			t.Errorf("%s (%q) must be reset before the ?1049l switch, on the buffer being left",
				tc.what, tc.esc)
		}
		if after <= switchIdx {
			t.Errorf("%s (%q) must ALSO be reset after the ?1049l switch: the main screen "+
				"keeps its own stack, so a program that enabled the protocol there before "+
				"its ?1049h leaves it enhanced across the switch back", tc.what, tc.esc)
		}
	}
}

// TestNeutralTerminalRestore_Ordering pins the ordering contract the constant's
// doc comment states, since the sequences are only correct as a sequence:
// reporting modes go quiet first so nothing new arrives mid-restore, then the
// keyboard encoding drops to legacy on the buffer being left, and only then the
// screen buffer switches back.
func TestNeutralTerminalRestore_Ordering(t *testing.T) {
	for _, tc := range []struct{ first, then, why string }{
		{"\x1b[?2004l", "\x1b[=0;1u", "reporting modes go quiet before the keyboard encoding changes"},
		{"\x1b[=0;1u", "\x1b[?1l", "the kitty flags drop before cursor-key mode is normalized"},
		{"\x1b[>4;0m", "\x1b[?1049l", "keyboard modes are reset before the buffer switch"},
		{"\x1b[?1049l", "\x1b[0m", "cosmetics are applied to the buffer we land on"},
	} {
		i, j := strings.Index(NeutralTerminalRestore, tc.first), strings.Index(NeutralTerminalRestore, tc.then)
		if i < 0 || j < 0 {
			t.Fatalf("restore is missing %q or %q", tc.first, tc.then)
		}
		if i > j {
			t.Errorf("%q must come before %q: %s", tc.first, tc.then, tc.why)
		}
	}
}
