package app

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/keys"
)

// Printable keys that arrive in one terminal read are the separate keys the
// user pressed, not one key (#7015). Bubble Tea delivers such a burst as a
// single message ("kk"), and nav dispatch resolves keys by their exact string,
// so the burst matched no binding. On the tree the presses were dropped. On a
// focused pane the burst read as an unbound letter, which switches into
// keyboard mode and types (#4722 Option 3), so a user who pressed `k` twice to
// move the cursor typed into the agent. The property restored here: in nav
// mode, a key sequence behaves the same however the terminal chunked it into
// reads.

// handleKeyInput is one keyboard input event: the per-input prologue that every
// key message runs (recovery dismissal, and disarming a stale quit-discard
// confirmation), then dispatch.
func (m *home) handleKeyInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	dismissed, endInput := m.beginUserInput()
	defer endInput()
	if dismissed {
		return m, nil
	}
	return m.handleKeyPress(msg)
}

// navKeyDispatch reports whether the next key goes to nav-mode dispatch, the
// exact-string key map that a burst defeats. It does not when an overlay owns
// the keyboard, when keyboard mode forwards keys to a pane, or while a
// full-screen attach is starting or live (the #1530 fence handleEnter and
// enterPane apply).
func (m *home) navKeyDispatch() bool {
	return m.state == stateDefault && !m.interactive && !m.attachTransitioning && !m.attached.Load()
}

// splitNavKeyBurst returns the separate keys of a rune burst that is about to
// reach nav dispatch, or nil when msg should be handled as it is. Keyboard mode
// forwards a burst whole, an overlay's text input takes it whole, and pastes
// and Alt keys are never split (keys.SplitRuneBurst).
func (m *home) splitNavKeyBurst(msg tea.KeyMsg) []tea.KeyMsg {
	if !m.navKeyDispatch() {
		return nil
	}
	return keys.SplitRuneBurst(msg)
}

// handleKeyBurst delivers burst, the split of msg, one key at a time. Each key
// is its own input event (handleKeyInput), so per-input bookkeeping runs between
// them exactly as it does between separate key messages. Without that, a `j`
// in "jq" would not disarm a refused quit's discard confirmation, and the `q`
// would discard unsaved edits (Codex on #7020).
//
// Splitting stops the moment a key takes the TUI out of nav dispatch, for
// example an unbound letter that enters keyboard mode, or one that opens the
// first-run help or another overlay. The rest of the burst is then delivered
// whole, to wherever the TUI now sends keys. That is where the user meant it
// to go. It is pane input in keyboard mode. It is the text of a form. After the
// first-run help, it is the dismiss key, which the deferred activation then
// types into the pane. Splitting further would run the rest as nav keys in the
// window before a deferred switch lands: in "zxx", the second `x` would hide
// the pane it was meant for (Codex on #7020).
func (m *home) handleKeyBurst(msg tea.KeyMsg, burst []tea.KeyMsg) (tea.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0, len(burst))
	for i, k := range burst {
		if i > 0 && !m.navKeyDispatch() {
			if rest := msg.Runes[i:]; len(rest) > 1 {
				k = tea.KeyMsg{Type: tea.KeyRunes, Runes: append([]rune(nil), rest...)}
			}
			_, cmd := m.handleKeyInput(k)
			return m, tea.Batch(append(cmds, cmd)...)
		}
		_, cmd := m.handleKeyInput(k)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}
