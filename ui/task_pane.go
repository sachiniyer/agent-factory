package ui

import (
	"claude-squad/task"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TaskPane renders an interactive task list inline in the right pane.
type TaskPane struct {
	tasks       []task.Task
	selectedIdx int
	editing     bool
	editBuffer  string
	adding      bool
	width       int
	height      int
	dirty       bool
	hasFocus    bool
}

// NewTaskPane creates a new task pane.
func NewTaskPane() *TaskPane {
	return &TaskPane{}
}

// SetSize sets the display dimensions.
func (t *TaskPane) SetSize(width, height int) {
	t.width = width
	t.height = height
}

// SetTasks sets the task data.
func (t *TaskPane) SetTasks(tasks []task.Task) {
	t.tasks = tasks
	t.dirty = false
	if t.selectedIdx >= len(t.tasks) && t.selectedIdx > 0 {
		t.selectedIdx = len(t.tasks) - 1
	}
}

// GetTasks returns the current tasks.
func (t *TaskPane) GetTasks() []task.Task {
	return t.tasks
}

// IsDirty returns true if tasks were modified.
func (t *TaskPane) IsDirty() bool {
	return t.dirty
}

// HasFocus returns whether the pane has input focus.
func (t *TaskPane) HasFocus() bool {
	return t.hasFocus
}

// SetFocus sets the focus state.
func (t *TaskPane) SetFocus(focus bool) {
	t.hasFocus = focus
	if !focus {
		t.editing = false
		t.adding = false
		t.editBuffer = ""
	}
}

// HandleKeyPress processes a key press. Returns true if the key was consumed.
func (t *TaskPane) HandleKeyPress(msg tea.KeyMsg) bool {
	if !t.hasFocus {
		return false
	}
	if t.editing || t.adding {
		return t.handleEditMode(msg)
	}
	return t.handleNormalMode(msg)
}

func (t *TaskPane) handleNormalMode(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc":
		t.hasFocus = false
		return true
	case "up", "k":
		if t.selectedIdx > 0 {
			t.selectedIdx--
		}
		return true
	case "down", "j":
		if t.selectedIdx < len(t.tasks)-1 {
			t.selectedIdx++
		}
		return true
	case "n":
		t.adding = true
		t.editBuffer = ""
		return true
	case "enter":
		if len(t.tasks) > 0 {
			t.editing = true
			t.editBuffer = t.tasks[t.selectedIdx].Title
		}
		return true
	case "x":
		if len(t.tasks) > 0 {
			t.tasks[t.selectedIdx].Done = !t.tasks[t.selectedIdx].Done
			t.dirty = true
		}
		return true
	case "D":
		if len(t.tasks) > 0 {
			t.tasks = append(t.tasks[:t.selectedIdx], t.tasks[t.selectedIdx+1:]...)
			t.dirty = true
			if t.selectedIdx >= len(t.tasks) && t.selectedIdx > 0 {
				t.selectedIdx--
			}
		}
		return true
	}
	return true // consume all keys when focused
}

func (t *TaskPane) handleEditMode(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEnter:
		if t.editBuffer != "" {
			if t.adding {
				t.tasks = append(t.tasks, task.Task{
					ID:        task.GenerateID(),
					Title:     t.editBuffer,
					CreatedAt: time.Now(),
				})
				t.selectedIdx = len(t.tasks) - 1
			} else {
				t.tasks[t.selectedIdx].Title = t.editBuffer
			}
			t.dirty = true
		}
		t.adding = false
		t.editing = false
		t.editBuffer = ""
	case tea.KeyEsc:
		t.adding = false
		t.editing = false
		t.editBuffer = ""
	case tea.KeyBackspace:
		if len(t.editBuffer) > 0 {
			runes := []rune(t.editBuffer)
			t.editBuffer = string(runes[:len(runes)-1])
		}
	case tea.KeySpace:
		t.editBuffer += " "
	case tea.KeyRunes:
		t.editBuffer += string(msg.Runes)
	}
	return true
}

// String renders the task pane.
func (t *TaskPane) String() string {
	tStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	doneStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#36CFC9"))
	normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	editStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF79C6"))

	var b strings.Builder
	b.WriteString(tStyle.Render("Tasks"))
	b.WriteString("\n\n")

	if len(t.tasks) == 0 && !t.adding {
		b.WriteString(normalStyle.Render("  No tasks yet. Press Enter to focus, then n to add."))
		b.WriteString("\n")
	}

	for i, tk := range t.tasks {
		checkbox := "[ ] "
		style := normalStyle
		if tk.Done {
			checkbox = "[x] "
			style = doneStyle
		}
		isSelected := i == t.selectedIdx
		if t.editing && isSelected {
			b.WriteString(editStyle.Render("▸ "+checkbox) + editStyle.Render(t.editBuffer+"_"))
		} else if isSelected && t.hasFocus {
			b.WriteString(selectedStyle.Render("▸ " + checkbox + tk.Title))
		} else {
			b.WriteString(style.Render("  " + checkbox + tk.Title))
		}
		b.WriteString("\n")
	}

	if t.adding {
		b.WriteString(editStyle.Render("▸ [ ] " + t.editBuffer + "_"))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if t.hasFocus {
		if t.editing || t.adding {
			b.WriteString(hintStyle.Render("enter save • esc cancel"))
		} else {
			b.WriteString(hintStyle.Render("n add • enter edit • x toggle • D delete • esc back"))
		}
	} else {
		b.WriteString(hintStyle.Render("enter to focus and edit tasks"))
	}

	return b.String()
}
