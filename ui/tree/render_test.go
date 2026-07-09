package tree

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// effectiveWidth models a sidebar-style content buffer (narrower than the
// allocation) so the renderer tests exercise an effective content width the
// way the sidebar passes one to SetWidth in production. Inlined rather than
// imported: these tests live in package tree, and importing ui (which imports
// tree) would cycle.
func effectiveWidth(width int) int {
	return int(float64(width) * 0.9)
}

// prLineIndent renders an instance at the given 1-based display index and
// returns the number of leading whitespace cells in front of the "PR #" text.
// Instance indices are no longer rendered (#1494), so secondary-row indentation
// must not change as idx crosses power-of-10 boundaries.
func prLineIndent(t *testing.T, idx int, spin *spinner.Model) int {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "feature",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	inst.SetPRInfo(&git.PRInfo{Number: 42, Title: "do a thing", State: "OPEN"})

	r := NewInstanceRenderer(spin)
	r.SetWidth(60) // wide enough to render the full PR line

	out := r.Render(inst, idx, false, false, false)
	for _, line := range strings.Split(out, "\n") {
		clean := ansiEscape.ReplaceAllString(line, "")
		if pos := strings.Index(clean, "PR #"); pos >= 0 {
			return len([]rune(clean[:pos]))
		}
	}
	t.Fatalf("no PR line found in rendered output for idx=%d:\n%s", idx, out)
	return -1
}

// TestInstanceRendererOmitsInstanceIndex guards #1494: the sidebar row may
// still carry tree arrows and state glyphs, but it must not render a leading
// "1. title" / "42. title" instance index.
func TestInstanceRendererOmitsInstanceIndex(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "feature",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	r := NewInstanceRenderer(&spin)
	r.SetWidth(60)
	indexPrefix := regexp.MustCompile(`\b\d+\.\s+feature`)
	for _, idx := range []int{1, 9, 10, 99, 100, 10000} {
		out := r.Render(inst, idx, false, false, false)
		lines := strings.Split(ansiEscape.ReplaceAllString(out, ""), "\n")
		require.GreaterOrEqual(t, len(lines), 2)
		assert.Contains(t, lines[1], "feature")
		assert.NotRegexp(t, indexPrefix, lines[1],
			"instance row rendered the display index at idx=%d", idx)
	}
}

// TestInstanceRendererSecondaryIndentIgnoresIndex keeps the branch/PR
// indentation stable now that idx is display-only input rather than a rendered
// prefix component.
func TestInstanceRendererSecondaryIndentIgnoresIndex(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))

	base := prLineIndent(t, 1, &spin)
	for _, idx := range []int{9, 10, 11, 99, 100, 101, 999, 1000, 1001, 9999, 10000} {
		got := prLineIndent(t, idx, &spin)
		require.Equalf(t, base, got,
			"PR line indent at idx=%d (%d) must match idx=1 (%d); idx leaked into display",
			idx, got, base)
	}
}

// renderForTerminal renders an instance at the sidebar width app.go derives
// from the given terminal width, and returns the rendered title and PR lines.
// The renderer wraps each section in lipgloss padding, so the visible title
// content sits on line 1 (after the top-padding line) and the PR content
// sits on line 4 (title content + branch + branch-pad + pr content).
func renderForTerminal(t *testing.T, terminalW int, inst *session.Instance, spin *spinner.Model) (titleLine string, prLine string, sidebarW int) {
	t.Helper()
	sidebarW = int(float32(terminalW) * 0.3)
	r := NewInstanceRenderer(spin)
	r.SetWidth(effectiveWidth(sidebarW))
	out := r.Render(inst, 1, false, false, false)
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
		// Plenty of room — full title, no truncation. (Wider than the pre-tree
		// test's 80: the ▸/▾ arrow cell costs 2 prefix cells, #1024 PR 3.)
		{name: "width90", terminalW: 90, expectFullTitle: true, expectNoOverflow: true},
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

	// terminalW=14..18 produces prMaxWidth in {1, 2}, which is the bug
	// range where the pre-fix code passed a negative width to
	// runewidth.Truncate and got back "..." (wider than prMaxWidth).
	for _, terminalW := range []int{14, 16, 18} {
		_, prLine, _ := renderForTerminal(t, terminalW, inst, &spin)
		trimmed := strings.TrimRight(prLine, " ")
		assert.Falsef(t, strings.HasSuffix(trimmed, "..."),
			"PR line must not produce a '...' artifact at terminal=%d; got %q",
			terminalW, prLine)
	}
}

