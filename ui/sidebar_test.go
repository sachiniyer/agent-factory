package ui

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSidebarInitialState(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	// Should have 3 sections
	assert.Equal(t, 3, len(s.sections))

	// Only Instances section is expanded by default
	assert.True(t, s.sections[0].Expanded)
	assert.False(t, s.sections[1].Expanded)
	assert.False(t, s.sections[2].Expanded)

	// Initial selection should be on Instances header
	sel := s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)
}

func TestSidebarNavigation(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	// Add some instances
	inst1, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst1", Path: t.TempDir(), Program: "test",
	})
	inst2, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst2", Path: t.TempDir(), Program: "test",
	})
	s.AddInstance(inst1)
	s.AddInstance(inst2)

	// Start on Instances header
	sel := s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)

	// Move down into instances
	s.Down()
	sel = s.GetSelection()
	assert.False(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)
	assert.Equal(t, 0, sel.ItemIndex)

	s.Down()
	sel = s.GetSelection()
	assert.Equal(t, 1, sel.ItemIndex)

	// Move down to Tasks header
	s.Down()
	sel = s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionTasks, sel.Kind)

	// Move down to Hooks header
	s.Down()
	sel = s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionHooks, sel.Kind)

	// Move back up
	s.Up()
	sel = s.GetSelection()
	assert.Equal(t, SectionTasks, sel.Kind)
}

func TestSidebarExpandCollapse(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst", Path: t.TempDir(), Program: "test",
	})
	s.AddInstance(inst)

	// Initially Instances is expanded, so we should see header + 1 instance + other headers
	initialCount := len(s.visibleItems)

	// Collapse Instances (should be on Instances header)
	s.CollapseSection()
	assert.Less(t, len(s.visibleItems), initialCount)

	// Verify instance is hidden
	sel := s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)

	// Expand again
	s.ExpandSection()
	assert.Equal(t, initialCount, len(s.visibleItems))
}

func TestSidebarToggleSection(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	// Toggle Instances (starts expanded)
	s.ToggleSection()
	assert.False(t, s.sections[0].Expanded)

	s.ToggleSection()
	assert.True(t, s.sections[0].Expanded)
}

func TestSidebarJumpSections(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst", Path: t.TempDir(), Program: "test",
	})
	s.AddInstance(inst)

	// Start on Instances header
	sel := s.GetSelection()
	assert.Equal(t, SectionInstances, sel.Kind)

	// Jump to next section
	s.JumpNextSection()
	sel = s.GetSelection()
	assert.Equal(t, SectionTasks, sel.Kind)

	s.JumpNextSection()
	sel = s.GetSelection()
	assert.Equal(t, SectionHooks, sel.Kind)

	// Jump back
	s.JumpPrevSection()
	sel = s.GetSelection()
	assert.Equal(t, SectionTasks, sel.Kind)
}

func TestSidebarCollapseFromChild(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst", Path: t.TempDir(), Program: "test",
	})
	s.AddInstance(inst)

	// Navigate to instance child
	s.Down()
	sel := s.GetSelection()
	assert.False(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)

	// Collapse from child should jump to parent header
	s.CollapseSection()
	sel = s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)
}

func TestSidebarInstanceManagement(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	assert.Equal(t, 0, s.NumInstances())

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "test", Path: t.TempDir(), Program: "test",
	})
	s.AddInstance(inst)
	assert.Equal(t, 1, s.NumInstances())

	instances := s.GetInstances()
	assert.Len(t, instances, 1)
	assert.Equal(t, "test", instances[0].Title)
}

// goneSetPreviewSizeBackend wraps FakeBackend so SetPreviewSize emulates the
// state after `tmux kill-session`: the underlying tmux/PTY layer returns
// ErrSessionGone. Used to verify the sidebar's SetSessionPreviewSize swallows
// the sentinel rather than spamming ERROR (#496).
type goneSetPreviewSizeBackend struct {
	*session.FakeBackend
}

func (b *goneSetPreviewSizeBackend) SetPreviewSize(*session.Instance, int, int) error {
	return tmux.ErrSessionGone
}

