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
	p := NewTabPane(previewFromInstance)
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

// TestTabPaneWideCaptureMarksTheCut is the #4175 half of #1082's contract: a
// preview-only session's tmux pane stays 80 columns while the preview box is
// narrower, so a row the pane had to shorten must carry "…" in its last cell —
// the same convention truncated tab and task names use — rather than amputate
// the tail silently.
func TestTabPaneWideCaptureMarksTheCut(t *testing.T) {
	const w, h = 56, 4
	p := NewTabPane(previewFromInstance)
	p.SetSize(w, h)

	p.mu.Lock()
	p.content = tabContentState{text: "AAAAAAAAAA-10 BBBBBBBBBB-20 CCCCCCCCCC-30 DDDDDDDDDD-40 EEEEEEEEEE-50 FFFFFFFFFF-60\nshort"}
	p.mu.Unlock()

	got := strings.Split(p.String(), "\n")
	require.Len(t, got, h)
	row := stripANSI(got[0])
	assert.Equal(t, w, lipgloss.Width(got[0]), "a cut row still measures exactly the pane width")
	assert.True(t, strings.HasPrefix(row, "AAAAAAAAAA-10"),
		"the leading columns of the capture survive: %q", row)
	assert.True(t, strings.HasSuffix(row, "…"),
		"the cut reads as truncation, not amputation: %q", row)

	short := stripANSI(got[1])
	assert.NotContains(t, short, "…",
		"a row that fits carries no marker — it must mean cut, not full: %q", short)
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
