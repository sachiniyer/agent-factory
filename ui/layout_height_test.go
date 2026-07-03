package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

// renderedLineCount counts the lines a component emits. Every component must
// emit exactly its allocated height: lipgloss.Place pads but never truncates,
// so a single over-tall pane pushes the menu and error box below the fold.
func renderedLineCount(s string) int {
	return len(strings.Split(s, "\n"))
}

// blankRuns returns the number of fully-empty lines at the top and bottom of
// the output. renderCenteredFallback pads with literally-empty lines, while
// rendered content lines (including the ASCII art's own blank rows) are
// space-padded to the pane width — so literal "" lines are exactly the
// vertical-centering padding.
func blankRuns(s string) (top, bottom int) {
	lines := strings.Split(s, "\n")
	for _, l := range lines {
		if l != "" {
			break
		}
		top++
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] != "" {
			break
		}
		bottom++
	}
	return top, bottom
}

// TestPreviewFallbackWrappedArtMatchesAllocatedHeight is the regression test
// for #699. At an 80-column terminal the preview pane is ~48 columns wide and
// the 58-column fallback ASCII art wraps, increasing the rendered line count.
// Centering math that counts pre-wrap lines then under-pads and the overall
// output overflows the pane allocation (49 lines for a 30-line pane).
func TestPreviewFallbackWrappedArtMatchesAllocatedHeight(t *testing.T) {
	for _, tc := range []struct{ w, h int }{
		{48, 15}, // 80x24 terminal: real preview pane content size
		{48, 30},
		{48, 60},
		{80, 30}, // wide enough that the art does not wrap
	} {
		p := NewTabPane()
		p.SetSize(tc.w, tc.h)
		p.setFallbackState("msg")

		got := renderedLineCount(p.String())
		require.Equal(t, tc.h, got,
			"%dx%d: fallback must render exactly the allocated height", tc.w, tc.h)
	}
}

// TestPreviewFallbackCentersWrappedLineCount verifies the padding is computed
// from the wrapped (rendered) line count: the top and bottom padding runs must
// be balanced even when wrapping changes the art's height (#699).
func TestPreviewFallbackCentersWrappedLineCount(t *testing.T) {
	for _, tc := range []struct{ w, h int }{
		{48, 60}, // art wraps at this width
		{80, 30}, // art does not wrap
	} {
		p := NewTabPane()
		p.SetSize(tc.w, tc.h)
		p.setFallbackState("msg")
		out := p.String()

		require.Contains(t, out, "msg",
			"%dx%d: message must be visible when the box is tall enough", tc.w, tc.h)
		top, bottom := blankRuns(out)
		require.Greater(t, top, 0,
			"%dx%d: content shorter than the box must be padded down from the top", tc.w, tc.h)
		require.InDelta(t, top, bottom, 1,
			"%dx%d: top/bottom padding must be balanced (centering must use the wrapped line count)", tc.w, tc.h)
	}
}

// TestTerminalFallbackMatchesNormalModeHeight is the regression test for
// #703. TabbedWindow.SetSize already strips tab-bar and window-frame chrome
// before sizing the terminal pane, but fallback rendering subtracted another
// 3+4 lines, rendering 7 lines short (23 lines for a 30-line pane) and
// centering the message 4 lines too high. Fallback and normal mode must fill
// the same allocation. PreviewPane had the identical bug, fixed for #616.
func TestTerminalFallbackMatchesNormalModeHeight(t *testing.T) {
	for _, h := range []int{20, 25, 30, 50} {
		fb := NewTabPane()
		fb.SetSize(80, h)
		fb.setFallbackState("Select an instance to open a terminal")
		require.Equal(t, h, renderedLineCount(fb.String()),
			"height=%d: fallback must render exactly the allocated height", h)

		normal := NewTabPane()
		normal.SetSize(80, h)
		normal.content = tabContentState{fallback: false, text: "line1\nline2"}
		require.Equal(t, h, renderedLineCount(normal.String()),
			"height=%d: normal mode must render exactly the allocated height", h)
	}
}

// TestTerminalFallbackWrappedArtMatchesAllocatedHeight covers the terminal
// pane's variant of #699: at narrow widths the fallback art wraps, and the
// pre-fix centering math both miscentered and overflowed the allocation.
func TestTerminalFallbackWrappedArtMatchesAllocatedHeight(t *testing.T) {
	for _, tc := range []struct{ w, h int }{
		{48, 15},
		{48, 30},
		{80, 30},
	} {
		tp := NewTabPane()
		tp.SetSize(tc.w, tc.h)
		tp.setFallbackState("msg")
		require.Equal(t, tc.h, renderedLineCount(tp.String()),
			"%dx%d: fallback must render exactly the allocated height", tc.w, tc.h)
	}
}

