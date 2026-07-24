package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
)

// TestFirstRunInteractiveHelpDismissedWithExitKeyStaysInteractive is #2413.
//
// The first-run interactive help is dismissed by ANY key, and the dismiss key is
// then replayed into the pane so it is not swallowed (#1576). The overlay's last
// line reads "Press ctrl+] to return to navigation." — so ctrl+] is the key the
// screen puts in front of the user, and pressing it is the most natural thing to
// do while reading.
//
// Replaying it ran it through handleInteractiveKey, where ctrl+] is the one
// host-reserved key: it exits interactive mode. So a first-time user who
// dismissed the help with the key the help named landed back in nav mode
// instantly, having never typed a character — interactive mode looked simply
// broken, on the one run where the user is least equipped to tell a bug from
// their own mistake.
//
// The existing coverage (TestFirstRunInteractiveHelpForwardsDismissKey) dismisses
// with `q`, which forwards harmlessly, so nothing caught this.
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
			"leave the user IN interactive mode. Replaying it through handleInteractiveKey "+
			"exits immediately, so the mode never starts (#2413).")

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
			"suppresses the REPLAY, never the key itself")
	assert.Equal(t, []string{"z"}, (*fakes)[0].keys,
		"the exit ctrl+] must not be forwarded to the pane either")
}

// TestFirstRunInteractiveHelpStillForwardsOrdinaryDismissKeys guards the
// behavior #1576 added and #2413 must not cost: every dismiss key that is NOT
// the host-reserved exit key still reaches the pane, so the keystroke the user
// typed is not silently eaten.
func TestFirstRunInteractiveHelpStillForwardsOrdinaryDismissKeys(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyMsg
		want string
	}{
		{"letter", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, "q"},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, "enter"},
		// ctrl+c is deliberately pane input inside interactive mode (it interrupts
		// the agent, not af) — TestInteractiveForwardsAllKeysIncludingTab pins
		// that, and the #2413 guard must not widen to it.
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}, "ctrl+c"},
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
			assert.Equal(t, []string{c.want}, (*fakes)[0].keys,
				"the key that dismisses the first-run interactive help must reach the pane (#1576)")
		})
	}
}
