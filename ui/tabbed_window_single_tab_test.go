package ui

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/require"
)

// TestTabbedWindowSingleTabHeaderRendering guards the single-tab corner case
// (#972's successor after the tab bar's removal, #1024 PR 4). A remote
// instance without terminal_cmd carries exactly one tab; the pane header must
// name it (`title · tab`) and the phantom second slot must not be jumpable.
func TestTabbedWindowSingleTabHeaderRendering(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedRemoteInstance(t)
	w := newTestTabbedWindow()
	setWindowInstance(w, inst)
	require.Equal(t, []string{"Agent"}, w.tabLabels(), "remote without terminal_cmd has a single tab")
	require.Equal(t, 0, w.GetActiveTab(), "the lone tab is active")

	setWindowSize(w, 120, 40)

	lines := strings.Split(w.String(), "\n")
	require.Greater(t, len(lines), 1, "expected the framed pane output")
	headerRow := lines[1] // row 0 is the top border; the header is inside the frame
	require.Contains(t, headerRow, "remote-tabbar · Agent",
		"the pane header must carry `title · tab`; got %q", headerRow)

	require.False(t, w.JumpToTab(1), "the phantom second slot must not be jumpable")
	require.Equal(t, 0, w.GetActiveTab())
}
