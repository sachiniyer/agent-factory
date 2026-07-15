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

// TestNeutralTerminalRestore_Ordering pins the ordering contract the constant's
// doc comment states, since the sequences are only correct as a sequence:
// reporting modes go quiet first so nothing new arrives mid-restore, then the
// keyboard encoding drops to legacy, and only then the screen buffer switches
// back — a mode reset written after 1049l would land on the wrong buffer in
// emulators that scope modes per buffer.
func TestNeutralTerminalRestore_Ordering(t *testing.T) {
	for _, tc := range []struct{ first, then, why string }{
		{"\x1b[?2004l", "\x1b[=0;1u", "reporting modes go quiet before the keyboard encoding changes"},
		{"\x1b[=0;1u", "\x1b[?1l", "the kitty flags drop before cursor-key mode is normalized"},
		{"\x1b[>4;0m", "\x1b[?1049l", "keyboard modes are reset before the buffer switch"},
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
