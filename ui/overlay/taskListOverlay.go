package overlay

import (
	"claude-squad/task"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TaskListOverlay is a persistent interactive list overlay for managing tasks.
type TaskListOverlay struct {
	tasks       []task.Task
	selectedIdx int
	editing     bool   // true when editing a task title inline
	editBuffer  string // buffer for inline editing
	adding      bool   // true when adding a new task
	width       int
	closed      bool
	dirty       bool // true if tasks were modified
}

// NewTaskListOverlay creates a new task list overlay with the given tasks.
func NewTaskListOverlay(tasks []task.Task) *TaskListOverlay {
	return &TaskListOverlay{
		tasks: tasks,
		width: 60,
	}
}

// SetWidth sets the width of the overlay.
func (t *TaskListOverlay) SetWidth(width int) {
	t.width = width
}

// IsClosed returns true if the overlay should be dismissed.
func (t *TaskListOverlay) IsClosed() bool {
	return t.closed
}

// IsDirty returns true if tasks were modified.
func (t *TaskListOverlay) IsDirty() bool {
	return t.dirty
}

// GetTasks returns the current list of tasks.
func (t *TaskListOverlay) GetTasks() []task.Task {
	return t.tasks
}

// HandleKeyPress processes a key press and updates overlay state.
// Returns true if the overlay should close.
func (t *TaskListOverlay) HandleKeyPress(msg tea.KeyMsg) bool {
	if t.editing || t.adding {
		return t.handleEditMode(msg)
	}
	return t.handleNormalMode(msg)
}

func (t *TaskListOverlay) handleNormalMode(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc", "ctrl+c":
		t.closed = true
		return true
	case "up", "k":
		if t.selectedIdx > 0 {
			t.selectedIdx--
		}
	case "down", "j":
		if t.selectedIdx < len(t.tasks)-1 {
			t.selectedIdx++
		}
	case "n":
		t.adding = true
		t.editBuffer = ""
	case "enter":
		if len(t.tasks) > 0 {
			t.editing = true
			t.editBuffer = t.tasks[t.selectedIdx].Title
		}
	case "x":
		if len(t.tasks) > 0 {
			t.tasks[t.selectedIdx].Done = !t.tasks[t.selectedIdx].Done
			t.dirty = true
		}
	case "D":
		if len(t.tasks) > 0 {
			t.tasks = append(t.tasks[:t.selectedIdx], t.tasks[t.selectedIdx+1:]...)
			t.dirty = true
			if t.selectedIdx >= len(t.tasks) && t.selectedIdx > 0 {
				t.selectedIdx--
			}
		}
	}
	return false
}

func (t *TaskListOverlay) handleEditMode(msg tea.KeyMsg) bool {
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
	return false
}

// Render renders the task list overlay.
func (t *TaskListOverlay) Render(opts ...WhitespaceOption) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	doneStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#36CFC9"))
	normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	editStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF79C6"))

	content := titleStyle.Render("Tasks") + "\n\n"

	if len(t.tasks) == 0 && !t.adding {
		content += normalStyle.Render("  No tasks yet. Press n to add one.") + "\n"
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
			content += editStyle.Render("▸ "+checkbox) + editStyle.Render(t.editBuffer+"_") + "\n"
		} else if isSelected {
			content += selectedStyle.Render("▸ "+checkbox+tk.Title) + "\n"
		} else {
			content += style.Render("  "+checkbox+tk.Title) + "\n"
		}
	}

	if t.adding {
		content += editStyle.Render("▸ [ ] "+t.editBuffer+"_") + "\n"
	}

	content += "\n"
	if t.editing || t.adding {
		content += hintStyle.Render("enter save • esc cancel")
	} else {
		content += hintStyle.Render("n add • enter edit • x toggle • D delete • esc close")
	}

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Padding(1, 2).
		Width(t.width)

	return style.Render(content)
}
