package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// TestHandleQuit_ReachesQuitWithCorruptedInstances guards #938 under the
// single-writer model. Both `q` and `Ctrl+C` route through handleQuit. As of
// #960 PR 4 the TUI no longer writes instances.json at all — the daemon is the
// sole writer — so a corrupted instances.json can never block the quit: handleQuit
// only flushes task/hooks state and then reaches tea.Quit. This pins that a
// corrupt session file on disk leaves the quit path entirely untouched (no error
// overlay, no trapped user).
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
	assert.True(t, commandEmitsQuit(cmd), "handleQuit must reach tea.Quit instead of trapping the user in an error loop (#938)")
	assert.Empty(t, strings.TrimSpace(h.errBox.String()), "a recoverable corruption must not surface a blocking error overlay")
}
