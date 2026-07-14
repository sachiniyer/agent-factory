package tree

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// brailleFrame matches any Braille-pattern glyph — the shape the (now removed)
// bubbles spinner animated the status cell through. After #1766 no status cell
// may contain one: a working/busy row shows nothing, never an animated frame.
var brailleFrame = regexp.MustCompile(`[\x{2800}-\x{28FF}]`)

// TestInstanceRendererRemoteBadge pins the sidebar "[remote]" title badge to the
// backend's WorkspaceRemote capability rather than a Type()=="remote" check
// (#1592 Phase 1 PR2): a hook-backed (remote-workspace) instance is prefixed,
// a local one is not.
func TestInstanceRendererRemoteBadge(t *testing.T) {
	r := NewInstanceRenderer()
	r.SetWidth(60)

	render := func(t *testing.T, remote bool) string {
		t.Helper()
		inst, err := session.NewInstance(session.InstanceOptions{
			Title:   "feature",
			Path:    t.TempDir(),
			Program: "test",
		})
		require.NoError(t, err)
		if remote {
			inst.SetBackend(&session.HookBackend{})
			require.Equal(t, session.WorkspaceRemote, inst.Capabilities().Workspace)
		}
		return ansiEscape.ReplaceAllString(r.Render(inst, 1, false, false, false), "")
	}

	assert.Contains(t, render(t, true), "[remote] feature",
		"a remote-workspace instance must carry the [remote] title badge")
	assert.NotContains(t, render(t, false), "[remote]",
		"a local instance must not carry the [remote] badge")
}

// effectiveWidth models a sidebar-style content buffer (narrower than the
// allocation) so the renderer tests exercise an effective content width the
// way the sidebar passes one to SetWidth in production. Inlined rather than
// imported: these tests live in package tree, and importing ui (which imports
// tree) would cycle.
func effectiveWidth(width int) int {
	return int(float64(width) * 0.9)
}

// branchLineIndent renders an instance at the given 1-based display index and
// returns the number of leading whitespace cells in front of the branch icon.
// Instance indices are no longer rendered (#1494), so secondary-row indentation
// must not change as idx crosses power-of-10 boundaries.
func branchLineIndent(t *testing.T, idx int) int {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "feature",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	r := NewInstanceRenderer()
	r.SetWidth(60) // wide enough to render the full branch line

	out := r.Render(inst, idx, false, false, false)
	for _, line := range strings.Split(out, "\n") {
		clean := ansiEscape.ReplaceAllString(line, "")
		if pos := strings.Index(clean, branchIcon); pos >= 0 {
			return len([]rune(clean[:pos]))
		}
	}
	t.Fatalf("no branch line found in rendered output for idx=%d:\n%s", idx, out)
	return -1
}

