package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// viewUnderHangGuard renders the model off the test goroutine and fails the test
// if View() does not return within timeout. The fit-and-measure loop in
// renderFittedPaneOverlay used to spin forever for the config editor at terminal
// height 10–11 (the #1431 loop fed non-shrinking content by #1960), so an overlay
// regression must fail the suite loudly instead of hanging it.
func viewUnderHangGuard(t *testing.T, h *home, timeout time.Duration) string {
	t.Helper()
	type result struct{ view string }
	done := make(chan result, 1)
	go func() { done <- result{view: h.View()} }()
	select {
	case r := <-done:
		return r.view
	case <-time.After(timeout):
		t.Fatalf("HUNG: View() did not return within %s — the renderFittedPaneOverlay fit loop spun forever (#1431/#1960 short-terminal freeze)", timeout)
		return ""
	}
}

// renderUnderHangGuard runs renderFittedPaneOverlay off the test goroutine with
// the same hang guard, so the loop's termination can be asserted in isolation
// from the rest of View().
func renderUnderHangGuard(t *testing.T, h *home, r layout.Rect, setSize func(int, int), renderContent func() string, timeout time.Duration) string {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- h.renderFittedPaneOverlay(r, setSize, renderContent) }()
	select {
	case view := <-done:
		return view
	case <-time.After(timeout):
		t.Fatalf("HUNG: renderFittedPaneOverlay did not return within %s — the fit loop failed to terminate", timeout)
		return ""
	}
}

// TestConfigEditorOverlayFitsShortTerminalWithoutFreezing is the regression lock
// for the reported freeze: opening the config editor at terminal height 10 or 11
// (any width >= 40) used to spin renderFittedPaneOverlay forever. The dialog's
// fixed minimum height (~8 lines -> 12 with border/padding) stays taller than the
// terminal, the height shrink branch floors r.H at 1 and dies, the only fitting
// escape required both axes floored, and View() never returned. Heights 10–11 sit
// above the 40x10 hard-min fallback banner, so the overlay path is live. View()
// must now return and compose a terminal-sized frame at every size that froze.
func TestConfigEditorOverlayFitsShortTerminalWithoutFreezing(t *testing.T) {
	for _, hgt := range []int{10, 11} {
		for _, w := range []int{40, 50, 80, 120} {
			t.Run(fmt.Sprintf("%dx%d", w, hgt), func(t *testing.T) {
				h := openConfigEditorForTest(t)
				resizeHome(h, w, hgt)

				view := viewUnderHangGuard(t, h, 5*time.Second)
				requireViewSized(t, view, w, hgt)
				assert.Contains(t, xansi.Strip(view), "Config",
					"the clamped config editor must still be present at %dx%d", w, hgt)
			})
		}
	}
}

// TestRenderFittedPaneOverlayClampsNonShrinkingContent isolates the loop fix
// from the config pane: a renderContent with a fixed minimum height never honors
// setSize, so the height shrink branch floors r.H at 1 and dies while the dialog
// still exceeds the terminal. Before the fix this looped forever (the one-axis-
// floored no-progress case); now the no-progress guard clips the frame to the
// terminal and returns.
func TestRenderFittedPaneOverlayClampsNonShrinkingContent(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 40, 10)
	content := strings.Repeat("line that does not shrink\n", 30)

	view := renderUnderHangGuard(t, h,
		layout.Rect{W: 24, H: 6},
		func(int, int) {},
		func() string { return content },
		5*time.Second,
	)

	assert.Greater(t, lipgloss.Height(content), 10,
		"precondition: the raw content is genuinely taller than the terminal")
	require.Equal(t, 10, lipgloss.Height(view),
		"a non-shrinking dialog taller than the terminal must be clamped to the terminal height, not left at its natural ~30 lines")
	require.LessOrEqual(t, layout.BlockWidth(view), 40,
		"the clamped frame must never exceed the terminal width")
}

// TestRenderFittedPaneOverlayReturnsUnclippedWhenContentFits is the control that
// the no-progress guard did not turn every overlay into a clamp. When content
// shrinks to fit the box (the hooks/tasks premise the loop was written for), the
// loop exits via the original fits branch and returns the dialog unclipped —
// shorter than the terminal rather than clamped to it.
func TestRenderFittedPaneOverlayReturnsUnclippedWhenContentFits(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 60, 20)

	var lastW, lastH int
	setSize := func(w, hh int) { lastW, lastH = w, hh }
	render := func() string {
		if lastH < 1 {
			lastH = 1
		}
		if lastW < 1 {
			lastW = 1
		}
		lines := make([]string, lastH)
		row := strings.Repeat("x", lastW)
		for i := range lines {
			lines[i] = row
		}
		return strings.Join(lines, "\n")
	}

	view := renderUnderHangGuard(t, h, layout.Rect{W: 24, H: 6}, setSize, render, 5*time.Second)

	require.Less(t, lipgloss.Height(view), 20,
		"fitting content must return the dialog unclipped (shorter than the terminal), not clamped to the full terminal height")
	require.LessOrEqual(t, layout.BlockWidth(view), 60,
		"the unclipped dialog must not exceed the terminal width")
}

// TestRenderFittedPaneOverlayFitsOnBothAxesFloored pins the pre-existing both-axes
// degenerate escape still holds: when both r.W and r.H reach 1 against oversized
// non-shrinking content, the original `r.W == 1 && r.H == 1` exit fires and clamps.
// The no-progress guard must not interfere with that path.
func TestRenderFittedPaneOverlayFitsOnBothAxesFloored(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 5, 3)
	content := strings.Repeat("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", 30)

	view := renderUnderHangGuard(t, h,
		layout.Rect{W: 4, H: 4},
		func(int, int) {},
		func() string { return content },
		5*time.Second,
	)

	require.Equal(t, 3, lipgloss.Height(view),
		"both-axes-floored oversized content must clamp to the terminal height")
	require.LessOrEqual(t, layout.BlockWidth(view), 5,
		"both-axes-floored oversized content must clamp within the terminal width")
}
