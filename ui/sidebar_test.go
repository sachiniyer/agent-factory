package ui

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/store"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSidebarInitialState(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	// The rail holds the Instances tree plus the Archived folder (#1028). The
	// Archived section always exists so its collapse state persists, but it is
	// rendered only when it holds archived sessions (see the assertion below).
	assert.Equal(t, 2, len(s.sections))
	assert.Equal(t, SectionInstances, s.sections[0].Kind)
	assert.Equal(t, SectionArchived, s.sections[1].Kind)

	// Instances is expanded by default; the Archived folder is collapsed.
	assert.True(t, s.sections[0].Expanded)
	assert.False(t, s.sections[1].Expanded, "the Archived folder starts collapsed")

	// With nothing archived, no Archived header is rendered — only the
	// Instances header is visible.
	for _, it := range s.visibleItems {
		assert.NotEqual(t, SectionArchived, it.Kind, "the empty Archived folder must not render")
	}

	// Initial selection should be on Instances header
	sel := s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)
}

func TestSidebarNavigation(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	// Add some instances, each with a real agent + shell tab pair so the tree
	// walk below has two child rows per instance (#1100: no padded slots).
	inst1, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst1", Path: t.TempDir(), Program: "test",
	})
	inst2, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst2", Path: t.TempDir(), Program: "test",
	})
	addAgentShellTabs(inst1)
	addAgentShellTabs(inst2)
	addTestInstance(s, inst1)
	addTestInstance(s, inst2)

	// Start on Instances header
	sel := s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)

	// Move down onto the first instance's first tab row: instance title rows
	// remain rendered, but vertical nav stops on tabs only (#1515).
	s.Down()
	sel = s.GetSelection()
	assert.False(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)
	assert.Equal(t, 0, sel.ItemIndex)
	assert.True(t, sel.IsTab)
	assert.Equal(t, 0, sel.TabIndex)
	assert.Equal(t, 0, s.proj.ActiveTab())

	s.Down()
	sel = s.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)
	assert.Equal(t, 1, s.proj.ActiveTab())

	// Past the last child: the second instance's first tab. Selecting it
	// collapses inst1 (collapse-by-default for non-selected) and expands inst2.
	s.Down()
	sel = s.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.ItemIndex)
	assert.Equal(t, 0, sel.TabIndex)
	require.NotNil(t, s.GetSelectedInstance())
	assert.Equal(t, "inst2", s.GetSelectedInstance().Title)

	// Through inst2's children to the last row; the rail ends there (no more
	// Tasks/Hooks headers since #1024 PR 4), so further Down is a no-op.
	s.Down()
	sel = s.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.ItemIndex)
	assert.Equal(t, 1, sel.TabIndex)

	s.Down()
	assert.Equal(t, sel, s.GetSelection(), "Down past the last row is a no-op")

	// Move back up onto inst2's first tab row.
	s.Up()
	sel = s.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 0, sel.TabIndex)
}

func TestSidebarExpandCollapse(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst", Path: t.TempDir(), Program: "test",
	})
	addTestInstance(s, inst)

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
	s := NewSidebar(&spin, false, store.NewProjection())

	// Toggle Instances (starts expanded)
	s.ToggleSection()
	assert.False(t, s.sections[0].Expanded)

	s.ToggleSection()
	assert.True(t, s.sections[0].Expanded)
}

func TestSidebarJumpSections(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst", Path: t.TempDir(), Program: "test",
	})
	addTestInstance(s, inst)

	// Start on Instances header
	sel := s.GetSelection()
	assert.Equal(t, SectionInstances, sel.Kind)

	// With Instances the only section (#1024 PR 4), the section-jump keys
	// no-op forward and return the cursor to the header from a tab row.
	s.JumpNextSection()
	assert.Equal(t, sel, s.GetSelection(), "no next section to jump to")

	s.Down() // onto the instance's Agent tab
	s.JumpPrevSection()
	sel = s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)
}