// TestContentPaneRendersExactlyAllocatedHeight is the regression test for
// #700. lipgloss.Place and Style.Height are minimums — they pad short content
// but never truncate tall content — so an over-tall task/hooks list made
// ContentPane.String() exceed its SetSize allocation and pushed the menu and
// error box below the fold at common terminal sizes. Every mode must render
// exactly the allocated height regardless of content volume.
func TestContentPaneRendersExactlyAllocatedHeight(t *testing.T) {
	// 80x24 terminal: contentWidth = 80 - int(80*0.3) = 56,
	// contentHeight = int(24*0.9) = 21 (see app.updateHandleWindowSizeEvent).
	const w, h = 56, 21

	longName := strings.Repeat("long-task-name-", 8)
	var tasks []task.Task
	for i := 0; i < 40; i++ {
		tasks = append(tasks, task.Task{ID: "t", Name: longName, Prompt: "p"})
	}
	var hooks []string
	for i := 0; i < 40; i++ {
		hooks = append(hooks, strings.Repeat("hook-cmd ", 20))
	}

	tw := newTestTabbedWindow()
	cp := NewContentPane(tw)
	cp.SetSize(w, h)

	cp.SetMode(ContentModeEmpty)
	require.Equal(t, h, renderedLineCount(cp.String()), "empty mode")

	cp.SetMode(ContentModeTasks)
	require.Equal(t, h, renderedLineCount(cp.String()), "tasks mode, no tasks")

	cp.TaskPane().SetTasks(tasks)
	require.Equal(t, h, renderedLineCount(cp.String()),
		"tasks mode with an over-tall task list must not overflow the allocation")

	cp.HooksPane().SetCommands(hooks)
	cp.SetMode(ContentModeHooks)
	require.Equal(t, h, renderedLineCount(cp.String()),
		"hooks mode with an over-tall hooks list must not overflow the allocation")

	// Inline panes must keep the same total height as the tabbed window so
	// the layout does not jump when switching sidebar selections.
	cp.SetMode(ContentModeInstance)
	require.Equal(t, h, renderedLineCount(tw.String()), "tabbed window")
}

// renderedWidth returns the widest line of a component's output, measured in
// terminal cells (lipgloss.Width is ANSI- and wide-rune-aware).
func renderedWidth(s string) int {
	widest := 0
	for _, line := range strings.Split(s, "\n") {
		if lw := lipgloss.Width(line); lw > widest {
			widest = lw
		}
	}
	return widest
}

// TestContentPaneInlinePanesMatchAllocatedWidth is the regression test for
// #821, the width counterpart of the #786 height fix above. windowStyle is
// bordered, and lipgloss Style.Width sets the *inner* content width — so
// renderInlinePane's .Width(w) call rendered w+frame total columns, making
// the tasks/hooks/empty panes 2 columns wider than the tabbed window and
// shifting the layout when switching between sidebar selections.
func TestContentPaneInlinePanesMatchAllocatedWidth(t *testing.T) {
	for _, tc := range []struct{ w, h int }{
		{56, 21}, // 80x24 terminal (see TestContentPaneRendersExactlyAllocatedHeight)
		{84, 27}, // 120x30 terminal
	} {
		// Both the inline panes and the tabbed window carve their window
		// width out of the allocation the same way.
		want := AdjustPreviewWidth(tc.w)

		tw := newTestTabbedWindow()
		cp := NewContentPane(tw)
		cp.SetSize(tc.w, tc.h)

		cp.SetMode(ContentModeEmpty)
		require.Equal(t, want, renderedWidth(cp.String()),
			"%dx%d: empty mode", tc.w, tc.h)

		cp.SetMode(ContentModeTasks)
		require.Equal(t, want, renderedWidth(cp.String()),
			"%dx%d: tasks mode, no tasks", tc.w, tc.h)

		cp.TaskPane().SetTasks([]task.Task{
			{ID: "t", Name: strings.Repeat("long-task-name-", 8), Prompt: "p"},
		})
		require.Equal(t, want, renderedWidth(cp.String()),
			"%dx%d: tasks mode with over-wide content", tc.w, tc.h)

		cp.HooksPane().SetCommands([]string{strings.Repeat("hook-cmd ", 20)})
		cp.SetMode(ContentModeHooks)
		require.Equal(t, want, renderedWidth(cp.String()),
			"%dx%d: hooks mode with over-wide content", tc.w, tc.h)

		// Inline panes must render exactly as wide as the tabbed window so
		// the border column does not move when switching sidebar selections.
		cp.SetMode(ContentModeInstance)
		require.Equal(t, want, renderedWidth(tw.String()),
			"%dx%d: tabbed window", tc.w, tc.h)
	}
}
