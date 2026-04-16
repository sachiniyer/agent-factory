package ui

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/task"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TaskPane renders an inline task editor in the right pane.
type TaskPane struct {
	tasks       []task.Task
	selectedIdx int

	// Edit mode
	editing    bool
	editName   textinput.Model
	editPrompt textarea.Model
	editCron   textinput.Model
	editPath   textinput.Model
	focusIndex int // 0=name, 1=prompt, 2=cron, 3=path, 4=save button

	// Create mode
	creating       bool
	createPath     string
	pendingCreate  bool
	pendingTrigger bool

	width, height int
	dirty         bool
	deleted       []task.Task
	hasFocus      bool
}

// NewTaskPane creates a new task pane.
func NewTaskPane() *TaskPane {
	return &TaskPane{}
}

// SetSize sets the display dimensions.
func (s *TaskPane) SetSize(width, height int) {
	s.width = width
	s.height = height
}

// SetTasks sets the task data.
func (s *TaskPane) SetTasks(tasks []task.Task) {
	s.tasks = tasks
	s.dirty = false
	s.deleted = nil
	s.editing = false
	if s.selectedIdx >= len(s.tasks) && s.selectedIdx > 0 {
		s.selectedIdx = len(s.tasks) - 1
	}
}

// GetTasks returns the current tasks.
func (s *TaskPane) GetTasks() []task.Task {
	return s.tasks
}

// GetDeleted returns deleted tasks for cleanup.
func (s *TaskPane) GetDeleted() []task.Task {
	return s.deleted
}

// IsDirty returns true if tasks were modified.
func (s *TaskPane) IsDirty() bool {
	return s.dirty
}

// HasFocus returns whether the pane has input focus.
func (s *TaskPane) HasFocus() bool {
	return s.hasFocus
}

// SetFocus sets the focus state.
func (s *TaskPane) SetFocus(focus bool) {
	s.hasFocus = focus
	if !focus {
		s.editing = false
		s.creating = false
	}
}

// IsEditing returns true if in edit mode.
func (s *TaskPane) IsEditing() bool {
	return s.editing
}

// IsCreating returns true if in create mode.
func (s *TaskPane) IsCreating() bool {
	return s.creating
}

// EnterCreateMode initializes empty edit fields for creating a new task.
func (s *TaskPane) EnterCreateMode(defaultPath string) {
	s.createPath = defaultPath

	name := textinput.New()
	name.Placeholder = "Task name"
	name.CharLimit = 64
	name.Focus()

	prompt := textarea.New()
	prompt.ShowLineNumbers = false
	prompt.Prompt = ""
	prompt.Blur()
	prompt.FocusedStyle.CursorLine = lipgloss.NewStyle()
	prompt.CharLimit = 0
	prompt.MaxHeight = 0
	prompt.Placeholder = "Enter task prompt..."

	cron := textinput.New()
	cron.Placeholder = "0 9 * * 1-5"
	cron.CharLimit = 64
	cron.Blur()

	path := textinput.New()
	path.SetValue(defaultPath)
	path.CharLimit = 256
	path.Blur()

	s.editName = name
	s.editPrompt = prompt
	s.editCron = cron
	s.editPath = path
	s.focusIndex = 0
	s.creating = true
	s.hasFocus = true
}

// HasPendingCreate returns true if a new task was submitted and needs saving.
func (s *TaskPane) HasPendingCreate() bool {
	return s.pendingCreate
}

// ConsumePendingCreate returns the submitted create data and clears the pending flag.
func (s *TaskPane) ConsumePendingCreate() (name, prompt, cron, path string) {
	s.pendingCreate = false
	return s.editName.Value(), s.editPrompt.Value(), s.editCron.Value(), s.editPath.Value()
}

// SetPendingTrigger marks the currently selected task to be triggered.
func (s *TaskPane) SetPendingTrigger() {
	if len(s.tasks) > 0 {
		s.pendingTrigger = true
	}
}

// HasPendingTrigger returns true if a task was triggered to run immediately.
func (s *TaskPane) HasPendingTrigger() bool {
	return s.pendingTrigger
}

// ConsumePendingTrigger returns the triggered task and clears the flag.
func (s *TaskPane) ConsumePendingTrigger() *task.Task {
	s.pendingTrigger = false
	if s.selectedIdx < len(s.tasks) {
		tsk := s.tasks[s.selectedIdx]
		return &tsk
	}
	return nil
}

// HandleKeyPress processes a key press. Returns true if consumed.
func (s *TaskPane) HandleKeyPress(msg tea.KeyMsg) bool {
	if !s.hasFocus {
		return false
	}
	if s.editing || s.creating {
		return s.handleEditMode(msg)
	}
	return s.handleNormalMode(msg)
}