func TestSidebarCollapseFromChild(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "inst", Path: t.TempDir(), Program: "test",
	})
	addTestInstance(s, inst)

	// Navigate to the instance's Agent tab (auto-expanded: it is now selected).
	s.Down()
	sel := s.GetSelection()
	assert.False(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)

	// First collapse folds the instance's own tab children in place (#1024
	// PR 3); the cursor lands on the instance row.
	s.CollapseSection()
	sel = s.GetSelection()
	assert.False(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)

	// Second collapse falls back to the pre-tree behavior: jump to the parent
	// section header and collapse the section.
	s.CollapseSection()
	sel = s.GetSelection()
	assert.True(t, sel.IsHeader)
	assert.Equal(t, SectionInstances, sel.Kind)
}

func TestSidebarInstanceManagement(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	assert.Equal(t, 0, s.proj.NumInstances())

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "test", Path: t.TempDir(), Program: "test",
	})
	addTestInstance(s, inst)
	assert.Equal(t, 1, s.proj.NumInstances())

	instances := s.proj.GetInstances()
	assert.Len(t, instances, 1)
	assert.Equal(t, "test", instances[0].Title)
}

// The #496 SetSessionPreviewSize / SetPreviewSize skip-ErrSessionGone regression
// was retired in #1592 Phase 2 PR7: the detached-tmux preview sizing (and the
// whole SetPreviewSize column) was deleted — the clientless WS broker sizes the
// pane via resize-window, so there is no per-instance preview resize to skip on a
// gone session.

func TestSidebarSelectInstance(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	inst1, _ := session.NewInstance(session.InstanceOptions{
		Title: "first", Path: t.TempDir(), Program: "test",
	})
	inst2, _ := session.NewInstance(session.InstanceOptions{
		Title: "second", Path: t.TempDir(), Program: "test",
	})
	addTestInstance(s, inst1)
	addTestInstance(s, inst2)

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
	s := NewSidebar(&spin, false, store.NewProjection())

	inst1, _ := session.NewInstance(session.InstanceOptions{
		Title: "first", Path: t.TempDir(), Program: "test",
	})
	inst2, _ := session.NewInstance(session.InstanceOptions{
		Title: "second", Path: t.TempDir(), Program: "test",
	})
	addTestInstance(s, inst1)
	addTestInstance(s, inst2)

	// Sanity check: SetSelectedInstance works when expanded.
	s.SetSelectedInstance(1)
	selected := s.GetSelectedInstance()
	require.NotNil(t, selected)
	assert.Equal(t, "second", selected.Title)

	// Collapse the Instances section; selection lands on the Instances header.
	// The first collapse folds the selected instance's own tab children (#1024
	// PR 3); the second falls back to collapsing the whole section.
	s.CollapseSection()
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
	s := NewSidebar(&spin, false, store.NewProjection())

	tasks := []task.Task{
		{ID: "1", Prompt: "backup", CronExpr: "0 0 * * *", Enabled: true, CreatedAt: time.Now()},
		{ID: "2", Prompt: "health check", CronExpr: "*/5 * * * *", Enabled: false, CreatedAt: time.Now()},
	}
	s.proj.SetTasks(tasks)

	result := s.proj.GetTasks()
	assert.Len(t, result, 2)
}

