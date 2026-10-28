package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// A short answer is a PARSE FAILURE, not a silently padded default. The producer
// and parser must agree on shape; falling short is the bug to surface, not a
// signal to fabricate a value the producer did not send (#3169).
func TestReadTerminalState_RefusesAShortAnswerRatherThanAssumingNoScrollback(t *testing.T) {
	ts := fakeTerminalStateTmux(t, "7 11 1 1 0 1 0 0 1")

	_, err := ts.ReadTerminalState()
	require.Error(t, err,
		"a short answer must surface the producer/parser disagreement, not silently default a missing field to zero")
	require.Contains(t, err.Error(), "want 11 fields")
}

// The ten-field shape an OLDER answer produced (before pane dimensions rode the
// format) is likewise a parse failure, not a "pane size unknown, read on" — a
// short answer means the producer and the parser disagree, which is always the
// bug to surface.
func TestReadTerminalState_RefusesThePreSizeFieldShape(t *testing.T) {
	ts := fakeTerminalStateTmux(t, "7 11 1 1 0 1 0 0 1 51")

	_, err := ts.ReadTerminalState()
	require.Error(t, err)
	require.Contains(t, err.Error(), "want 11 fields")
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
	// Both halves matter, and the second is the one I had missed. IsolateTmux SKIPS
	// when tmux is absent — the package convention, so the suite runs without the
	// external binary — and it also points TMUX_TMPDIR at a private socket dir. My
	// first version used the DEFAULT server, which on a developer box is the one
	// holding their live sessions (#3169 review).
	testguard.IsolateTmux(t)
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
