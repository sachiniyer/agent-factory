package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"

	"github.com/stretchr/testify/require"
)

// enterScrollOver returns a TabPane in scroll/copy mode holding histStr as its
// filled scrollback, scrolled up one row so the scroll branch (not the loading
// spinner) is what String() renders. It shares the set-up shape of the
// off-loop scroll tests: a mocked shell instance, a host-history source, and a
// dispatched-then-completed fill.
func enterScrollOver(t *testing.T, name, histStr string, w, h int) (*TabPane, *session.Instance) {
	t.Helper()
	inst := makeShellInstance(t, name, "ignored")
	src := func(_ *session.Instance, _ int, _ bool) (PreviewSnapshot, error) {
		return hostPreview(histStr), nil
	}
	p := NewTabPane(src)
	p.SetSize(w, h)
	enableHostHistory(p, inst, 1)
	require.NoError(t, p.ScrollUp(inst, 1))
	p.BeginScrollFill()
	require.NoError(t, p.UpdateContent(inst, 1))
	return p, inst
}

func TestScrollModeWideLineClipIsSilent(t *testing.T) {
	const w, h = 20, 4
	inst := makeShellInstance(t, "wide", "ignored")
	defer func() { _ = inst.Kill() }()

	wideLine := strings.Repeat("X", 100)
	histStr := strings.Join([]string{
		wideLine, wideLine, wideLine, wideLine,
		wideLine, wideLine, wideLine, wideLine,
	}, "\n")
	src := func(_ *session.Instance, _ int, _ bool) (PreviewSnapshot, error) {
		return hostPreview(histStr), nil
	}
	p := NewTabPane(src)
	p.SetSize(w, h)

	// Normal (capture) mode: the #4175 marker is present on the wide rows.
	require.NoError(t, p.UpdateContent(inst, 1))
	require.Contains(t, p.String(), "…",
		"normal mode marks a wide-row clip with an ellipsis (#4175)")

	// Scroll (copy) mode over the SAME wide history must show the same clip
	// marker. The scroll branch renders through viewport.View(), whose own
	// MaxWidth truncates wide rows with an empty tail, so it must render with
	// an unbounded content width and run the same per-line fitLine pass the
	// normal-mode branch uses (#4175).
	enableHostHistory(p, inst, 1)
	require.NoError(t, p.ScrollUp(inst, 1))
	p.BeginScrollFill()
	require.NoError(t, p.UpdateContent(inst, 1))
	require.Contains(t, p.String(), "…",
		"scroll mode must mark a wide-row clip with an ellipsis like normal mode (#4175)")
}

// TestScrollModeClipNoMarkerForFittingContent guards against a false positive:
// rows that already fit the pane must NOT get a … in scroll mode. Rendering
// with an unbounded content width only makes the raw rows visible; the fitLine
// pass must still return them unchanged when their measured width is <= p.width.
func TestScrollModeClipNoMarkerForFittingContent(t *testing.T) {
	const w, h = 20, 4
	histStr := strings.Join([]string{
		"history-aaa", "history-bbb", "history-ccc", "history-ddd",
		"history-eee", "history-fff", "history-ggg", "history-hhh",
	}, "\n")
	p, inst := enterScrollOver(t, "fits", histStr, w, h)
	defer func() { _ = inst.Kill() }()

	rendered := p.String()
	require.NotContains(t, rendered, "…",
		"a row that fits the pane must not get a clip marker in scroll mode")
	for _, line := range strings.Split(rendered, "\n") {
		require.Equal(t, w, lipgloss.Width(line),
			"scroll-mode rows must still be padded to exactly the pane width")
	}
}