// TestSidebarSetSessionPreviewSizeSkipsErrSessionGone is the #496 regression:
// when an instance's tmux session has vanished, item.SetPreviewSize returns
// ErrSessionGone, and the sidebar wrapper must skip that instance silently —
// not surface it as a returned error that the caller (app.go:226) would log
// at ERROR on every window-resize.
func TestSidebarSetSessionPreviewSizeSkipsErrSessionGone(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "dead", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(&goneSetPreviewSizeBackend{FakeBackend: session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	s.AddInstance(inst)

	require.NoError(t, s.SetSessionPreviewSize(80, 24),
		"ErrSessionGone from a single instance must not propagate as a returned error")
}

func TestSidebarSelectInstance(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	inst1, _ := session.NewInstance(session.InstanceOptions{
		Title: "first", Path: t.TempDir(), Program: "test",
	})
	inst2, _ := session.NewInstance(session.InstanceOptions{
		Title: "second", Path: t.TempDir(), Program: "test",
	})
	s.AddInstance(inst1)
	s.AddInstance(inst2)

	s.SetSelectedInstance(1)
	selected := s.GetSelectedInstance()
	require.NotNil(t, selected)
	assert.Equal(t, "second", selected.Title)

	s.SelectInstance(inst1)
	selected = s.GetSelectedInstance()
	require.NotNil(t, selected)
	assert.Equal(t, "first", selected.Title)
}

// TestSetSelectedInstanceExpandsCollapsedSection verifies that calling
// SetSelectedInstance while the Instances section is collapsed transparently
// expands the section and selects the target instance (regression for #275).
func TestSetSelectedInstanceExpandsCollapsedSection(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	inst1, _ := session.NewInstance(session.InstanceOptions{
		Title: "first", Path: t.TempDir(), Program: "test",
	})
	inst2, _ := session.NewInstance(session.InstanceOptions{
		Title: "second", Path: t.TempDir(), Program: "test",
	})
	s.AddInstance(inst1)
	s.AddInstance(inst2)

	// Sanity check: SetSelectedInstance works when expanded.
	s.SetSelectedInstance(1)
	selected := s.GetSelectedInstance()
	require.NotNil(t, selected)
	assert.Equal(t, "second", selected.Title)

	// Collapse the Instances section; selection lands on the Instances header.
	s.CollapseSection()
	sel := s.GetSelection()
	require.True(t, sel.IsHeader)
	require.Equal(t, SectionInstances, sel.Kind)

	// Selecting while collapsed should transparently expand the section and
	// land on the requested instance instead of silently no-oping.
	s.SetSelectedInstance(0)
	selected = s.GetSelectedInstance()
	require.NotNil(t, selected, "SetSelectedInstance should expand the collapsed Instances section")
	assert.Equal(t, "first", selected.Title)

	// The Instances section should now be expanded.
	assert.True(t, s.sections[0].Expanded)
}

func TestSidebarTaskData(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)

	tasks := []task.Task{
		{ID: "1", Prompt: "backup", CronExpr: "0 0 * * *", Enabled: true, CreatedAt: time.Now()},
		{ID: "2", Prompt: "health check", CronExpr: "*/5 * * * *", Enabled: false, CreatedAt: time.Now()},
	}
	s.SetTasks(tasks)

	result := s.GetTasks()
	assert.Len(t, result, 2)
}

func TestSidebarRender(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)
	s.SetSize(40, 20)

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "my-feature", Path: t.TempDir(), Program: "test",
	})
	s.AddInstance(inst)

	rendered := s.String()
	assert.Contains(t, rendered, "Instances (1)")
	assert.NotEmpty(t, rendered)
}

// indicatorArrows reports which "▲/▼ N more" scroll-indicator rows are present
// in the rendered output. Detection keys on the "more" text because expanded
// section headers also use a "▼ " arrow.
func indicatorArrows(out string) (up, down bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "more") {
			continue
		}
		if strings.Contains(line, "▲") {
			up = true
		}
		if strings.Contains(line, "▼") {
			down = true
		}
	}
	return up, down
}

// newWindowingSidebar builds a sidebar with n instances titled win-00..win-NN
// for the #787 windowing tests.
func newWindowingSidebar(t *testing.T, n int) *Sidebar {
	t.Helper()
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false)
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: fmt.Sprintf("win-%02d", i), Path: dir, Program: "test",
		})
		require.NoError(t, err)
		s.AddInstance(inst)
	}
	return s
}

