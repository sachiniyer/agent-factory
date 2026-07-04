package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reachesQuit reports whether the command returned by the key handler resolves
// to tea.Quit.
func reachesQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, isQuit := cmd().(tea.QuitMsg)
	return isQuit
}

func runeKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// dispatchKey drives one key through the real handler as the DISPATCH pass. A
// mapped key press is first intercepted by handleMenuHighlighting (which sets
// keySent and re-emits the same key); the action fires on the re-emitted pass,
// when keySent is already true. Setting keySent here reproduces that second
// pass so the test exercises the dispatch wiring, not the highlight animation.
func dispatchKey(h *home, msg tea.KeyMsg) tea.Cmd {
	h.keySent = true
	_, cmd := h.handleKeyPress(msg)
	return cmd
}

// TestQuitDispatchesThroughKeymap is the regression guard for the #1141
// play-test blocker 1: quit was short-circuited on a hardcoded "q" before the
// generated keymap lookup and had no KeyQuit dispatch case, so rebinding quit
// was a no-op in both directions. Quit must now flow through the generated
// table like every other rebindable action.
func TestQuitDispatchesThroughKeymap(t *testing.T) {
	t.Run("default q quits", func(t *testing.T) {
		require.NoError(t, keys.ApplyOverrides(nil))
		h := newTestHome(t)
		assert.True(t, reachesQuit(dispatchKey(h, runeKey('q'))), "the default quit key must still quit")
	})

	t.Run("rebound quit works and the default goes dead", func(t *testing.T) {
		require.NoError(t, keys.ApplyOverrides(map[string][]string{"quit": {"Q"}}))
		t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })
		h := newTestHome(t)

		assert.True(t, reachesQuit(dispatchKey(h, runeKey('Q'))), "the rebound quit key must quit")
		assert.False(t, reachesQuit(dispatchKey(h, runeKey('q'))), "the replaced default quit key must no longer quit")
	})

	t.Run("ctrl+c is an always-on hard exit regardless of rebinds", func(t *testing.T) {
		require.NoError(t, keys.ApplyOverrides(map[string][]string{"quit": {"Q"}}))
		t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })
		h := newTestHome(t)

		assert.True(t, reachesQuit(dispatchKey(h, tea.KeyMsg{Type: tea.KeyCtrlC})), "ctrl+c must always quit")
	})
}