// TestInstanceRendererDeletingMarker pins the #844 sidebar treatment: a row
// whose teardown is running in the background must carry an explicit
// "[deleting]" marker (the spinner alone reads as "busy working").
func TestInstanceRendererDeletingMarker(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "going-away",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	inst.SetStatusForTest(session.Ready)
	before, _, _ := renderForTerminal(t, 120, inst, &spin)
	assert.NotContains(t, before, "[deleting]")

	inst.SetStatusForTest(session.Deleting)
	after, _, _ := renderForTerminal(t, 120, inst, &spin)
	assert.Contains(t, after, "[deleting]", "deleting rows must be explicitly marked")
	assert.Contains(t, after, "going-away", "the title must remain visible while deleting")
}

// TestInstanceRendererLimitReachedMarker guards the #1195 exhaustive-render
// requirement for #1146: a LimitReached liveness renders its own explicit marker
// (no silent default / blank dot). #1204 refines the label + reset time on top.
func TestInstanceRendererLimitReachedMarker(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "throttled",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	_ = inst.Transition(session.ObserveLiveness(session.LiveLimitReached))
	out, _, _ := renderForTerminal(t, 120, inst, &spin)
	assert.Contains(t, out, "[limit]", "a usage-limit-reached row must be explicitly marked (#1146)")
	assert.Contains(t, out, "throttled", "the title must remain visible")
}

// TestInstanceRendererArchivedShowsName pins #1225: an archived row must show
// its NAME at the widths users actually run (100/80 cols), not a name-eating
// "[archived] " word prefix that clips every archived session to "[archived]...".
// The archived state is conveyed by the ▧ glyph + dimming + section header, so
// the title cell is spent on the identifier, exactly like a live row.
func TestInstanceRendererArchivedShowsName(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "one",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	inst.SetArchived()

	for _, terminalW := range []int{100, 80} {
		title, _, _ := renderForTerminal(t, terminalW, inst, &spin)
		clean := ansiEscape.ReplaceAllString(title, "")
		assert.Containsf(t, clean, "one",
			"archived row must show its name at %d cols; got %q", terminalW, clean)
		assert.NotContainsf(t, clean, "[archived]",
			"archived row must not carry the name-eating text prefix at %d cols; got %q", terminalW, clean)
	}
}

func TestInstanceRendererCreatingRowLabelsNamePrompt(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "alpha",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(session.BeginCreate()))

	r := NewInstanceRenderer(&spin)
	r.SetWidth(40)

	title := strings.Split(ansiEscape.ReplaceAllString(r.Render(inst, 1, true, false, false), ""), "\n")[1]
	assert.Contains(t, title, "Session name: alpha", "creating rows must label the active name target")
}

// TestInstanceRendererDeletingDimsSelectedRow pins the #853 fix: a SELECTED
// deleting row must dim its branch and PR lines along with the title. Before
// the fix only titleS picked up deletingTitleColor, so the high-contrast
// selectedDescStyle left the secondary lines brighter than the dimmed title.
// (Unselected rows never showed the bug: listDescStyle is already the same
// gray as deletingTitleColor.)
func TestInstanceRendererDeletingDimsSelectedRow(t *testing.T) {
	// Force a real color profile and a fixed background so lipgloss emits the
	// foreground escapes the assertions match on; the Ascii profile used by
	// default in non-TTY test runs strips all styling.
	prevProfile := lipgloss.ColorProfile()
	prevDark := lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(prevProfile)
		lipgloss.SetHasDarkBackground(prevDark)
	})

	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "going-away",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	inst.SetPRInfo(&git.PRInfo{Number: 7, Title: "teardown", State: "OPEN"})

	// SGR foreground params of the default Zenburn deleting gray.
	dimFG := termenv.RGBColor("#989890").Sequence(false)

	r := NewInstanceRenderer(&spin)
	r.SetWidth(effectiveWidth(36))

	renderLines := func() []string {
		out := r.Render(inst, 1, true, false, false)
		lines := strings.Split(out, "\n")
		// [0] title top padding, [1] title, [2] branch line, [3] branch
		// bottom padding, [4] PR line, [5] PR bottom padding.
		require.GreaterOrEqual(t, len(lines), 5, "expected title, branch, and PR rows")
		return lines
	}

	inst.SetStatusForTest(session.Ready)
	before := renderLines()
	require.NotContains(t, before[1], dimFG, "selected title must not be dimmed before deletion")
	require.NotContains(t, before[2], dimFG, "selected branch line must not be dimmed before deletion")
	require.NotContains(t, before[4], dimFG, "selected PR line must not be dimmed before deletion")

	inst.SetStatusForTest(session.Deleting)
	after := renderLines()
	assert.Contains(t, after[1], dimFG, "selected deleting title must be dimmed")
	assert.Contains(t, after[2], dimFG, "selected deleting branch line must be dimmed")
	assert.Contains(t, after[4], dimFG, "selected deleting PR line must be dimmed")
}

