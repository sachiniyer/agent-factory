package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestNamingFormCtrlUClearsTitle pins #4846 for the naming form: ctrl+u clears
// the title as it does in every other TUI text field. It goes through
// handleKeyPress, the full routing, because ctrl+u is also the global
// scroll_up binding and must reach the form rather than the preview.
func TestNamingFormCtrlUClearsTitle(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	h.errBox.SetSize(160, 1)
	h.pendingProgram = "claude"

	naming, err := session.NewInstance(session.InstanceOptions{
		Title:   "todo-core",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.namingInstance = naming

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlU})
	require.Equal(t, stateNew, h.state, "ctrl+u edits the title; it does not leave the form")
	require.Empty(t, naming.Title)

	for _, r := range "todo-api" {
		_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	require.Equal(t, "todo-api", naming.Title)
}