// TestSidebarWindowsLongInstanceListToAllocation is the regression test for
// #787: lipgloss.Place pads but never truncates, so before windowing 25
// instances at a 20-line allocation rendered ~100 lines and pushed the menu
// and error box below the fold. The sidebar must render exactly its allocated
// height regardless of instance count, with the selected row always inside
// the rendered window.
func TestSidebarWindowsLongInstanceListToAllocation(t *testing.T) {
	const w, h = 40, 20

	cases := []struct {
		name    string
		sel     func(s *Sidebar)
		visible string
	}{
		{"top (Instances header)", func(s *Sidebar) {}, "Instances (25)"},
		{"middle instance", func(s *Sidebar) { s.SetSelectedInstance(12) }, "win-12"},
		{"bottom instance", func(s *Sidebar) { s.SetSelectedInstance(24) }, "win-24"},
		{"trailing Hooks header", func(s *Sidebar) {
			s.SetSelectedInstance(24)
			s.Down() // Tasks header
			s.Down() // Hooks header
		}, "Hooks"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newWindowingSidebar(t, 25)
			s.SetSize(w, h)
			tc.sel(s)

			out := s.String()
			require.Equal(t, h, renderedLineCount(out),
				"sidebar must render exactly the allocated height")
			assert.Contains(t, out, tc.visible,
				"selected row must be inside the rendered window")
		})
	}
}

// TestSidebarWindowScrollsWithSelection walks the selection from the top of
// the list to the bottom and back. At every step the rendered output must
// stay at the allocated height and contain the selected row, and the ▲/▼
// indicators must only appear when rows are actually hidden on that side.
func TestSidebarWindowScrollsWithSelection(t *testing.T) {
	const w, h = 40, 20

	s := newWindowingSidebar(t, 25)
	s.SetSize(w, h)

	check := func(step string) {
		out := s.String()
		require.Equal(t, h, renderedLineCount(out), "%s: height must stay at the allocation", step)
		if inst := s.GetSelectedInstance(); inst != nil {
			assert.Contains(t, out, inst.Title, "%s: selected instance must be visible", step)
		}
	}

	// Walk down through every row (1 header + 25 instances + 2 headers).
	check("initial")
	for i := 0; i < len(s.visibleItems)-1; i++ {
		s.Down()
		check(fmt.Sprintf("down %d", i))
	}
	// At the very bottom nothing is hidden below, so only ▲ may show.
	up, down := indicatorArrows(s.String())
	assert.True(t, up, "rows above must be indicated at the bottom")
	assert.False(t, down, "nothing is hidden below at the bottom")

	// Walk back up to the top.
	for i := 0; i < len(s.visibleItems)-1; i++ {
		s.Up()
		check(fmt.Sprintf("up %d", i))
	}
	top := s.String()
	assert.Contains(t, top, "win-00", "first instance must be visible at the top")
	up, down = indicatorArrows(top)
	assert.False(t, up, "nothing is hidden above at the top")
	assert.True(t, down, "rows below must be indicated at the top")
}

// TestSidebarShortListRendersUnwindowed verifies the fast path: when the list
// fits the allocation the sidebar renders as before — every row visible, no
// scroll indicators, output padded to exactly the allocated height.
func TestSidebarShortListRendersUnwindowed(t *testing.T) {
	const w, h = 40, 30

	s := newWindowingSidebar(t, 2)
	s.SetSize(w, h)

	out := s.String()
	require.Equal(t, h, renderedLineCount(out))
	assert.Contains(t, out, "win-00")
	assert.Contains(t, out, "win-01")
	assert.NotContains(t, out, "more", "short lists must not show scroll indicators")
}

// renderForTerminal renders an instance at the sidebar width app.go derives
// from the given terminal width, and returns the rendered title and PR lines.
// The renderer wraps each section in lipgloss padding, so the visible title
// content sits on line 1 (after the top-padding line) and the PR content
// sits on line 4 (title content + branch + branch-pad + pr content).
func renderForTerminal(t *testing.T, terminalW int, inst *session.Instance, spin *spinner.Model) (titleLine string, prLine string, sidebarW int) {
	t.Helper()
	sidebarW = int(float32(terminalW) * 0.3)
	r := &InstanceRenderer{spinner: spin}
	r.setWidth(sidebarW)
	out := r.Render(inst, 1, false, false)
	lines := strings.Split(out, "\n")
	require.GreaterOrEqual(t, len(lines), 2, "renderer should emit at least a title row")
	titleLine = lines[1]
	if len(lines) >= 5 {
		prLine = lines[4]
	}
	return titleLine, prLine, sidebarW
}