// TestScrollModeClipNoMarkerForTrailingPadding mirrors the normal-mode branch's
// TrimRight: tmux capture pads every row to the window width with trailing
// spaces, so a row whose real content fits but whose blank-space tail pushes it
// past the pane width must NOT get a … — the cut drops only blank space. Without
// the TrimRight the fitLine raw measure would mark the blank tail as lost content.
func TestScrollModeClipNoMarkerForTrailingPadding(t *testing.T) {
	const w, h = 20, 4
	// "hello" (5 cells) padded to 80 cells with trailing spaces — exactly what a
	// capture-pane row looks like for a short prompt in an 80-col window.
	row := "hello" + strings.Repeat(" ", 75)
	histStr := strings.Join([]string{row, row, row, row, row, row, row, row}, "\n")
	p, inst := enterScrollOver(t, "padded", histStr, w, h)
	defer func() { _ = inst.Kill() }()

	rendered := p.String()
	require.NotContains(t, rendered, "…",
		"a row whose overflow is only trailing padding must not get a clip marker")
	require.Contains(t, rendered, "hello",
		"the real leading content must still be rendered")
}

// TestScrollModeClipRespectsRect verifies the rectangle contract survives the
// redrawn scroll branch: String() is exactly p.height lines, each exactly
// p.width cells, even with wide rows in the viewport (no wrap onto extra rows,
// no overflow past the pane width) — the original ClampToRect guarantee.
func TestScrollModeClipRespectsRect(t *testing.T) {
	const w, h = 20, 4
	wideLine := strings.Repeat("Y", 100)
	histStr := strings.Join([]string{
		wideLine, wideLine, wideLine, wideLine,
		wideLine, wideLine, wideLine, wideLine,
	}, "\n")
	p, inst := enterScrollOver(t, "rect", histStr, w, h)
	defer func() { _ = inst.Kill() }()

	rendered := p.String()
	lines := strings.Split(rendered, "\n")
	require.Len(t, lines, h, "scroll mode must render exactly p.height rows, not wrap")
	for i, line := range lines {
		require.Equalf(t, w, lipgloss.Width(line),
			"row %d must be exactly the pane width after the clip+pad pass", i)
	}
	// Every clipped row keeps its leading columns and gains the edge marker.
	stripped := strings.ReplaceAll(rendered, "\x1b[0m", "")
	require.Contains(t, stripped, strings.Repeat("Y", w-1)+"…",
		"each wide row must keep its leading columns and end with the clip marker")
}

// TestScrollModeClipPreservesScrollGeometry guards the temporary viewport
// mutation the fix uses to bypass MaxWidth: rendering String() must not move the
// scroll position or leak the unbounded width back into the viewport. A further
// ScrollUp must still advance the same visible window, and the restored width
// must equal the pane width so the next resize/scroll measures correctly.
func TestScrollModeClipPreservesScrollGeometry(t *testing.T) {
	const w, h = 20, 4
	// Eight indexed rows so a one-row scroll reveals a different oldest line.
	rows := make([]string, 8)
	for i := range rows {
		rows[i] = "row-" + string(rune('0'+i)) + strings.Repeat("Z", 95) // wide on purpose
	}
	histStr := strings.Join(rows, "\n")
	p, inst := enterScrollOver(t, "geom", histStr, w, h)
	defer func() { _ = inst.Kill() }()

	p.mu.Lock()
	yBefore := p.viewport.YOffset
	widthBefore := p.viewport.Width
	p.mu.Unlock()
	require.Equal(t, w, widthBefore, "after fill the viewport width must match the pane width")

	// Rendering must be a pure read: it must not move the scroll position or
	// leave the viewport pinned at width 0 (the unbounded render width).
	_ = p.String()
	_ = p.String()
	p.mu.Lock()
	yAfter := p.viewport.YOffset
	widthAfter := p.viewport.Width
	p.mu.Unlock()
	require.Equal(t, yBefore, yAfter, "String() must not move the scroll offset")
	require.Equal(t, w, widthAfter, "String() must restore the viewport width")

	// A further scroll must still reveal an older row — the marker pass did
	// not corrupt the viewport's content or scroll bookkeeping.
	require.NoError(t, p.ScrollUp(inst, 1))
	require.Contains(t, p.String(), "row-2",
		"a further ScrollUp must reveal the next older wide row")
}

