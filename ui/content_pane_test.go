package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
)

func TestContentPaneModeSwitch(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	cp := NewContentPane(tw)

	assert.Equal(t, ContentModeEmpty, cp.GetMode())

	cp.SetMode(ContentModeInstance)
	assert.Equal(t, ContentModeInstance, cp.GetMode())

	cp.SetMode(ContentModeTasks)
	assert.Equal(t, ContentModeTasks, cp.GetMode())

}

func TestContentPaneFocus(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	cp := NewContentPane(tw)

	// No focus initially
	assert.False(t, cp.HasFocus())

	// Switch to tasks mode
	cp.SetMode(ContentModeTasks)
	assert.False(t, cp.HasFocus())

	// Enter focuses the task pane
	msg := tea.KeyMsg{Type: tea.KeyEnter}
	consumed := cp.HandleKeyPress(msg)
	assert.True(t, consumed)
	assert.True(t, cp.HasFocus())

	// Esc releases focus
	escMsg := tea.KeyMsg{Type: tea.KeyEscape}
	consumed = cp.HandleKeyPress(escMsg)
	assert.True(t, consumed)
	assert.False(t, cp.HasFocus())
}

func TestContentPaneTaskFocus(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	cp := NewContentPane(tw)

	cp.SetMode(ContentModeTasks)
	assert.False(t, cp.HasFocus())

	// Enter focuses task pane
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")}
	consumed := cp.HandleKeyPress(msg)
	assert.True(t, consumed)
	assert.True(t, cp.HasFocus())
}

func TestContentPaneModeSwitchUnfocuses(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	cp := NewContentPane(tw)

	// Focus task pane
	cp.SetMode(ContentModeTasks)
	cp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, cp.HasFocus())

	// Switch mode should unfocus
	cp.SetMode(ContentModeInstance)
	assert.False(t, cp.HasFocus())
}

func TestContentPaneSetSizeMatchesRenderHeight(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	cp := NewContentPane(tw)

	const w, h = 80, 30
	cp.SetSize(w, h)

	// renderInlinePane uses Place height = c.height - windowStyle.GetVerticalFrameSize() - 2.
	// The child panes must be sized to match, otherwise content is cut off
	// or padded incorrectly (issue #336).
	expected := h - windowStyle.GetVerticalFrameSize() - 2
	assert.Equal(t, expected, cp.taskPane.height,
		"taskPane height must match renderInlinePane Place height")
	assert.Equal(t, expected, cp.hooksPane.height,
		"hooksPane height must match renderInlinePane Place height")
}

// TestContentPaneScrollRoutesToTaskPane verifies that ContentPane.ScrollUp /
// ScrollDown in Tasks mode actually move the TaskPane's selected index,
// rather than being a no-op as in #524. Both shift+up/down keys and mouse
// wheel events feed into these same methods.
func TestContentPaneScrollRoutesToTaskPane(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	cp := NewContentPane(tw)
	cp.SetMode(ContentModeTasks)
	cp.TaskPane().SetTasks([]task.Task{
		{ID: "a", Name: "first"},
		{ID: "b", Name: "second"},
		{ID: "c", Name: "third"},
	})

	assert.Equal(t, 0, cp.TaskPane().selectedIdx)

	cp.ScrollDown()
	assert.Equal(t, 1, cp.TaskPane().selectedIdx, "ScrollDown should advance selection")

	cp.ScrollDown()
	assert.Equal(t, 2, cp.TaskPane().selectedIdx)

	cp.ScrollDown()
	assert.Equal(t, 2, cp.TaskPane().selectedIdx, "ScrollDown at end should clamp")

	cp.ScrollUp()
	assert.Equal(t, 1, cp.TaskPane().selectedIdx, "ScrollUp should move selection back")

	cp.ScrollUp()
	cp.ScrollUp()
	assert.Equal(t, 0, cp.TaskPane().selectedIdx, "ScrollUp past 0 should clamp")
}

// TestContentPaneScrollRoutesToHooksPane is the hooks-mode counterpart of the
// task-pane test above. Regression test for #524.
func TestContentPaneScrollRoutesToHooksPane(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	cp := NewContentPane(tw)
	cp.SetMode(ContentModeHooks)
	cp.HooksPane().SetCommands([]string{"make build", "make test", "make lint"})

	assert.Equal(t, 0, cp.HooksPane().selectedIdx)

	cp.ScrollDown()
	assert.Equal(t, 1, cp.HooksPane().selectedIdx)

	cp.ScrollDown()
	cp.ScrollDown()
	assert.Equal(t, 2, cp.HooksPane().selectedIdx, "ScrollDown at end should clamp")

	cp.ScrollUp()
	assert.Equal(t, 1, cp.HooksPane().selectedIdx)

	cp.ScrollUp()
	cp.ScrollUp()
	assert.Equal(t, 0, cp.HooksPane().selectedIdx, "ScrollUp past 0 should clamp")
}

// TestContentPaneScrollEmptyModeNoOp verifies scroll in modes without lists
// remains a safe no-op (no panics, no side-effects on other panes).
func TestContentPaneScrollEmptyModeNoOp(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	cp := NewContentPane(tw)
	cp.TaskPane().SetTasks([]task.Task{{ID: "a"}, {ID: "b"}})
	cp.HooksPane().SetCommands([]string{"x", "y"})

	cp.SetMode(ContentModeEmpty)
	assert.NotPanics(t, func() {
		cp.ScrollUp()
		cp.ScrollDown()
	})
	assert.Equal(t, 0, cp.TaskPane().selectedIdx, "ContentModeEmpty must not move task selection")
	assert.Equal(t, 0, cp.HooksPane().selectedIdx, "ContentModeEmpty must not move hooks selection")
}

// TestTaskPaneScrollNoOpDuringEdit verifies scroll is suppressed while the
// task pane is in edit/create mode so background selection doesn't drift out
// from under the form.
func TestTaskPaneScrollNoOpDuringEdit(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a", Name: "first", Prompt: "p"},
		{ID: "b", Name: "second", Prompt: "p"},
	})
	tp.SetFocus(true)
	// Enter edit mode on the first task.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, tp.IsEditing())

	tp.ScrollDown()
	assert.Equal(t, 0, tp.selectedIdx, "ScrollDown during edit must be a no-op")
	tp.ScrollUp()
	assert.Equal(t, 0, tp.selectedIdx, "ScrollUp during edit must be a no-op")
}

// TestHooksPaneScrollNoOpDuringEdit is the hooks counterpart of the above.
func TestHooksPaneScrollNoOpDuringEdit(t *testing.T) {
	hp := NewHooksPane()
	hp.SetCommands([]string{"a", "b"})
	hp.SetFocus(true)
	hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, hp.editing)

	hp.ScrollDown()
	assert.Equal(t, 0, hp.selectedIdx, "ScrollDown during edit must be a no-op")
	hp.ScrollUp()
	assert.Equal(t, 0, hp.selectedIdx, "ScrollUp during edit must be a no-op")
}

func TestContentPaneRender(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	tw.SetSize(80, 30)
	cp := NewContentPane(tw)
	cp.SetSize(80, 30)

	// Empty mode
	cp.SetMode(ContentModeEmpty)
	rendered := cp.String()
	assert.Contains(t, rendered, "Select an item")

	// Instance mode should render the tabbed window
	cp.SetMode(ContentModeInstance)
	rendered = cp.String()
	assert.NotEmpty(t, rendered)
}
