package overlay

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ctrlU = tea.KeyMsg{Type: tea.KeyCtrlU}

func typeInto(handle func(tea.KeyMsg) bool, s string) {
	for _, r := range s {
		handle(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestSearchOverlayCtrlUClearsQuery is #4846's repro: type "gam", ctrl+u, type
// "zzz". ctrl+u was swallowed, so the query became "gamzzz".
func TestSearchOverlayCtrlUClearsQuery(t *testing.T) {
	s := NewSearchOverlay(nil)
	typeInto(s.HandleKeyPress, "gam")
	assert.False(t, s.HandleKeyPress(ctrlU), "ctrl+u edits the query; it does not close search")
	typeInto(s.HandleKeyPress, "zzz")
	assert.Equal(t, "zzz", s.query)
}

func TestProjectPickerPathInputsCtrlUClears(t *testing.T) {
	p := pickerFixture()
	for !p.addRowSelected() {
		p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	}
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, p.adding)
	typeInto(p.HandleKeyPress, "/repos/typo")
	p.HandleKeyPress(ctrlU)
	typeInto(p.HandleKeyPress, "/r")
	assert.Equal(t, "/r", p.pathInput, "add-project path")

	r := NewProjectPickerOverlay([]Project{
		{Name: "moved", Root: "/old/moved", RegistryID: "prj_aaa", MissingPath: true},
	}, "")
	r.HandleKeyPress(keyRune('b'))
	require.True(t, r.rebinding)
	typeInto(r.HandleKeyPress, "/new/typo")
	r.HandleKeyPress(ctrlU)
	typeInto(r.HandleKeyPress, "/n")
	assert.Equal(t, "/n", r.rebindInput, "rebind path")
}

// TestPromptOverlayCtrlUClears guards the rename and initial-prompt fields,
// which get ctrl+u from the bubbles textarea keymap.
func TestPromptOverlayCtrlUClears(t *testing.T) {
	p := NewPromptOverlay("Rename tab", "web")
	p.HandleKeyPress(ctrlU)
	assert.Empty(t, p.Value())
}
