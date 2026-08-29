package layout_test

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// stubPane is a minimal Pane implementation exercising the §2.2 contract
// the way real panes will: it records dispatched input and hard-clamps its
// View to its rect via ClampToRect.
type stubPane struct {
	rect    layout.Rect
	focused bool
	content string
	consume bool

	keys []string
}

var _ layout.Pane = (*stubPane)(nil)

func (p *stubPane) SetRect(r layout.Rect) { p.rect = r }
func (p *stubPane) Focused() bool         { return p.focused }
func (p *stubPane) Focus()                { p.focused = true }
func (p *stubPane) Blur()                 { p.focused = false }

func (p *stubPane) HandleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	p.keys = append(p.keys, msg.String())
	return nil, p.consume
}

func (p *stubPane) View() string {
	return layout.ClampToRect(p.content, p.rect)
}

func TestPaneFocusLifecycle(t *testing.T) {
	var pane layout.Pane = &stubPane{}

	assert.False(t, pane.Focused())
	pane.Focus()
	assert.True(t, pane.Focused())
	pane.Blur()
	assert.False(t, pane.Focused())
}

func TestPaneHandleKeyConsumption(t *testing.T) {
	stub := &stubPane{consume: true}
	var pane layout.Pane = stub

	cmd, consumed := pane.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	assert.Nil(t, cmd)
	assert.True(t, consumed, "pane reports the key as consumed")

	stub.consume = false
	_, consumed = pane.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	assert.False(t, consumed, "unconsumed keys bubble to global bindings")

	assert.Equal(t, []string{"j", "q"}, stub.keys)
}

// TestPaneViewIsExactlyRectSized is the Pane-contract check the RFC (§2.6)
// makes shared test infrastructure: whatever the content, View() renders
// exactly the rect handed to SetRect.
func TestPaneViewIsExactlyRectSized(t *testing.T) {
	contents := []string{
		"",
		"short",
		strings.Repeat("very long line that overflows any pane width ", 8),
		strings.Repeat("many\nlines\n", 30),
	}

	l := layout.Grid{Panes: 2}.Solve(132, 43)
	require.False(t, l.Fallback)
	for _, r := range visibleRegions(l) {
		for _, content := range contents {
			var pane layout.Pane = &stubPane{content: content}
			pane.SetRect(r)
			requireExactSize(t, pane.View(), r.W, r.H)
		}
	}
}