// TestInstanceRendererOmitsInstanceIndex guards #1494: the sidebar row may
// still carry tree arrows and state glyphs, but it must not render a leading
// "1. title" / "42. title" instance index.
func TestInstanceRendererOmitsInstanceIndex(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "feature",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	r := NewInstanceRenderer()
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

// TestInstanceRendererSecondaryIndentIgnoresIndex keeps the branch-line
// indentation stable now that idx is display-only input rather than a rendered
// prefix component.
func TestInstanceRendererSecondaryIndentIgnoresIndex(t *testing.T) {

	base := branchLineIndent(t, 1)
	for _, idx := range []int{9, 10, 11, 99, 100, 101, 999, 1000, 1001, 9999, 10000} {
		got := branchLineIndent(t, idx)
		require.Equalf(t, base, got,
			"branch line indent at idx=%d (%d) must match idx=1 (%d); idx leaked into display",
			idx, got, base)
	}
}

// renderForTerminal renders an instance at the sidebar width app.go derives
// from the given terminal width, and returns the rendered title line. The
// renderer wraps each section in lipgloss padding, so the visible title
// content sits on line 1 (after the top-padding line).
func renderForTerminal(t *testing.T, terminalW int, inst *session.Instance) (titleLine string, sidebarW int) {
	t.Helper()
	sidebarW = int(float32(terminalW) * 0.3)
	r := NewInstanceRenderer()
	r.SetWidth(effectiveWidth(sidebarW))
	out := r.Render(inst, 1, false, false, false)
	lines := strings.Split(out, "\n")
	require.GreaterOrEqual(t, len(lines), 2, "renderer should emit at least a title row")
	titleLine = lines[1]
	return titleLine, sidebarW
}

// TestInstanceRendererNarrowTerminalNoOverflow guards against the regression
// reported in #466: at 40-43 column terminal widths the sidebar's instance
// row ended with a "..." artifact that pushed the rendered line one cell past
// the sidebar container width.
func TestInstanceRendererNarrowTerminalNoOverflow(t *testing.T) {
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
			titleLine, sidebarW := renderForTerminal(t, tc.terminalW, inst)
			w := lipgloss.Width(titleLine)

			if tc.expectFullTitle {
				assert.Contains(t, titleLine, inst.Title, "wide terminal should render the full title")
				assert.NotContains(t, titleLine, "…", "wide terminal should not truncate")
			}
			if tc.expectEllipsis {
				assert.Contains(t, titleLine, "…", "title should be truncated with ellipsis when there is room for it")
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

// branchLineForTerminal renders an instance at the sidebar width app.go
// derives from terminalW and returns the rendered secondary row — the one
// carrying the ⎇ branch glyph.
func branchLineForTerminal(t *testing.T, terminalW int, inst *session.Instance) (branchLine string, sidebarW int) {
	t.Helper()
	sidebarW = int(float32(terminalW) * 0.3)
	r := NewInstanceRenderer()
	r.SetWidth(effectiveWidth(sidebarW))
	out := r.Render(inst, 1, false, false, false)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(ansiEscape.ReplaceAllString(line, ""), branchIcon) {
			return line, sidebarW
		}
	}
	t.Fatalf("no branch line found in rendered output:\n%s", out)
	return "", sidebarW
}

// TestInstanceRendererBranchLineNarrowWidth guards the #1772-review fix: the
// branch-name truncation must budget for the one-cell "…" tail (U+2026), not
// the old three-cell ASCII "..." reservation (remainingWidth-3), which
// over-reserved two cells and mis-truncated at narrow widths. Across a sweep of
// narrow-to-medium widths the rendered branch row must (a) stay within the
// sidebar container, (b) never emit an ASCII "..." artifact, and (c) truncate
// with a "…" tail where it doesn't fit.
func TestInstanceRendererBranchLineNarrowWidth(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "feature",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	// Single-goroutine test: set the branch directly (GetBranch reads this).
	inst.Branch = "very-long-feature-branch-name-that-must-truncate"

	sawEllipsis := false
	for terminalW := 30; terminalW <= 120; terminalW++ {
		branchLine, sidebarW := branchLineForTerminal(t, terminalW, inst)
		clean := ansiEscape.ReplaceAllString(branchLine, "")
		assert.LessOrEqualf(t, lipgloss.Width(branchLine), sidebarW,
			"branch line width (%d) must fit sidebar container (%d) at terminal=%d",
			lipgloss.Width(branchLine), sidebarW, terminalW)
		assert.NotContainsf(t, clean, "...",
			"branch line must not carry an ASCII '...' artifact at terminal=%d", terminalW)
		if strings.Contains(clean, "…") {
			sawEllipsis = true
		}
	}
	assert.True(t, sawEllipsis,
		"a long branch must truncate with a '…' tail at some swept width")
}

// TestInstanceRendererDeletingMarker pins the #844 sidebar treatment: a row
// whose teardown is running in the background must carry an explicit
// "[deleting]" marker (a bare working row shows no status glyph).
func TestInstanceRendererDeletingMarker(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "going-away",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	inst.SetStatusForTest(session.Ready)
	before, _ := renderForTerminal(t, 120, inst)
	assert.NotContains(t, before, "[deleting]")

	inst.SetStatusForTest(session.Deleting)
	after, _ := renderForTerminal(t, 120, inst)
	assert.Contains(t, after, "[deleting]", "deleting rows must be explicitly marked")
	assert.Contains(t, after, "going-away", "the title must remain visible while deleting")
}

// TestInstanceRendererLimitReachedMarker guards the #1195 exhaustive-render
// requirement for #1146: a LimitReached liveness renders its own explicit marker
// (no silent default / blank dot). #1204 refines the label + reset time on top.
func TestInstanceRendererLimitReachedMarker(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "throttled",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	_ = inst.Transition(session.ObserveLiveness(session.LiveLimitReached))
	out, _ := renderForTerminal(t, 120, inst)
	assert.Contains(t, out, "[limit]", "a usage-limit-reached row must be explicitly marked (#1146)")
	assert.Contains(t, out, "throttled", "the title must remain visible")
}

// TestInstanceRendererStatusGlyphs pins the #1766 status-cell contract, the same
// (Liveness, InFlightOp)→glyph mapping the web mirrors (web/src/status.ts): only a
// waiting/Ready session shows the green ● dot; every working/busy state — LiveRunning
// or ANY in-flight op (create/restore/kill/archive) — shows NO status glyph and NO
// animated spinner frame; the terminal/error states keep their STATIC glyphs (◌/○/◆/▧).
// An in-flight create/restore is a LOADING state that draws a clean blank cell — no
// dot, no spinner, no stale glyph — and the op axis deliberately masks the liveness
// glyph so a restoring-but-still-Archived row never leaks the ▧ nor a premature dot.
func TestInstanceRendererStatusGlyphs(t *testing.T) {
	newInst := func(t *testing.T, title string) *session.Instance {
		t.Helper()
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: title, Path: t.TempDir(), Program: "test",
		})
		require.NoError(t, err)
		return inst
	}
	// cell renders inst at a wide width and returns the ANSI-stripped title line —
	// the row's status glyph sits at its trailing edge.
	cell := func(t *testing.T, inst *session.Instance) string {
		t.Helper()
		title, _ := renderForTerminal(t, 120, inst)
		return ansiEscape.ReplaceAllString(title, "")
	}
	// allGlyphs is every positive/terminal status glyph; a working row must show none.
	allGlyphs := []string{"●", "○", "◌", "▧", "◆"}

	t.Run("ready shows the green dot", func(t *testing.T) {
		inst := newInst(t, "waiting")
		require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
		clean := cell(t, inst)
		assert.Contains(t, clean, "●", "a waiting/Ready row must show the green ● dot")
		assert.NotRegexp(t, brailleFrame, clean, "the ready dot is static, never an animated frame")
	})

	t.Run("running shows no status glyph", func(t *testing.T) {
		inst := newInst(t, "busy")
		require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
		clean := cell(t, inst)
		assert.NotRegexp(t, brailleFrame, clean, "a working row must not render a spinner frame")
		for _, g := range allGlyphs {
			assert.NotContainsf(t, clean, g, "a working row must not show the %s status glyph", g)
		}
	})

	for _, op := range []session.InFlightOp{session.OpKilling, session.OpArchiving} {
		op := op
		t.Run(fmt.Sprintf("in-flight op %v shows no glyph but stays [deleting]", op), func(t *testing.T) {
			inst := newInst(t, "going")
			require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
			inst.SetInFlightOpForTest(op)
			clean := cell(t, inst)
			assert.Contains(t, clean, "[deleting]", "a kill/archive row keeps its [deleting] prefix")
			assert.NotRegexp(t, brailleFrame, clean, "an in-flight-op row must not render a spinner frame")
			for _, g := range allGlyphs {
				assert.NotContainsf(t, clean, g, "an in-flight-op row must not show the %s status glyph", g)
			}
		})
	}

	// An in-flight create is a LOADING state: no glyph, and (unlike kill/archive) NO
	// title prefix — a bare, clean loading row (#1766). The green dot appears only once
	// the op clears and the agent is Ready (the progression test below).
	t.Run("creating (OpCreating) shows no glyph and no prefix", func(t *testing.T) {
		inst := newInst(t, "spawning")
		inst.SetInFlightOpForTest(session.OpCreating)
		clean := cell(t, inst)
		assert.NotRegexp(t, brailleFrame, clean, "a creating row must not render a spinner frame")
		for _, g := range allGlyphs {
			assert.NotContainsf(t, clean, g, "a creating row must not show the %s status glyph", g)
		}
		assert.NotContains(t, clean, "[deleting]", "create adds no going-away prefix")
		assert.Contains(t, clean, "spawning", "a creating row still shows its name")
	})

	// An in-flight restore of an ARCHIVED instance is rehomed into the live section
	// (#1210) while its liveness is still LiveArchived. The op axis is checked FIRST, so
	// it must render a BLANK status cell — NOT the ▧ archived glyph and NOT the green
	// dot — until the restore completes (#1766).
	t.Run("restoring (OpRestoring) on an archived instance shows no glyph", func(t *testing.T) {
		inst := newInst(t, "coming-back")
		inst.SetArchived()
		inst.SetInFlightOpForTest(session.OpRestoring)
		require.False(t, inst.ShownArchived(), "a restoring instance is rehomed into the live section")
		clean := cell(t, inst)
		assert.NotRegexp(t, brailleFrame, clean, "a restoring row must not render a spinner frame")
		for _, g := range allGlyphs {
			assert.NotContainsf(t, clean, g, "a restoring row must not show the %s status glyph (not ▧, not ●)", g)
		}
	})

	// The loading → ready progression: a create shows nothing while in flight, then the
	// green dot appears the instant the op clears and the agent reaches Ready (#1766).
	t.Run("a cleared create at Ready shows the green dot", func(t *testing.T) {
		inst := newInst(t, "born")
		inst.SetInFlightOpForTest(session.OpCreating)
		for _, g := range allGlyphs {
			assert.NotContainsf(t, cell(t, inst), g, "while creating, no %s glyph", g)
		}
		inst.SetInFlightOpForTest(session.OpNone)
		require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
		clean := cell(t, inst)
		assert.Contains(t, clean, "●", "once Ready with no op, the green dot appears")
		assert.NotRegexp(t, brailleFrame, clean, "the ready dot is static")
	})

	statics := []struct {
		name  string
		lv    session.Liveness
		glyph string
	}{
		{"lost", session.LiveLost, "◌"},
		{"dead", session.LiveDead, "○"},
		{"limit", session.LiveLimitReached, "◆"},
	}
	for _, tc := range statics {
		tc := tc
		t.Run(tc.name+" shows its static glyph", func(t *testing.T) {
			inst := newInst(t, "x")
			require.NoError(t, inst.Transition(session.ObserveLiveness(tc.lv)))
			clean := cell(t, inst)
			assert.Containsf(t, clean, tc.glyph, "%s row must show its static %s glyph", tc.name, tc.glyph)
			assert.NotRegexpf(t, brailleFrame, clean, "%s row must render a static glyph, no spinner", tc.name)
		})
	}

	t.Run("archived shows its static glyph", func(t *testing.T) {
		inst := newInst(t, "filed")
		inst.SetArchived()
		clean := cell(t, inst)
		assert.Contains(t, clean, "▧", "an archived row must show its static ▧ glyph")
		assert.NotRegexp(t, brailleFrame, clean, "an archived row must render a static glyph, no spinner")
	})
}

