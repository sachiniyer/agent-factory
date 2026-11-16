package tmux

import (
	"strings"
	"testing"
	"time"

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
