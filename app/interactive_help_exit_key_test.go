package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
)

// TestFirstRunInteractiveHelpDismissedWithExitKeyStaysInteractive is #2413,
// re-pinned under #4722's contract.
//
// The first-run interactive help is dismissed by ANY key. Its last line reads
// "Press ctrl+] to navigate." — so ctrl+] is the key the screen puts in front
// of the user, and pressing it is the most natural thing to do while reading.
//
// The dismiss key must be consumed by the overlay, never forwarded into the
// pane (#4722: a key spent on a UI transition is not pane input). Forwarding it
// ran it through handleInteractiveKey, where ctrl+] is the one host-reserved
// key — so a first-time user who dismissed the help with the key the help named
// landed back in nav mode instantly, having never typed a character. It also
// typed every other dismiss key into the pane (#4722's leak).
func TestFirstRunInteractiveHelpDismissedWithExitKeyStaysInteractive(t *testing.T) {
	h, _ := liveTestHome(t)
	fakes, _ := stubLiveTermFactory(t)

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	require.Nil(t, cmd, "first-time interactive entry waits on the help overlay")
	require.Equal(t, stateHelp, h.state, "precondition: the first-run help is showing")

	// The user presses the key the overlay just told them about.
	_, cmd = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	runHermeticCmd(t, h, cmd, 0)

	assert.True(t, h.interactive,
		"dismissing the first-run help with ctrl+] — the key the overlay itself names — must "+
			"leave the user IN interactive mode. The dismiss key is consumed by the overlay "+
			"(#4722); forwarding it would run the mode straight back out (#2413).")

	require.Len(t, *fakes, 1, "the pane's attachment must still be bound")
	assert.Empty(t, (*fakes)[0].keys,
		"ctrl+] is the host's mode-exit key, not pane input — it must be swallowed, not typed "+
			"into the agent as a raw control byte")

	// The mode is genuinely usable, not just flagged on: ordinary keys forward…
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	assert.Equal(t, []string{"z"}, (*fakes)[0].keys,
		"interactive mode must actually forward keystrokes after the help dismiss")

	// …and the NEXT ctrl+] is the one that exits, exactly as the help promised.
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	assert.False(t, h.interactive,
		"ctrl+] pressed inside interactive mode must still return to navigation — the fix "+
			"suppresses the transition key, never the key itself")
	assert.Equal(t, []string{"z"}, (*fakes)[0].keys,
		"the exit ctrl+] must not be forwarded to the pane either")
}

// TestFirstRunInteractiveHelpConsumesDismissKeys pins the #4722 contract on the
// first-run help path: whatever key dismisses the overlay — a printable rune,
// Enter, or a control key — is consumed by the dismissal and must NOT be typed
// into the pane it just entered. Replaying it would submit a half-typed input
// line (Enter) or paste a stray character into an agent's composer.
func TestFirstRunInteractiveHelpConsumesDismissKeys(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"letter", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}},
		// ctrl+c is pane input once interactive mode is live — but the keypress
		// that dismisses the overlay is spent on the dismissal, so it must not
		// reach the pane as an interrupt either.
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _ := liveTestHome(t)
			fakes, _ := stubLiveTermFactory(t)

			_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
			require.Nil(t, cmd)
			require.Equal(t, stateHelp, h.state)

			_, cmd = h.handleKeyPress(c.msg)
			runHermeticCmd(t, h, cmd, 0)

			require.True(t, h.interactive, "dismissing first-run help enters interactive mode")
			require.Len(t, *fakes, 1)
			assert.Empty(t, (*fakes)[0].keys,
				"the key that dismisses the first-run interactive help must be consumed, "+
					"never forwarded into the pane (#4722)")
		})
	}
}
