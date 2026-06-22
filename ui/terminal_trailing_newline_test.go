package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests mirror the PreviewPane trailing-newline regression tests
// (#649) for TerminalPane.String(). tmux capture-pane output commonly ends
// in "\n", which makes strings.Split produce one extra (empty) element. Before
// #898 TerminalPane.String() lacked the trailing-empty strip that
// PreviewPane.String() has, so the off-by-one pushed exact/near-fit content
// into the truncation branch and dropped the oldest visible line.

// TestTerminalTrailingNewlineAtExactHeight checks the boundary: content that
// fills exactly height lines but ends in "\n" must not be truncated. The
// trailing-empty strip exists to handle this case.
func TestTerminalTrailingNewlineAtExactHeight(t *testing.T) {
	const paneHeight = 5
	contentLines := []string{"a", "b", "c", "d", "e"}
	text := strings.Join(contentLines, "\n") + "\n"

	tp := NewTabPane()
	tp.SetSize(80, paneHeight)
	tp.content = tabContentState{text: text}

	rendered := tp.String()
	for _, line := range contentLines {
		require.Contains(t, rendered, line,
			"line %q must remain visible at exact-fit boundary — trailing newline must not drop the oldest line", line)
	}
}

// TestTerminalBottomTruncateShowsNewestLines is the regression analogue of
// #649 for TerminalPane: when content exceeds the pane height, the newest
// height lines (bottom) must be kept and the oldest dropped.
func TestTerminalBottomTruncateShowsNewestLines(t *testing.T) {
	const totalLines = 100
	const paneHeight = 10

	lines := make([]string, totalLines)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%03d", i+1)
	}
	text := strings.Join(lines, "\n")

	tp := NewTabPane()
	tp.SetSize(80, paneHeight)
	tp.content = tabContentState{text: text}

	rendered := tp.String()

	// The newest height lines must be present; the oldest must not.
	for i := totalLines - paneHeight; i < totalLines; i++ {
		require.Contains(t, rendered, lines[i],
			"newest line %q must be visible after truncation", lines[i])
	}
	for i := 0; i < totalLines-paneHeight; i++ {
		require.NotContains(t, rendered, lines[i],
			"older line %q must NOT be visible after truncation", lines[i])
	}
}

// TestTerminalTrailingNewlineWithMoreContentThanHeight is the core #898 case:
// content that exceeds the pane height AND ends in "\n". The trailing empty
// element must be stripped before truncation so the slice keeps the correct
// newest height lines, not one line too few from the bottom.
func TestTerminalTrailingNewlineWithMoreContentThanHeight(t *testing.T) {
	const paneHeight = 5
	// 6 real lines + trailing newline. With the trailing empty element stripped,
	// the newest 5 lines (b..f) are shown and only the oldest real line ("a") is
	// dropped. Without the strip, Split yields 7 elements and the truncation
	// slices [c d e f ""], wrongly dropping "b" as well.
	text := "a\nb\nc\nd\ne\nf\n"

	tp := NewTabPane()
	tp.SetSize(80, paneHeight)
	tp.content = tabContentState{text: text}

	rendered := tp.String()
	for _, line := range []string{"b", "c", "d", "e", "f"} {
		require.Contains(t, rendered, line,
			"newest line %q must be visible — trailing newline must not drop an extra real line", line)
	}
	require.NotContains(t, rendered, "a",
		"only the single oldest real line should be dropped when content exceeds height")
}

// TestTerminalNoTrailingNewlineUnchanged guards that content without a trailing
// newline behaves exactly as before: all lines that fit remain visible.
func TestTerminalNoTrailingNewlineUnchanged(t *testing.T) {
	tp := NewTabPane()
	tp.SetSize(80, 10)
	tp.content = tabContentState{text: "one\ntwo\nthree\nfour\nfive"}

	rendered := tp.String()
	for _, line := range []string{"one", "two", "three", "four", "five"} {
		require.Contains(t, rendered, line,
			"line %q must remain visible when content fits and has no trailing newline", line)
	}
}