// TestInstanceRendererTreeArrow pins the tree affordance on instance rows
// (#1024 PR 3): an expandable instance carries ▸ collapsed / ▾ expanded, and a
// transient (Loading/Deleting) row — never expandable — renders neither.
func TestInstanceRendererTreeArrow(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "arrowed",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	r := NewInstanceRenderer(&spin)
	r.SetWidth(40)

	collapsed := strings.Split(r.Render(inst, 1, false, false, false), "\n")[1]
	assert.Contains(t, collapsed, collapsedArrow, "collapsed expandable row must show ▸")

	expanded := strings.Split(r.Render(inst, 1, false, false, true), "\n")[1]
	assert.Contains(t, expanded, expandedArrow, "expanded row must show ▾")

	inst.SetStatusForTest(session.Loading)
	transient := strings.Split(r.Render(inst, 1, false, false, false), "\n")[1]
	assert.NotContains(t, transient, collapsedArrow, "transient rows are not expandable")
	assert.NotContains(t, transient, expandedArrow, "transient rows are not expandable")
	clean := ansiEscape.ReplaceAllString(transient, "")
	assert.Contains(t, clean, "arrowed")
	assert.NotRegexp(t, regexp.MustCompile(`\b\d+\.\s+arrowed`), clean)
}

// TestRenderTabRows pins the tab child row shape: ├/└ connectors, the 1-based
// slot number matching the 1-9 jump keys, the tmux-style " *" marker on the
// active tab, and hard truncation at narrow widths.
func TestRenderTabRows(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	r := NewInstanceRenderer(&spin)
	r.SetWidth(30)

	mid := ansiEscape.ReplaceAllString(r.RenderTab("Agent", 1, false, false, true), "")
	assert.Contains(t, mid, "├ 1 Agent *", "active non-last tab: ├ connector + slot number + * marker")

	last := ansiEscape.ReplaceAllString(r.RenderTab("Terminal", 2, true, false, false), "")
	assert.Contains(t, last, "└ 2 Terminal", "last tab uses the └ connector")
	assert.NotContains(t, last, "*", "inactive tabs carry no active marker")

	// Rows must not exceed the container: effective width + the 2-cell padding
	// every sidebar row shares.
	r.SetWidth(12)
	narrow := r.RenderTab("a-very-long-process-tab-name", 3, true, true, true)
	assert.LessOrEqual(t, lipgloss.Width(narrow), 14, "tab rows must hard-truncate to the row width")
}

func TestTabRowForegroundMatchesAgentTab(t *testing.T) {
	assert.Equal(t, tabRowActiveStyle.GetForeground(), tabRowStyle.GetForeground(),
		"Terminal/default tab rows must use the same foreground as the Agent/active tab")
	assert.NotEqual(t, lipgloss.NoColor{}, tabRowSelectedStyle.GetBackground(),
		"selected tab rows keep their highlight background")
}

// TestFlatten pins the tree flattening order: each instance row immediately
// followed by its tab children when expanded.
func TestFlatten(t *testing.T) {
	rows := Flatten(3,
		func(i int) bool { return i == 1 },
		func(i int) int { return 2 })
	require.Equal(t, []Row{
		{InstanceIndex: 0, TabIndex: -1},
		{InstanceIndex: 1, TabIndex: -1},
		{InstanceIndex: 1, TabIndex: 0},
		{InstanceIndex: 1, TabIndex: 1},
		{InstanceIndex: 2, TabIndex: -1},
	}, rows)
	assert.False(t, rows[1].IsTab())
	assert.True(t, rows[2].IsTab())
}

// TestTabLabelsMirrorRealTabs pins TabLabels since #1100: once tabs have
// materialized the labels mirror the real tab list exactly — a fresh local
// instance has only its agent tab, so it renders exactly one slot; `t`
// (AddShellTab) grows it to two. Before tabs materialize (nil instance or
// mid-start) the placeholder is the single guaranteed slot, never a padded
// two-slot bar that would advertise a phantom Terminal target.
func TestTabLabelsMirrorRealTabs(t *testing.T) {
	assert.Equal(t, []string{"Agent"}, TabLabels(nil),
		"nil instance: single-slot placeholder, no phantom Terminal")

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "labeled", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"Agent"}, TabLabels(inst),
		"mid-start (no tabs yet): single-slot placeholder")

	inst.AddTabForTest("agent", session.TabKindAgent)
	assert.Equal(t, []string{"Agent"}, TabLabels(inst),
		"fresh instance (#1100): exactly one real slot, no padding to two")

	inst.AddTabForTest("shell", session.TabKindShell)
	assert.Equal(t, []string{"Agent", "Terminal"}, TabLabels(inst),
		"after t: the on-demand terminal is the second slot")

	inst.AddTabForTest("btop", session.TabKindProcess)
	assert.Equal(t, []string{"Agent", "Terminal", "btop"}, TabLabels(inst),
		"process tabs extend the list under their own names")
}
