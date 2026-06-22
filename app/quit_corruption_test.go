package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleQuit_ReachesQuitWithCorruptedInstances is the app-level half of
// sachiniyer/agent-factory#938. Both `q` and `Ctrl+C` route through
// handleQuit, which saves session state before exiting and returns early
// (showing an error overlay, no tea.Quit) on save failure. Before the fix, a
// corrupted instances.json made the save fail every time, so the user could
// never quit cleanly — force-kill was the only escape.
//
// With the TUI save path now overwriting unparseable disk state (mirroring the
// daemon), handleQuit's save succeeds and it must return a tea.Quit command,
// not an error overlay.
func TestHandleQuit_ReachesQuitWithCorruptedInstances(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(500, 1)

	// Corrupt instances.json mid-session: successfully readable bytes that fail
	// json.Unmarshal (not a read error). newTestHome redirects
	// AGENT_FACTORY_HOME, so this writes to the same hermetic file the home's
	// storage reads back.
	require.NoError(t, config.DefaultState().SaveInstances(h.repoID, []byte(`{not valid json`)))

	_, cmd := h.handleQuit()

	require.NotNil(t, cmd, "handleQuit must return a command even with corrupted instances.json")
	msg := cmd()
	_, isQuit := msg.(tea.QuitMsg)
	assert.True(t, isQuit, "handleQuit must reach tea.Quit (got %T) instead of trapping the user in an error loop (#938)", msg)
	assert.Empty(t, strings.TrimSpace(h.errBox.String()), "a recoverable corruption must not surface a blocking error overlay")
}