// TestInstanceRendererArchivedShowsName pins #1225: an archived row must show
// its NAME at the widths users actually run (100/80 cols), not a name-eating
// "[archived] " word prefix that clips every archived session to "[archived]...".
// The archived state is conveyed by the ▧ glyph + dimming + section header, so
// the title cell is spent on the identifier, exactly like a live row.
func TestInstanceRendererArchivedShowsName(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "one",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	inst.SetArchived()

	for _, terminalW := range []int{100, 80} {
		title, _ := renderForTerminal(t, terminalW, inst)
		clean := ansiEscape.ReplaceAllString(title, "")
		assert.Containsf(t, clean, "one",
			"archived row must show its name at %d cols; got %q", terminalW, clean)
		assert.NotContainsf(t, clean, "[archived]",
			"archived row must not carry the name-eating text prefix at %d cols; got %q", terminalW, clean)
	}
}

func TestInstanceRendererCreatingRowShowsBareName(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "alpha",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(session.BeginCreate()))

	r := NewInstanceRenderer()
	r.SetWidth(40)

	title := strings.Split(ansiEscape.ReplaceAllString(r.Render(inst, 1, true, false, false), ""), "\n")[1]
	assert.Contains(t, title, "alpha", "creating rows must show the typed name")
	assert.NotContains(t, title, "Session name:", "creating rows must not prefix the input label")
}

