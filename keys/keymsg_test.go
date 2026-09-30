package keys

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestKeyMsgForStringRoundtrips verifies the translator rebuilds the KeyMsg
// whose String() is the input — so a click re-derives the same action the
// keyboard press does. The four ctrl+<rune> rebinds Bubble Tea collapses to a
// name round-trip to that name (the keyboard path's canonical spelling),
// keeping click and keyboard on the same dispatch path. ok is required for
// every form a [keys] override or a default binding can produce.
func TestKeyMsgForStringRoundtrips(t *testing.T) {
	cases := []struct{ in, want string }{
		// ctrl+<letter> the click dispatcher's old switch never enumerated.
		{"ctrl+b", "ctrl+b"},
		{"ctrl+f", "ctrl+f"},
		{"ctrl+g", "ctrl+g"},
		{"ctrl+j", "ctrl+j"},
		{"ctrl+n", "ctrl+n"},
		{"ctrl+e", "ctrl+e"},
		// ctrl+<letter> Bubble Tea spells as a word: click must land on the same
		// (named) dispatch key the keyboard press resolves to.
		{"ctrl+i", "tab"},
		{"ctrl+m", "enter"},
		{"ctrl+[", "esc"},
		{"ctrl+?", "backspace"},
		// Special ctrl+ symbols the switch enumerated (ctrl+r/o/]) plus ones it
		// did not (ctrl+@/\^/_), with the Alt modifier layered on top.
		{"ctrl+u", "ctrl+u"},
		{"ctrl+d", "ctrl+d"},
		{"ctrl+p", "ctrl+p"},
		{"ctrl+r", "ctrl+r"},
		{"ctrl+o", "ctrl+o"},
		{"ctrl+]", "ctrl+]"},
		{"ctrl+@", "ctrl+@"},
		{"ctrl+\\", "ctrl+\\"},
		{"ctrl+^", "ctrl+^"},
		{"ctrl+_", "ctrl+_"},
		{"ctrl+h", "ctrl+h"},
		{"alt+ctrl+u", "alt+ctrl+u"},
		{"alt+ctrl+@", "alt+ctrl+@"},
		// Function keys, named keys, and their ctrl/shift variants.
		{"f1", "f1"},
		{"f12", "f12"},
		{"home", "home"},
		{"end", "end"},
		{"pgup", "pgup"},
		{"pgdown", "pgdown"},
		{"insert", "insert"},
		{"delete", "delete"},
		{"backspace", "backspace"},
		{"up", "up"},
		{"down", "down"},
		{"left", "left"},
		{"right", "right"},
		{"shift+up", "shift+up"},
		{"shift+down", "shift+down"},
		{"shift+tab", "shift+tab"},
		{" ", " "},
		{"ctrl+up", "ctrl+up"},
		{"ctrl+down", "ctrl+down"},
		{"ctrl+home", "ctrl+home"},
		{"ctrl+pgup", "ctrl+pgup"},
		{"ctrl+shift+up", "ctrl+shift+up"},
		{"ctrl+shift+home", "ctrl+shift+home"},
		// Alt + anything (a rune, a named key, a ctrl combo) keeps the alt prefix.
		{"alt+x", "alt+x"},
		{"alt+up", "alt+up"},
		{"alt+ctrl+shift+up", "alt+ctrl+shift+up"},
		{"alt+ ", "alt+ "},
		{"alt+?", "alt+?"},
		// Single-rune rebind targets and defaults.
		{"q", "q"},
		{"D", "D"},
		{"/", "/"},
		{"Q", "Q"},
		{"<", "<"},
		{">", ">"},
		{",", ","},
	}
	for _, c := range cases {
		msg, ok := KeyMsgForString(c.in)
		if !ok {
			t.Errorf("KeyMsgForString(%q) ok=false, want true", c.in)
			continue
		}
		if got := msg.String(); got != c.want {
			t.Errorf("KeyMsgForString(%q).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestKeyMsgForStringCoversEveryDefaultBinding asserts the translator covers
// every primary key the menu can advertise against the default keymap, so a
// hint click dispatches the same action the key would (the invariant the click
// path requires).
func TestKeyMsgForStringCoversEveryDefaultBinding(t *testing.T) {
	for k := range GlobalKeyStringsMap {
		msg, ok := KeyMsgForString(k)
		if !ok {
			t.Errorf("default binding %q: KeyMsgForString ok=false, want true", k)
			continue
		}
		if got := msg.String(); got != k {
			t.Errorf("default binding %q round-trips to %q, want %q", k, got, k)
		}
	}
}

// TestKeyMsgForStringRoundtripsEverySpecialType is the automatic version of the
// table above: for every special KeyType Bubble Tea names, the canonical
// spelling it produces must translate back to that same KeyType. This catches
// any gap between specialKeyTypes and the translator's reverse map.
func TestKeyMsgForStringRoundtripsEverySpecialType(t *testing.T) {
	for _, t2 := range specialKeyTypes {
		s := tea.KeyMsg{Type: t2}.String()
		if s == "" || s == "runes" {
			continue
		}
		msg, ok := KeyMsgForString(s)
		if !ok {
			t.Errorf("special key %q (type %v): KeyMsgForString ok=false", s, t2)
			continue
		}
		if msg.String() != s {
			t.Errorf("special key %q round-trips to %q", s, msg.String())
		}
		if msg.Alt {
			t.Errorf("special key %q synthesized with Alt set (no alt prefix)", s)
		}
	}
}

// TestKeyMsgForStringRejectsUntranslatable confirms the translator returns
// false for forms that are not a single physical key, so the dispatcher can
// swallow them rather than fabricating a misleading event.
func TestKeyMsgForStringRejectsUntranslatable(t *testing.T) {
	for _, s := range []string{"", "ctrl+", "alt+", "ctrl+1", "ctrl+2", "xyz", "alt+ctrl+", "runes"} {
		if _, ok := KeyMsgForString(s); ok {
			t.Errorf("KeyMsgForString(%q) ok=true, want false", s)
		}
	}
}
