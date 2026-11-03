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

// TestSearchOverlayDropsLeadingSlash is #6710's repro: '/' opens the dialog as
// a prompt affordance and the buffer must start empty, yet the play-test still
// captured "/ gam" — a '/' reached the field anyway (a second press, or a
// vim-style "/pattern" paste). No session title or branch begins with '/', so
// a leading one is never intended query text; mid-query '/' stays valid.
func TestSearchOverlayDropsLeadingSlash(t *testing.T) {
	s := NewSearchOverlay(nil)
	assert.False(t, s.HandleKeyPress(keyRune('/')),
		"a stray opener edits nothing; it does not close search")
	typeInto(s.HandleKeyPress, "gam")
	assert.Equal(t, "gam", s.query, "the query starts empty — no '/' may lead it")

	mid := NewSearchOverlay(nil)
	typeInto(mid.HandleKeyPress, "a/b")
	assert.Equal(t, "a/b", mid.query, "a '/' inside the query is ordinary text")

	pasted := NewSearchOverlay(nil)
	assert.False(t, pasted.HandleKeyPress(pasteRunes("/gam")),
		"a vim-style '/pattern' paste is the same affordance, and does not close search")
	assert.Equal(t, "gam", pasted.query, "a pasted leading '/' is dropped the same way")
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