// TestInstanceRendererDeletingDimsSelectedRow pins the #853 fix: a SELECTED
// deleting row must dim its branch line along with the title. Before the fix
// only titleS picked up deletingTitleColor, so the high-contrast
// selectedDescStyle left the secondary line brighter than the dimmed title.
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

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "going-away",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	// SGR foreground params of the default Zenburn deleting gray.
	dimFG := termenv.RGBColor("#989890").Sequence(false)

	r := NewInstanceRenderer()
	r.SetWidth(effectiveWidth(36))

	renderLines := func() []string {
		out := r.Render(inst, 1, true, false, false)
		lines := strings.Split(out, "\n")
		// [0] title top padding, [1] title, [2] branch line, [3] branch
		// bottom padding.
		require.GreaterOrEqual(t, len(lines), 3, "expected title and branch rows")
		return lines
	}

	inst.SetStatusForTest(session.Ready)
	before := renderLines()
	require.NotContains(t, before[1], dimFG, "selected title must not be dimmed before deletion")
	require.NotContains(t, before[2], dimFG, "selected branch line must not be dimmed before deletion")

	inst.SetStatusForTest(session.Deleting)
	after := renderLines()
	assert.Contains(t, after[1], dimFG, "selected deleting title must be dimmed")
	assert.Contains(t, after[2], dimFG, "selected deleting branch line must be dimmed")
}

// TestInstanceRendererTreeArrow pins the tree affordance on instance rows
// (#1024 PR 3): an expandable instance carries ▸ collapsed / ▾ expanded, and a
// transient (Loading/Deleting) row — never expandable — renders neither.
func TestInstanceRendererTreeArrow(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "arrowed",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)

	r := NewInstanceRenderer()
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
	r := NewInstanceRenderer()
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
