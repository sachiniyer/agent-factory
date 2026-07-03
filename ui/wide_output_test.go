package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTabPaneWideProcessOutputTruncatesToPaneWidth is the regression test for
// #1082: a process tab whose program emits lines wider than the pane (btop,
// wide tables, unwrapped logs) must render clamped to the pane width. The
// pre-cutover TabPane styled its content with Style.Width, which WRAPS long
// lines onto extra rows — overflowing the pane's height allocation and
// pushing the chrome below it off screen. The ClampToRect contract truncates
// per line instead.
func TestTabPaneWideProcessOutputTruncatesToPaneWidth(t *testing.T) {
	const w, h = 60, 10
	p := NewTabPane()
	p.SetSize(w, h)

	wide := strings.Repeat("0123456789", 30) // 300 cells, 5x the pane width
	var lines []string
	for i := 0; i < h; i++ {
		lines = append(lines, wide)
	}
	p.mu.Lock()
	p.content = tabContentState{text: strings.Join(lines, "\n")}
	p.mu.Unlock()

	out := p.String()
	got := strings.Split(out, "\n")
	require.Len(t, got, h, "wide lines must be truncated, not wrapped onto extra rows")
	for i, line := range got {
		assert.Equalf(t, w, lipgloss.Width(line), "line %d must be exactly the pane width", i)
		assert.Truef(t, strings.HasPrefix(stripANSI(line), "0123456789"),
			"line %d must keep the leading columns of the capture", i)
	}
}

// TestTabbedWindowWideProcessOutputStaysInsideRect covers #1082 end to end at
// the pane level: with wide capture content loaded, the framed workspace pane
// still renders exactly its rect, so the wide tab cannot push the automations
// strip or status bar off screen.
func TestTabbedWindowWideProcessOutputStaysInsideRect(t *testing.T) {
	tw := newTestTabbedWindow()
	r := layout.Rect{W: 80, H: 20}
	tw.SetRect(r)

	wide := strings.Repeat("x", 500)
	tw.tab.mu.Lock()
	tw.tab.content = tabContentState{text: strings.Repeat(wide+"\n", 50)}
	tw.tab.mu.Unlock()

	requireExactRect(t, tw.View(), r, "workspace pane with wide process output")
}