// TestInstanceRendererNarrowTerminalNoOverflow guards against the regression
// reported in #466: at 40-43 column terminal widths the sidebar's instance
// row ended with a "..." artifact that pushed the rendered line one cell past
// the sidebar container width.
func TestInstanceRendererNarrowTerminalNoOverflow(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "long-feature",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	cases := []struct {
		name              string
		terminalW         int
		expectFullTitle   bool
		expectEllipsis    bool
		expectNoOverflow  bool
		expectNoTitleTail bool
	}{
		// Plenty of room — full title, no truncation.
		{name: "width80", terminalW: 80, expectFullTitle: true, expectNoOverflow: true},
		// Some truncation, room for the ellipsis.
		{name: "width50", terminalW: 50, expectEllipsis: true, expectNoOverflow: true},
		// Bug range: widthAvail is positive but less than the 3-cell ellipsis.
		// The fix must drop the tail rather than render a "..." artifact.
		{name: "width43", terminalW: 43, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width42", terminalW: 42, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width41", terminalW: 41, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width40", terminalW: 40, expectNoOverflow: true, expectNoTitleTail: true},
		// Bug range from #646: widthAvail goes non-positive so the
		// truncation block used to be skipped entirely and the rendered
		// row spilled past sidebarW. Sweep 30..39 inclusive — every row
		// must fit within the sidebar container width and must not leave
		// a stray "..." artifact.
		{name: "width39", terminalW: 39, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width38", terminalW: 38, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width37", terminalW: 37, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width36", terminalW: 36, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width35", terminalW: 35, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width34", terminalW: 34, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width33", terminalW: 33, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width32", terminalW: 32, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width31", terminalW: 31, expectNoOverflow: true, expectNoTitleTail: true},
		{name: "width30", terminalW: 30, expectNoOverflow: true, expectNoTitleTail: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			titleLine, _, sidebarW := renderForTerminal(t, tc.terminalW, inst, &spin)
			w := lipgloss.Width(titleLine)

			if tc.expectFullTitle {
				assert.Contains(t, titleLine, inst.Title, "wide terminal should render the full title")
				assert.NotContains(t, titleLine, "...", "wide terminal should not truncate")
			}
			if tc.expectEllipsis {
				assert.Contains(t, titleLine, "...", "title should be truncated with ellipsis when there is room for it")
			}
			if tc.expectNoOverflow {
				assert.LessOrEqualf(t, w, sidebarW,
					"title line width (%d) must fit within sidebar container width (%d) at terminal=%d",
					w, sidebarW, tc.terminalW)
			}
			if tc.expectNoTitleTail {
				// Strip trailing padding, then the visible title text must
				// not end with a stray ellipsis from a negative-width
				// runewidth.Truncate call.
				trimmed := strings.TrimRight(titleLine, " ")
				assert.Falsef(t, strings.HasSuffix(trimmed, "..."),
					"narrow terminal must not produce a '...' artifact; got %q", titleLine)
			}
		})
	}
}

// TestInstanceRendererNarrowTerminalPRNoTail exercises the parallel
// truncation site for PR text: when prMaxWidth drops below the 3-cell
// ellipsis the row must drop the tail rather than render a "..." that
// overflows the sidebar.
func TestInstanceRendererNarrowTerminalPRNoTail(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "feat",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	inst.SetPRInfo(&git.PRInfo{
		Number: 1234,
		Title:  "long pull request title needing truncation",
		State:  "OPEN",
	})

	// terminalW=28..32 produces prMaxWidth in {1, 2}, which is the bug
	// range where the pre-fix code passed a negative width to
	// runewidth.Truncate and got back "..." (wider than prMaxWidth).
	for _, terminalW := range []int{28, 30, 32} {
		_, prLine, _ := renderForTerminal(t, terminalW, inst, &spin)
		trimmed := strings.TrimRight(prLine, " ")
		assert.Falsef(t, strings.HasSuffix(trimmed, "..."),
			"PR line must not produce a '...' artifact at terminal=%d; got %q",
			terminalW, prLine)
	}
}
