package layout_test

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// stubPane is a minimal Pane implementation exercising the §2.2 contract
// the way real panes will: it records dispatched input and hard-clamps its
// View to its rect via ClampToRect.
type stubPane struct {
	rect    layout.Rect
	focused bool
	content string
	consume bool

	keys  []string
	mouse []layout.Point
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

func (p *stubPane) HandleMouse(_ tea.MouseMsg, pt layout.Point) tea.Cmd {
	p.mouse = append(p.mouse, pt)
	return nil
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

	l := layout.Grid{Split: true}.Solve(132, 43)
	require.False(t, l.Fallback)
	for _, r := range l.VisibleRegions() {
		for _, content := range contents {
			var pane layout.Pane = &stubPane{content: content}
			pane.SetRect(r)
			requireExactSize(t, pane.View(), r.W, r.H)
		}
	}
}

// TestPaneMouseDispatchViaZones mirrors the root model's §2.5 mouse
// routing: hit-test the click through a zone registry, then hand the pane
// the zone-local point.
func TestPaneMouseDispatchViaZones(t *testing.T) {
	l := layout.Grid{}.Solve(120, 40)
	require.False(t, l.Fallback)

	panes := map[string]*stubPane{}
	reg := zones.NewRegistry()
	for id, r := range l.VisibleRegions() {
		pane := &stubPane{}
		pane.SetRect(r)
		panes[id] = pane
		reg.Register(id, r)
	}

	click := tea.MouseMsg{X: l.PaneA.X + 7, Y: l.PaneA.Y + 3, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
	id, local, ok := reg.Resolve(click.X, click.Y)
	require.True(t, ok)
	require.Equal(t, layout.RegionPaneA, id)

	var target layout.Pane = panes[id]
	assert.Nil(t, target.HandleMouse(click, local))
	assert.Equal(t, []layout.Point{{X: 7, Y: 3}}, panes[id].mouse,
		"pane receives pane-local coordinates")
}