func (s *TaskPane) handleNormalMode(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc":
		s.hasFocus = false
		return true
	case "up", "k":
		if s.selectedIdx > 0 {
			s.selectedIdx--
		}
		return true
	case "down", "j":
		if s.selectedIdx < len(s.tasks)-1 {
			s.selectedIdx++
		}
		return true
	case "x":
		if len(s.tasks) > 0 {
			s.tasks[s.selectedIdx].Enabled = !s.tasks[s.selectedIdx].Enabled
			s.dirty = true
		}
		return true
	case "D":
		if len(s.tasks) > 0 {
			deleted := s.tasks[s.selectedIdx]
			s.deleted = append(s.deleted, deleted)
			s.tasks = append(s.tasks[:s.selectedIdx], s.tasks[s.selectedIdx+1:]...)
			s.dirty = true
			if s.selectedIdx >= len(s.tasks) && s.selectedIdx > 0 {
				s.selectedIdx--
			}
		}
		return true
	case "enter":
		if len(s.tasks) > 0 {
			s.enterEditMode()
		}
		return true
	case "r":
		if len(s.tasks) > 0 {
			s.pendingTrigger = true
		}
		return true
	case "n":
		s.EnterCreateMode(s.createPath)
		return true
	}
	return true
}

func (s *TaskPane) enterEditMode() {
	tsk := s.tasks[s.selectedIdx]

	name := textinput.New()
	name.SetValue(tsk.Name)
	name.CharLimit = 64
	name.Focus()

	prompt := textarea.New()
	prompt.ShowLineNumbers = false
	prompt.Prompt = ""
	prompt.Blur()
	prompt.FocusedStyle.CursorLine = lipgloss.NewStyle()
	prompt.CharLimit = 0
	prompt.MaxHeight = 0
	prompt.SetValue(tsk.Prompt)

	cron := textinput.New()
	cron.SetValue(tsk.CronExpr)
	cron.CharLimit = 64
	cron.Blur()

	path := textinput.New()
	path.SetValue(tsk.ProjectPath)
	path.CharLimit = 256
	path.Blur()

	s.editName = name
	s.editPrompt = prompt
	s.editCron = cron
	s.editPath = path
	s.focusIndex = 0
	s.editing = true
}

func (s *TaskPane) handleEditMode(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyTab:
		s.focusIndex = (s.focusIndex + 1) % 5
		s.updateEditFocus()
	case tea.KeyShiftTab:
		s.focusIndex = (s.focusIndex + 4) % 5
		s.updateEditFocus()
	case tea.KeyEsc:
		s.editing = false
		s.creating = false
	case tea.KeyEnter:
		if s.focusIndex == 4 {
			if s.creating {
				if s.editName.Value() == "" {
					return true // name is required
				}
				s.pendingCreate = true
				s.creating = false
			} else {
				s.tasks[s.selectedIdx].Name = s.editName.Value()
				s.tasks[s.selectedIdx].Prompt = s.editPrompt.Value()
				s.tasks[s.selectedIdx].CronExpr = s.editCron.Value()
				s.tasks[s.selectedIdx].ProjectPath = s.editPath.Value()
				s.dirty = true
				s.editing = false
			}
			return true
		}
		if s.focusIndex == 1 {
			s.editPrompt, _ = s.editPrompt.Update(msg)
		}
	default:
		switch s.focusIndex {
		case 0:
			s.editName, _ = s.editName.Update(msg)
		case 1:
			s.editPrompt, _ = s.editPrompt.Update(msg)
		case 2:
			s.editCron, _ = s.editCron.Update(msg)
		case 3:
			s.editPath, _ = s.editPath.Update(msg)
		}
	}
	return true
}

func (s *TaskPane) updateEditFocus() {
	s.editName.Blur()
	s.editPrompt.Blur()
	s.editCron.Blur()
	s.editPath.Blur()

	switch s.focusIndex {
	case 0:
		s.editName.Focus()
	case 1:
		s.editPrompt.Focus()
	case 2:
		s.editCron.Focus()
	case 3:
		s.editPath.Focus()
	}
}

// String renders the task pane.
func (s *TaskPane) String() string {
	if s.editing || s.creating {
		return s.renderEditMode()
	}
	return s.renderListMode()
}

