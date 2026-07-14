package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
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
// vertical-centering padding. (TabPane.String space-pads everything to the
// exact rect since #1024 PR 4, so this helper inspects renderCenteredFallback
// output directly.)
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
		p := NewTabPane(previewFromInstance)
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
		p := NewTabPane(previewFromInstance)
		p.SetSize(tc.w, tc.h)
		p.setFallbackState("msg")
		require.Contains(t, p.String(), "msg",
			"%dx%d: message must be visible when the box is tall enough", tc.w, tc.h)

		// Inspect the centering math pre-clamp: the final exact-rect clamp
		// space-pads every line, which would hide which lines are padding.
		out := renderCenteredFallback(tabPaneStyle, p.content.text, tc.w, tc.h)
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
		fb := NewTabPane(previewFromInstance)
		fb.SetSize(80, h)
		fb.setFallbackState("Select an instance to open a terminal")
		require.Equal(t, h, renderedLineCount(fb.String()),
			"height=%d: fallback must render exactly the allocated height", h)

		normal := NewTabPane(previewFromInstance)
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
		tp := NewTabPane(previewFromInstance)
		tp.SetSize(tc.w, tc.h)
		tp.setFallbackState("msg")
		require.Equal(t, tc.h, renderedLineCount(tp.String()),
			"%dx%d: fallback must render exactly the allocated height", tc.w, tc.h)
	}
}

// requireExactRect asserts a pane's output honors the layout.Pane contract
// (#1024 PR 4): exactly r.H lines, every line exactly r.W cells. This is the
// shared enforcement the RFC (§5.5) asks every pane to pass — the successor
// of the per-pane #700/#821 allocation regressions.
func requireExactRect(t *testing.T, out string, r layout.Rect, name string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	require.Equalf(t, r.H, len(lines), "%s: must render exactly rect.H lines", name)
	for i, line := range lines {
		require.Equalf(t, r.W, lipgloss.Width(line), "%s: line %d must be exactly rect.W cells", name, i)
	}
}

// newTestWorkspace builds one of each workspace pane over a fresh projection.
// The content window is unbound (nil pane): the rect/clamp contract under test
// is binding-independent.
func newTestWorkspace() (*Sidebar, *TabbedWindow, *AutomationsPane, *StatusBar) {
	proj := store.NewProjection()
	sidebar := NewSidebar(false, proj)
	paneA := NewTabbedWindow(NewTabPane(previewFromInstance), nil)
	automations := NewAutomationsPane(proj)
	statusBar := NewStatusBar(NewMenu(), NewErrBox())
	return sidebar, paneA, automations, statusBar
}

// TestWorkspacePanesRenderExactlyTheirRects drives every pane through
// layout.Grid at the RFC's key sizes — including 80×24 and the degradation
// thresholds — with over-tall and over-wide content loaded, and asserts each
// visible region renders exactly its rect. Composed row-by-row, the regions
// therefore tile the full window: the property the pre-cutover layout had to
// re-earn per-pane (#700, #786, #821 class).
func TestWorkspacePanesRenderExactlyTheirRects(t *testing.T) {
	longName := strings.Repeat("long-task-name-", 8)
	var tasks []task.Task
	for i := 0; i < 40; i++ {
		tasks = append(tasks, task.Task{ID: "t", Name: longName, Prompt: "p", CronExpr: "0 3 * * *"})
	}

	for _, tc := range []struct{ w, h int }{
		{120, 40}, // roomy
		{80, 24},  // the RFC's canonical small terminal
		{79, 24},  // below AutomationsFullMinWidth: 1-line strip
		{60, 15},  // exactly the minimal-mode threshold
		{59, 15},  // minimal mode: no automations strip
		{40, 10},  // exactly the hard minimum
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.w, tc.h), func(t *testing.T) {
			lay := layout.Grid{Panes: 1}.Solve(tc.w, tc.h)
			require.False(t, lay.Fallback)

			sidebar, paneA, automations, statusBar := newTestWorkspace()
			automations.proj.SetTasks(tasks)
			paneA.tab.content = tabContentState{text: strings.Repeat(strings.Repeat("wide ", 100)+"\n", 100)}

			sidebar.SetRect(lay.Tree)
			paneA.SetRect(lay.Panes[0])
			statusBar.SetRect(lay.StatusBar)

			requireExactRect(t, sidebar.View(), lay.Tree, "tree")
			requireExactRect(t, paneA.View(), lay.Panes[0], "paneA")
			requireExactRect(t, statusBar.View(), lay.StatusBar, "statusBar")

			// The composed workspace must be exactly the terminal size. The
			// left rail stacks tree + rule + automations (#1087); the
			// workspace pane runs the full height beside it (#1090).
			rail := sidebar.View()
			if lay.AutomationsVisible {
				automations.SetRect(lay.Automations)
				automations.SetCompact(lay.AutomationsCompact)
				requireExactRect(t, automations.View(), lay.Automations, "automations")
				rule := strings.Repeat("─", lay.RailRule.W)
				rail = lipgloss.JoinVertical(lipgloss.Left, rail, rule, automations.View())
			}
			top := lipgloss.JoinHorizontal(lipgloss.Top, rail, paneA.View())
			full := lipgloss.JoinVertical(lipgloss.Left, top, statusBar.View())
			requireExactRect(t, full, layout.Rect{W: tc.w, H: tc.h}, "composed workspace")
		})
	}
}

