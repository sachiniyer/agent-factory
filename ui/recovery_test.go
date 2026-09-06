package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/require"
)

func TestRecoveryNarrowHeadingStaysOneBoldRow(t *testing.T) {
	truecolor(t)
	for _, failed := range []bool{false, true} {
		out := RecoveryContent("No project registered", "", "Check.", failed, 18)
		lines := strings.Split(out, "\n")
		require.Len(t, lines, 3, "one heading, one blank row, one action")
		require.Equal(t, 18, lipgloss.Width(lines[0]))
		require.Contains(t, stripANSI(lines[0]), "…", "a truncated heading marks its cut")
		require.Contains(t, lines[0], "\x1b[1;", "only the condition is bold")
		require.NotContains(t, lines[2], "\x1b[1;")
	}
}

func TestRecoveryNarrowActionWrapsWithinPane(t *testing.T) {
	r := layout.Rect{W: 18, H: 8}
	out := RecoveryScreen(r, "No tasks", "", "Press n to create one.", false)
	requireExactRect(t, out, r, "narrow recovery")
	plain := stripANSI(out)
	require.Contains(t, plain, "No tasks")
	require.Contains(t, plain, "Press n to create")
	require.Contains(t, plain, "one.")
	require.NotContains(t, plain, "╭", "recovery has no card frame")
}

func TestRecoveryLongErrorKeepsNextActionVisible(t *testing.T) {
	truecolor(t)
	r := layout.Rect{W: 40, H: 10}
	action := "Press any key to return to the form."
	out := RecoveryScreen(r, "Cannot create session", strings.Repeat("The daemon refused this request. ", 100), action, true)
	requireExactRect(t, out, r, "long failure at the minimum usable size")
	plain := stripANSI(out)
	require.Contains(t, plain, "Cannot create session")
	require.Contains(t, plain, action, "only raw details may be clipped")
	require.True(t, strings.HasSuffix(strings.TrimSpace(plain), action))
}