func (s *TaskPane) renderListMode() string {
	tStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	enabledStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#36CFC9"))
	disabledStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	detailStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	promptStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF"))
	sepLineStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#3C3C3C"))

	var b strings.Builder
	b.WriteString(tStyle.Render("Tasks"))
	b.WriteString("\n\n")

	if len(s.tasks) == 0 {
		b.WriteString(disabledStyle.Render("  No tasks. Press n to create one."))
		b.WriteString("\n")
	}

	// Available width for word-wrapping prompt text (account for indent)
	wrapWidth := s.width - 6
	if wrapWidth < 20 {
		wrapWidth = 20
	}

	for i, tsk := range s.tasks {
		if i > 0 {
			// Visual separator between tasks
			sep := strings.Repeat("─", wrapWidth)
			b.WriteString("  " + sepLineStyle.Render(sep) + "\n")
		}

		status := "[✓]"
		style := enabledStyle
		if !tsk.Enabled {
			status = "[✗]"
			style = disabledStyle
		}

		isSelected := i == s.selectedIdx
		var header string
		if tsk.Name != "" {
			header = fmt.Sprintf("%s %s  %s", status, tsk.Name, tsk.CronExpr)
		} else {
			header = fmt.Sprintf("%s %s", status, tsk.CronExpr)
		}

		if isSelected && s.hasFocus {
			b.WriteString(selectedStyle.Render("▸ " + header))
		} else {
			b.WriteString(style.Render("  " + header))
		}
		b.WriteString("\n")

		// Full prompt text, word-wrapped
		wrapped := taskPaneWordWrap(tsk.Prompt, wrapWidth)
		for _, line := range wrapped {
			b.WriteString(promptStyle.Render("    " + line))
			b.WriteString("\n")
		}

		// Program and last run info for all items
		lastRun := "never"
		if tsk.LastRunAt != nil {
			lastRun = tsk.LastRunAt.Format("Jan 02 15:04")
		}
		detail := fmt.Sprintf("    %s • last: %s", tsk.Program, lastRun)
		if tsk.LastRunStatus != "" {
			detail += " (" + tsk.LastRunStatus + ")"
		}
		b.WriteString(detailStyle.Render(detail))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if s.hasFocus {
		b.WriteString(hintStyle.Render("n new • enter edit • r run now • x toggle • D delete • esc back"))
	} else {
		b.WriteString(hintStyle.Render("enter to focus and edit tasks"))
	}

	return b.String()
}

func (s *TaskPane) renderEditMode() string {
	editTitleStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("62")).
		Bold(true).
		MarginBottom(1)

	labelStyle := lipgloss.NewStyle().Bold(true)

	buttonStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	focusedButtonStyle := buttonStyle.
		Background(lipgloss.Color("62")).
		Foreground(lipgloss.Color("0"))

	inputWidth := s.width - 6
	if inputWidth < 20 {
		inputWidth = 20
	}
	s.editName.Width = inputWidth
	s.editPrompt.SetWidth(inputWidth)
	if s.height > 0 {
		s.editPrompt.SetHeight(s.height / 4)
	}
	s.editCron.Width = inputWidth
	s.editPath.Width = inputWidth

	var b strings.Builder
	if s.creating {
		b.WriteString(editTitleStyle.Render("New Task"))
	} else {
		tsk := s.tasks[s.selectedIdx]
		b.WriteString(editTitleStyle.Render(fmt.Sprintf("Edit Task %s", tsk.ID)))
	}
	b.WriteString("\n")
	b.WriteString(labelStyle.Render("Name:"))
	b.WriteString("  ")
	b.WriteString(s.editName.View())
	b.WriteString("\n")
	b.WriteString(labelStyle.Render("Prompt:"))
	b.WriteString("\n")
	b.WriteString(s.editPrompt.View())
	b.WriteString("\n\n")
	b.WriteString(labelStyle.Render("Cron:"))
	b.WriteString("  ")
	b.WriteString(s.editCron.View())
	b.WriteString("\n")
	b.WriteString(labelStyle.Render("Path:"))
	b.WriteString("  ")
	b.WriteString(s.editPath.View())
	b.WriteString("\n\n")

	submitLabel := " Save "
	if s.creating {
		submitLabel = " Create "
	}
	if s.focusIndex == 4 {
		b.WriteString("       " + focusedButtonStyle.Render(submitLabel))
	} else {
		b.WriteString("       " + buttonStyle.Render(submitLabel))
	}

	return b.String()
}

// taskPaneWordWrap wraps text to fit within maxWidth, breaking on word boundaries.
func taskPaneWordWrap(text string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{text}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{}
	}
	var lines []string
	current := words[0]
	for _, word := range words[1:] {
		if len(current)+1+len(word) > maxWidth {
			lines = append(lines, current)
			current = word
		} else {
			current += " " + word
		}
	}
	lines = append(lines, current)
	return lines
}
