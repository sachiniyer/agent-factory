package ui

import (
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
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
