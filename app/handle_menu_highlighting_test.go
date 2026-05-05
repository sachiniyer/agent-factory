package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// TestHandleMenuHighlighting_BypassesNamingState reproduces issue #317:
// while the user is naming a session, single-letter shortcut keys (the
// entries in keys.GlobalKeyStringsMap such as 'r', 's', 'a', 'p', 'h', 'l',
// etc.) must not be intercepted by handleMenuHighlighting. If they are,
// the key gets re-emitted on a later Update cycle and characters arrive
// out of order ("first" → "fistr").
//
// The fix: handleMenuHighlighting short-circuits in stateNew the same way
// it already does in stateHelp / stateConfirm / stateSelectWorktree.
func TestHandleMenuHighlighting_BypassesNamingState(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew

	// Every single-letter shortcut MUST not be intercepted while naming.
	mappedLetters := []string{"r", "s", "a", "p", "h", "l", "k", "j", "o", "n", "q"}
	for _, k := range mappedLetters {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		_, returnEarly := h.handleMenuHighlighting(msg)
		assert.False(t, returnEarly,
			"key %q must not be intercepted by handleMenuHighlighting in stateNew", k)
		// keySent must stay false so the next key isn't dropped by the
		// alternating flip.
		assert.False(t, h.keySent,
			"keySent must remain false after key %q in stateNew", k)
	}
}

// TestHandleMenuHighlighting_BypassesSearchState covers the same
// short-circuit for stateSearch. While the user is typing into the search
// overlay, shortcut letters must reach the input directly.
func TestHandleMenuHighlighting_BypassesSearchState(t *testing.T) {
	h := newTestHome(t)
	h.state = stateSearch

	for _, k := range []string{"r", "s", "a", "p", "h", "l"} {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		_, returnEarly := h.handleMenuHighlighting(msg)
		assert.False(t, returnEarly,
			"key %q must not be intercepted by handleMenuHighlighting in stateSearch", k)
	}
}

// TestHandleStateNew_TypingShortcutLettersInOrder is the end-to-end
// regression test for #317: typing "first" through handleKeyPress (the
// real entry point) must produce the literal string "first" — not "fistr"
// or any other reordering. Before the fix, 'r' and 's' were intercepted
// by handleMenuHighlighting and re-emitted on a later cycle, arriving
// after the following non-mapped letter.
func TestHandleStateNew_TypingShortcutLettersInOrder(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.namingInstance = inst

	for _, r := range "first" {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
		model, _ := h.handleKeyPress(msg)
		hm, ok := model.(*home)
		require.True(t, ok)
		h = hm
	}

	assert.Equal(t, "first", h.namingInstance.Title,
		"typing 'first' must land as 'first' in the naming input")
}
