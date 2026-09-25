package ui

import (
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
		return value + string(msg.Runes), true
	}
	return value, false
}
