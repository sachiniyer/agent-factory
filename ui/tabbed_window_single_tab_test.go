package ui

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/require"
)

// TestTabbedWindowSingleTabBorderRendering guards the single-tab corner case
// (#972). A remote instance without terminal_cmd renders exactly one tab, which
// is both first and last. The bottom border of an active single tab must show a
// vertical bar on BOTH corners; the old mutually exclusive if/else-if chain set
// only the bottom-left bar and left the bottom-right corner at its default "└".
func TestTabbedWindowSingleTabBorderRendering(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedRemoteInstance(t, false)
	w := newTestTabbedWindow()
	setWindowInstance(w, inst)
	require.Equal(t, []string{"Preview"}, w.tabLabels(), "remote without terminal_cmd has a single tab")
	require.Equal(t, 0, w.GetActiveTab(), "the lone tab is active")

	w.SetSize(120, 40)

	lines := strings.Split(w.String(), "\n")

	// The tab renders as three rows: top border, the label row, then the bottom
	// border row. Anchor on the label row so the bottom-border index is robust to
	// the leading blank line(s) JoinVertical emits.
	labelIdx := -1
	for i, ln := range lines {
		if strings.Contains(ln, "Preview") {
			labelIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, labelIdx, 0, "could not find the tab label row in:\n%s", strings.Join(lines, "\n"))
	require.Greater(t, len(lines), labelIdx+1, "missing the tab bottom-border row")

	border := []rune(lines[labelIdx+1])
	require.NotEmpty(t, border, "expected a non-empty tab bottom-border line")

	left := string(border[0])
	right := string(border[len(border)-1])
	require.Equalf(t, "│", left, "single active tab bottom-left corner; line=%q", lines[labelIdx+1])
	require.Equalf(t, "│", right, "single active tab bottom-right corner; line=%q", lines[labelIdx+1])
}
