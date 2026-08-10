package tmux

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
)

// #3169: the scrollback size must be reported alongside the terminal state.
//
// `af sessions preview` captures the visible screen by default, and for any pane an
// agent has worked in the answer is scrolled just above it. The capture is
// well-formed and simply omits it, with nothing saying it was partial — this repo's
// failed-read-is-not-an-empty-result class applied to its primary fleet-debugging
// verb. Marking a partial capture requires knowing how many lines sit above the
// captured region.
//
// It rides ReadTerminalState because that display-message ALREADY runs on every
// preview (previewSnapshotWithModes), so the count costs NO extra tmux invocation
// and no second capture — which is what the issue asked to check before adding one.
//
// history_size is exactly the lines above the visible region. Measured against real
// tmux while designing this: a 10-row pane holding 61 lines of output reported
// history_size 51, and a full capture returned 61 lines.
func TestReadTerminalState_ReportsScrollbackSize(t *testing.T) {
	ts := fakeTerminalStateTmux(t, "7 11 1 1 0 1 0 0 1 51")

	state, err := ts.ReadTerminalState()
	require.NoError(t, err)
	require.Equal(t, 51, state.HistorySize,
		"the lines above the visible region must be reported, or a partial preview cannot be "+
			"distinguished from a complete one (#3169)")
	// The pre-existing fields must still land, since this widens one format string
	// every preview depends on.
	require.Equal(t, 7, state.CursorRow)
	require.Equal(t, 11, state.CursorCol)
	require.True(t, state.Modes.AlternateScreen)
	require.True(t, state.Modes.MouseSGR)
}

// Zero means the visible screen IS the whole pane, so no marker is printed. That
// answer must stay distinguishable from "we did not measure", which is the failure
// this issue is about — see PreviewSnapshot.LinesAboveKnown.
func TestReadTerminalState_ReportsZeroScrollback(t *testing.T) {
	ts := fakeTerminalStateTmux(t, "0 0 0 0 0 0 0 0 0 0")

	state, err := ts.ReadTerminalState()
	require.NoError(t, err)
	require.Equal(t, 0, state.HistorySize)
}

// A short answer is a PARSE FAILURE, not a zero. An older tmux (or a truncated
// answer) reporting nine fields must not be read as "no scrollback": that is the
// fabricated negative this whole issue is about, one layer down.
func TestReadTerminalState_RefusesAShortAnswerRatherThanAssumingNoScrollback(t *testing.T) {
	ts := fakeTerminalStateTmux(t, "7 11 1 1 0 1 0 0 1")

	_, err := ts.ReadTerminalState()
	require.Error(t, err,
		"nine fields must not silently mean history_size=0; an unmeasured pane is not an unscrolled one")
	require.Contains(t, err.Error(), "want 10 fields")
}

func fakeTerminalStateTmux(t *testing.T, fields string) *TmuxSession {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '" + fields + "\\n'\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return NewTmuxSessionWithDeps("terminal-state-history", "sh", MakePtyFactory(), cmd.MakeExecutor())
}
