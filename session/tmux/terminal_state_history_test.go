package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// The ATOMIC capture, against REAL tmux (#3169 review). The count and the content
// must come from one invocation, because a bracket around two separate reads still
// reports an incomplete capture as complete: zero, history gained, history cleared,
// zero — both endpoints agree and the returned content omitted lines anyway.
//
// Real tmux rather than a fake, because the property under test is that tmux runs
// both commands in ONE command queue. A fake would only be asserting my own split.
func TestCaptureVisibleWithScrollback_CountAndContentFromOneInvocation(t *testing.T) {
	session := NewTmuxSession("af-atomic-"+t.Name()[:8], "sh -c 'i=1; while [ $i -le 200 ]; do echo atomic-$i; i=$((i+1)); done; exec sleep 60'")
	require.NoError(t, session.Start(t.TempDir()))
	t.Cleanup(func() { _, _ = session.Close() })

	var content string
	var above int
	require.Eventually(t, func() bool {
		var err error
		content, above, err = session.CaptureVisibleWithScrollback()
		return err == nil && above > 0
	}, 10*time.Second, 200*time.Millisecond, "the pane must scroll and report its history in one call")

	// Cross-check against tmux's own answers, so this is not just self-consistent.
	state, err := session.ReadTerminalState()
	require.NoError(t, err)
	require.InDelta(t, state.HistorySize, above, 5,
		"the atomic count must agree with tmux's own history_size")

	full, err := session.CapturePaneContentWithOptions("-", "-")
	require.NoError(t, err)
	visibleLines := strings.Count(content, "\n")
	fullLines := strings.Count(full, "\n")
	require.Greater(t, fullLines, visibleLines,
		"the full capture must be longer than the visible one, or this pane never scrolled")
	require.InDelta(t, fullLines, above+visibleLines, 5,
		"history_size + visible must account for the full capture — the arithmetic is what makes the "+
			"count mean 'lines above the captured region'")
	require.Contains(t, content, "atomic-200", "the visible screen holds the tail")
	require.NotContains(t, content, "atomic-1\n", "and not the head, which is what the marker is for")
}

// The combined capture must never become a REQUIREMENT (#3169 review).
//
// My first version failed the whole capture when an answer could not be split, and
// this path is shared with the TUI's tab panes and ordinal preview resolution — they
// lost scroll mode, the session-gone fallback and ordinal resolution because a
// marker feature turned into a new demand on what preview can CAPTURE. A marker is
// an enhancement to what preview REPORTS.
//
// So an unsplittable answer must be reported as ErrScrollbackCaptureUnparseable
// specifically, and NOT as a session-gone or timeout error: callers degrade on the
// first and act on the other two, so conflating them would trade a real signal for
// a count.
func TestCaptureVisibleWithScrollback_UnparseableAnswerIsItsOwnClass(t *testing.T) {
	dir := t.TempDir()
	// A producer that answers the plain capture shape — one line, no count.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"),
		[]byte("#!/bin/sh\nprintf 'just pane content'\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ts := NewTmuxSessionWithDeps("unparseable", "sh", MakePtyFactory(), cmd.MakeExecutor())

	_, _, err := ts.CaptureVisibleWithScrollback()
	require.ErrorIs(t, err, ErrScrollbackCaptureUnparseable,
		"an answer with no count line must be its own class so callers can degrade to a plain capture")
	require.NotErrorIs(t, err, ErrSessionGone,
		"and must NOT read as a vanished session — the session-gone fallback acts on that")
	require.NotErrorIs(t, err, ErrTmuxTimeout,
		"nor as a wedged server, which callers treat as unknown state")
}