// TestWorkspaceFocusedAutomationsTilesExactly covers the focused automations
// section: focus adds a cursor that expands its row's detail inline (#1126) —
// the manager itself stays a modal overlay, never rendered in-rail — and the
// composed workspace must still tile the window exactly (#1087).
func TestWorkspaceFocusedAutomationsTilesExactly(t *testing.T) {
	lay := layout.Grid{Panes: 1}.Solve(100, 30)
	require.True(t, lay.AutomationsVisible)
	require.False(t, lay.AutomationsCompact)

	sidebar, paneA, automations, statusBar := newTestWorkspace()
	automations.proj.SetTasks([]task.Task{{ID: "t", Name: "nightly", Prompt: "p", CronExpr: "0 3 * * *"}})
	automations.Focus()

	sidebar.SetRect(lay.Tree)
	paneA.SetRect(lay.Panes[0])
	automations.SetRect(lay.Automations)
	automations.SetCompact(lay.AutomationsCompact)
	statusBar.SetRect(lay.StatusBar)

	requireExactRect(t, automations.View(), lay.Automations, "focused automations")
	require.Contains(t, automations.View(), "▾", "the focused section carries an expanded cursor")
	require.NotContains(t, automations.View(), "Tasks",
		"the manager must NOT render in-rail — it lives in the tasks overlay")

	rule := strings.Repeat("─", lay.RailRule.W)
	rail := lipgloss.JoinVertical(lipgloss.Left, sidebar.View(), rule, automations.View())
	top := lipgloss.JoinHorizontal(lipgloss.Top, rail, paneA.View())
	full := lipgloss.JoinVertical(lipgloss.Left, top, statusBar.View())
	requireExactRect(t, full, layout.Rect{W: 100, H: 30}, "composed workspace")
}

// TestTerminalTooSmallBannerIsExactlyTerminalSized covers the below-hard-
// minimum fallback: the banner alone fills the window.
func TestTerminalTooSmallBannerIsExactlyTerminalSized(t *testing.T) {
	for _, tc := range []struct{ w, h int }{
		{39, 10}, // below hard min width
		{40, 9},  // below hard min height
		{20, 5},
	} {
		lay := layout.Grid{}.Solve(tc.w, tc.h)
		require.True(t, lay.Fallback, "%dx%d must be fallback", tc.w, tc.h)
		out := TerminalTooSmall(tc.w, tc.h)
		requireExactRect(t, out, layout.Rect{W: tc.w, H: tc.h}, "banner")
		require.Contains(t, out, "Terminal too small")
	}
}

// TestWorkspacePanesImplementPaneContract drives every workspace pane through
// the layout.Pane interface itself: rect, focus flags, key/mouse stubs, and
// the exact-rect View. This is the shared §5.5 contract check run against
// every pane.
func TestWorkspacePanesImplementPaneContract(t *testing.T) {
	sidebar, paneA, automations, statusBar := newTestWorkspace()
	r := layout.Rect{X: 0, Y: 0, W: 50, H: 12}
	for name, p := range map[string]layout.Pane{
		"tree":        sidebar,
		"paneA":       paneA,
		"automations": automations,
		"statusBar":   statusBar,
	} {
		p.SetRect(r)
		p.Focus()
		p.Blur()
		require.False(t, p.Focused(), "%s: blurred after Blur", name)
		_, _ = p.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
		_ = p.HandleMouse(tea.MouseMsg{}, layout.Point{})
		requireExactRect(t, p.View(), r, name)
	}
}
