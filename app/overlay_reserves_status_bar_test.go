package app

import (
	"fmt"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/require"
)

// TestModalOverlayLeavesTheStatusBarIntact is #2578. The help overlay was
// composited by centering it over the WHOLE frame, so an overlay tall enough to
// reach the bottom painted its border straight through the hint row — `D kill`
// rendered as `D ki`, `? help` as `elp`. The row read as corruption rather than
// as an overlay, on the one screen a confused user goes to.
//
// The assertion is the strongest available: the status-bar rows must be byte-
// for-byte what they are with no overlay up (the fade only rewrites SGR codes,
// so stripping ANSI compares exactly the printable cells).
func TestModalOverlayLeavesTheStatusBarIntact(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {200, 50}} {
		t.Run(sizeName(size), func(t *testing.T) {
			h := newTestHome(t)
			resizeHome(h, size[0], size[1])
			want := statusBarRows(t, h.View(), size[1])

			_, _ = h.showHelpScreen(helpTypeGeneral{}, nil)
			require.Equal(t, stateHelp, h.state, "precondition: the help overlay is up")
			got := statusBarRows(t, h.View(), size[1])

			require.Equal(t, want, got,
				"the overlay painted over the status bar instead of stopping above it")
		})
	}
}

func sizeName(size [2]int) string {
	return fmt.Sprintf("%dx%d", size[0], size[1])
}

// statusBarRows returns the frame's final layout.StatusBarRows lines with ANSI
// stripped.
func statusBarRows(t *testing.T, frame string, height int) []string {
	t.Helper()
	lines := strings.Split(frame, "\n")
	require.Len(t, lines, height, "the frame must tile the whole window")
	rows := make([]string, 0, layout.StatusBarRows)
	for _, line := range lines[height-layout.StatusBarRows:] {
		rows = append(rows, xansi.Strip(line))
	}
	return rows
}
