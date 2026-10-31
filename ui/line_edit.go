package ui

import (
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// IsClearLineKey reports whether msg is the line-clear key: ctrl+u, readline's
// kill-line (#4846).
//
// The TUI's bubbles text fields (task form, config editor, rename and
// initial-prompt prompts) already bind it through their default keymaps as
// "delete before cursor". The hand-rolled fields ask here, and the binding is
// read from that same keymap, so every text field answers the same key.
func IsClearLineKey(msg tea.KeyMsg) bool {
	return key.Matches(msg, textinput.DefaultKeyMap.DeleteBeforeCursor)
}

// EditLine applies the editing keys shared by the TUI's hand-rolled
// single-line fields to value. Backspace deletes the last rune, the
// line-clear key empties the field, and space and printable runes append.
// These fields have no cursor, since typing always lands at the end, so
// "delete before cursor" clears the whole value. It returns false for any
// other key, which the caller keeps for its own bindings.
//
// Control runes are stripped from KeyRunes payloads before appending. A
// bracketed paste arrives as one KeyMsg{Type: KeyRunes, Paste: true} whose
// runes bubbletea preserves verbatim, including '\n'/'\r'/'\t'. These fields
// render and budget as single physical lines, so a control rune in the
// payload would split the rendered field, break the mouse-zone geometry, and
// collapse matching (no sanitized title matches a control-rune query). This
// mirrors the paste hardening the naming field already applies (#2640) so
// every hand-rolled single-line field gets it at the shared chokepoint.
func EditLine(value string, msg tea.KeyMsg) (string, bool) {
	switch {
	case IsClearLineKey(msg):
		return "", true
	case msg.Type == tea.KeyBackspace:
		runes := []rune(value)
		if len(runes) == 0 {
			return value, true
		}
		return string(runes[:len(runes)-1]), true
	case msg.Type == tea.KeySpace:
		return value + " ", true
	case msg.Type == tea.KeyRunes:
		return value + string(filterControlRunes(msg.Runes)), true
	}
	return value, false
}

// filterControlRunes returns runes with every Unicode control character
// removed, preserving order. KeyRunes payloads from a bracketed paste may
// carry '\n'/'\r'/'\t' and other control runes verbatim; single-line fields
// must drop them rather than render as multiple physical lines (#2640).
func filterControlRunes(runes []rune) []rune {
	clean := make([]rune, 0, len(runes))
	for _, r := range runes {
		if !unicode.IsControl(r) {
			clean = append(clean, r)
		}
	}
	return clean
}