func TestSidebarRender(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	s.SetSize(40, 20)

	inst, _ := session.NewInstance(session.InstanceOptions{
		Title: "my-feature", Path: t.TempDir(), Program: "test",
	})
	addTestInstance(s, inst)

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
// for the #787 windowing tests. Each carries a real agent + shell tab pair so
// the selected instance contributes two tab child rows (#1100: no padded slots).
func newWindowingSidebar(t *testing.T, n int) *Sidebar {
	t.Helper()
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: fmt.Sprintf("win-%02d", i), Path: dir, Program: "test",
		})
		require.NoError(t, err)
		addAgentShellTabs(inst)
		addTestInstance(s, inst)
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
		{"trailing tab row", func(s *Sidebar) {
			s.SetSelectedInstance(24)
			s.Down() // first tab child of win-24
			s.Down() // last tab child — the final row of the rail
		}, "win-24"},
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

	// Walk down through every row (the header, instances, and the selected
	// instance's tab children). The row list reshapes as the selection moves —
	// each newly selected instance expands and the previous one folds (#1024
	// PR 3) — so walk until the cursor stops moving (the rail's last row)
	// rather than a fixed count.
	check("initial")
	for i := 0; ; i++ {
		require.Less(t, i, 500, "down-walk must terminate")
		before := s.rowIdentityAt(s.rawSelection())
		s.Down()
		check(fmt.Sprintf("down %d", i))
		if s.rowIdentityAt(s.rawSelection()) == before {
			break
		}
	}
	// At the very bottom nothing is hidden below, so only ▲ may show.
	up, down := indicatorArrows(s.String())
	assert.True(t, up, "rows above must be indicated at the bottom")
	assert.False(t, down, "nothing is hidden below at the bottom")

	// Walk back up to the first live tab stop.
	for i := 0; ; i++ {
		require.Less(t, i, 500, "up-walk must terminate")
		before := s.rowIdentityAt(s.rawSelection())
		s.Up()
		check(fmt.Sprintf("up %d", i))
		if s.rowIdentityAt(s.rawSelection()) == before {
			break
		}
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

// The InstanceRenderer unit tests (narrow-terminal overflow #466/#646,
// deleting marker #844/#853, prefix alignment #871/#923) moved to
// ui/tree/render_test.go with the renderer itself (#1024 PR 3).

// newRepoInstance builds a started sidebar instance whose RepoName() resolves to
// repoName, so the sidebar's repo bookkeeping (which drives the multi-repo
// indicator) can be exercised. GetRepoName() is filepath.Base(repoPath), so the
// worktree's repoPath ends in repoName.
func newRepoInstance(t *testing.T, title, repoName string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	gw, err := git.NewGitWorktreeFromStorage(
		filepath.Join(t.TempDir(), repoName), // repoPath base = repoName
		filepath.Join(t.TempDir(), "worktree"),
		title,
		"branch",
		"deadbeef",
		false,
		true,
	)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	return inst
}

// TestReplaceInstanceRepoTrackingStaleEntry: replacing an instance with one from a
// DIFFERENT repo must drop the outgoing instance's repo from the bookkeeping —
// otherwise a kill+recreate that moves a session to another repo leaves a stale
// repo entry and shows a phantom multi-repo indicator (#971).
func TestReplaceInstanceRepoTrackingStaleEntry(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	a := newRepoInstance(t, "a", "repo-A")
	c := newRepoInstance(t, "c", "repo-C")
	addTestInstance(s, a)()
	addTestInstance(s, c)()
	require.Equal(t, 2, s.proj.NumRepos())

	// Replace the repo-A instance with one from repo-B.
	b := newRepoInstance(t, "b", "repo-B")
	require.True(t, s.proj.ReplaceInstance(a, b))

	staleA := s.proj.HasRepo("repo-A")
	assert.False(t, staleA, "outgoing instance's repo (repo-A) must be dropped after a cross-repo replace")
	assert.Equal(t, 2, s.proj.NumRepos(), "repos must be {repo-B, repo-C} after the replace")
}

// TestReplaceInstanceRepoTrackingMissingNew: replacing an instance with one from a
// DIFFERENT repo must register the incoming instance's repo — otherwise the new
// repo is invisible to the multi-repo indicator until the next full reload (#971).
func TestReplaceInstanceRepoTrackingMissingNew(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	a := newRepoInstance(t, "a", "repo-A")
	addTestInstance(s, a)()
	require.Equal(t, 1, s.proj.NumRepos())

	// Replace the only instance with one from repo-B.
	b := newRepoInstance(t, "b", "repo-B")
	require.True(t, s.proj.ReplaceInstance(a, b))

	hasB := s.proj.HasRepo("repo-B")
	assert.True(t, hasB, "incoming instance's repo (repo-B) must be registered after a cross-repo replace")
	staleA := s.proj.HasRepo("repo-A")
	assert.False(t, staleA, "outgoing repo-A must not linger")
	assert.Equal(t, 1, s.proj.NumRepos(), "repos must be exactly {repo-B} after the replace")
}
