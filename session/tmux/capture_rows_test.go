package tmux

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestCaptureRowsKeepASoftWrappedRowApart pins #7406 against real tmux. Writing
// at the pending-wrap position of a row filled exactly to the pane width makes
// tmux flag that row as soft-wrapped. readline does exactly that on an input
// line as wide as the pane, then returns with CR and prints the command's output
// on the continuation row, so -J glued "$ echo hi" and "hi" into one line. The
// pane printed below does the same without readline: a full row, one character
// at the pending-wrap position, CR, then new text. The rows captures a renderer
// uses must keep the two rows apart.
func TestCaptureRowsKeepASoftWrappedRowApart(t *testing.T) {
	testguard.IsolateTmux(t)
	session := NewTmuxSession("af-capture-rows",
		`sh -c 'set -- $(stty size); printf "%0${2}d" 0; printf "X\rnext-row\n"; exec sleep 60'`)
	require.NoError(t, session.Start(t.TempDir()))
	t.Cleanup(func() { _, _ = session.Close() })

	var joined string
	require.Eventually(t, func() bool {
		var err error
		joined, _, err = session.CaptureVisibleWithScrollback()
		return err == nil && strings.Contains(joined, "next-row")
	}, 10*time.Second, 100*time.Millisecond, "the pane must draw its two rows")
	require.Contains(t, joined, "0next-row",
		"precondition: the joined capture glues the continuation row onto the full row")

	rows, _, err := session.CaptureVisibleRowsWithScrollback()
	require.NoError(t, err)
	require.Contains(t, rows, "\nnext-row", "the continuation row is its own row")
	require.NotContains(t, rows, "0next-row")

	full, err := session.CapturePaneRowsWithOptions("-", "-")
	require.NoError(t, err)
	require.Contains(t, full, "\nnext-row", "the full-history rows capture keeps the row too")
	require.NotContains(t, full, "0next-row")
}

// TestCaptureRowsBoundAGenuineWrapToThePaneWidth pins #7406's second symptom. A
// line two and a half panes wide genuinely wraps across three rows. Joined, it
// came back as one line wider than the pane and the renderer clipped it to the
// pane: a 200-cell line at 80 columns drew 79 cells and an ellipsis, and the
// other 121 were lost. Captured for a renderer it must be one line per row, each
// no wider than the pane, with the line's tail on the last row.
func TestCaptureRowsBoundAGenuineWrapToThePaneWidth(t *testing.T) {
	testguard.IsolateTmux(t)
	session := NewTmuxSession("af-capture-wrap",
		`sh -c 'set -- $(stty size); echo "width=$2"; printf "%0$(( $2 * 5 / 2 - 4 ))d" 0; printf "TAIL\n"; exec sleep 60'`)
	require.NoError(t, session.Start(t.TempDir()))
	t.Cleanup(func() { _, _ = session.Close() })

	var visible string
	require.Eventually(t, func() bool {
		var err error
		visible, _, err = session.CaptureVisibleRowsWithScrollback()
		return err == nil && strings.Contains(visible, "TAIL")
	}, 10*time.Second, 100*time.Millisecond, "the pane must draw the wrapped line")
	match := regexp.MustCompile(`width=(\d+)`).FindStringSubmatch(visible)
	require.NotNil(t, match, "precondition: the pane reports its width")
	width, err := strconv.Atoi(match[1])
	require.NoError(t, err)
	full, err := session.CapturePaneRowsWithOptions("-", "-")
	require.NoError(t, err)

	for name, capture := range map[string]string{"visible": visible, "full history": full} {
		var rows []string
		for _, line := range strings.Split(capture, "\n") {
			line = strings.TrimRight(line, " ")
			if line != "" && strings.Trim(strings.TrimSuffix(line, "TAIL"), "0") == "" {
				rows = append(rows, line)
			}
		}
		require.Len(t, rows, 3, "%s: a line 2.5 panes wide is drawn as three rows, got %q", name, rows)
		for i, row := range rows {
			require.LessOrEqual(t, utf8.RuneCountInString(row), width,
				"%s: row %d is wider than the %d-column pane", name, i+1, width)
		}
		require.True(t, strings.HasSuffix(rows[2], "TAIL"), "%s: the last row holds the tail", name)
	}
}