// TestScrollModeClipReAppliesAfterResize verifies that resizing the pane while in
// scroll mode re-runs the fitLine pass at the NEW width. A row wider than the
// new width still gets …; a row that now fits loses the marker. SetSize routes
// through scroll.Resize, which preserves distance-from-bottom for a ready
// history viewport, so the same wide rows stay visible across the resize.
func TestScrollModeClipReAppliesAfterResize(t *testing.T) {
	const w, h = 20, 4
	wideRow := strings.Repeat("Q", 100) // 5x the starting pane width
	histStr := strings.Join([]string{
		wideRow, wideRow, wideRow, wideRow,
		wideRow, wideRow, wideRow, wideRow,
	}, "\n")
	p, inst := enterScrollOver(t, "resize", histStr, w, h)
	defer func() { _ = inst.Kill() }()

	t.Run("still_wider_than_pane", func(t *testing.T) {
		const newW = 40 // < 100: rows still clipped
		p.SetSize(newW, h)
		require.NoError(t, p.ScrollUp(inst, 1))
		rendered := p.String()
		require.Contains(t, rendered, "…",
			"a row wider than the resized pane width must keep the clip marker")
		for i, line := range strings.Split(rendered, "\n") {
			require.Equalf(t, newW, lipgloss.Width(line),
				"row %d must be exactly the resized pane width", i)
		}
	})

	t.Run("now_fits_pane", func(t *testing.T) {
		const newW = 120 // >= 100: rows now fit, no clip
		p.SetSize(newW, h)
		require.NoError(t, p.ScrollUp(inst, 1))
		rendered := p.String()
		require.NotContains(t, rendered, "…",
			"a row that fits the resized pane must not get a clip marker")
	})
}

// TestScrollModeClipEmptyHistory verifies an empty scrollback renders cleanly —
// exactly p.width × p.height, no panic, no spurious marker. enterScrollOver
// completes the fill so the scroll branch is the active render path even for
// empty content.
func TestScrollModeClipEmptyHistory(t *testing.T) {
	const w, h = 20, 4
	p, inst := enterScrollOver(t, "empty", "", w, h)
	defer func() { _ = inst.Kill() }()

	rendered := p.String()
	require.NotContains(t, rendered, "…",
		"empty scrollback must not get a spurious clip marker")
	lines := strings.Split(rendered, "\n")
	require.Len(t, lines, h, "empty scrollback still renders exactly p.height rows")
	for i, line := range lines {
		require.Equalf(t, w, lipgloss.Width(line),
			"row %d must be padded to exactly p.width for empty scrollback", i)
	}
}

// TestScrollModeClipSurvivesWindowReClamp verifies the marker survives the
// upstream re-clamp: TabbedWindow.String() re-clamps w.tab.String() with
// ClampToRect (no marker pass of its own), so the … produced by TabPane must
// survive the window-level re-clamp and the framed pane must still render
// exactly its Rect with wide scroll content.
func TestScrollModeClipSurvivesWindowReClamp(t *testing.T) {
	const w, h = 30, 10
	inst := makeShellInstance(t, "win-reclamp", "ignored")
	defer func() { _ = inst.Kill() }()

	wideRow := strings.Repeat("W", 100)
	histStr := strings.Join([]string{
		wideRow, wideRow, wideRow, wideRow,
		wideRow, wideRow, wideRow, wideRow,
	}, "\n")
	src := func(_ *session.Instance, _ int, _ bool) (PreviewSnapshot, error) {
		return hostPreview(histStr), nil
	}
	pane := NewTabPane(src)
	tw := NewTabbedWindow(pane, nil)
	r := layout.Rect{W: w, H: h}
	tw.SetRect(r)

	enableHostHistory(pane, inst, 1)
	require.NoError(t, pane.ScrollUp(inst, 1))
	pane.BeginScrollFill()
	require.NoError(t, pane.UpdateContent(inst, 1))
	require.True(t, tw.IsInScrollMode(), "window must be in scroll mode after fill")

	view := tw.View()
	requireExactRect(t, view, r, "window with wide scroll content")
	require.Contains(t, xansi.Strip(view), "…",
		"the ellipsis clip marker must survive the window-level ClampToRect re-clamp")
}
