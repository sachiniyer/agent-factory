package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// programDefaultLabel is the selector option that resolves to an empty Program
// string (the daemon then falls back to the user's configured default_program).
const programDefaultLabel = "(use config default)"

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
	// Program selector state. editProgramOptions is the list of choices shown
	// inline (index 0 is always the "use config default" entry, followed by
	// tmux.SupportedPrograms). Per-task Program is restricted to the agent
	// enum (#658); per-task paths-with-flags are out of scope.
	editProgramOptions []string
	editProgramIdx     int
	editError          string // last validation error shown to the user
	focusIndex         int    // 0=name, 1=prompt, 2=cron, 3=path, 4=program, 5=save button

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
	if len(s.tasks) == 0 {
		s.selectedIdx = 0
	} else if s.selectedIdx >= len(s.tasks) {
		s.selectedIdx = len(s.tasks) - 1
	}
}

// GetTasks returns the current tasks.
func (s *TaskPane) GetTasks() []task.Task {
	return s.tasks
}

// ConsumeDeleted returns the tasks pending deletion and clears the pane's
// dirty state so a subsequent save can't reprocess already-deleted tasks. The
// deletion loop in saveContentPaneState removes task records as a side
// effect, so re-running it would call RemoveTask on records that no longer
// exist and log spurious errors (fixes #763).
func (s *TaskPane) ConsumeDeleted() []task.Task {
	deleted := s.deleted
	s.deleted = nil
	s.dirty = false
	return deleted
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
	s.setProgramFromValue("")
	s.focusIndex = 0
	s.creating = true
	s.hasFocus = true
	s.editError = ""
}

// setProgramFromValue initializes the selector state from a stored Program
// string. An empty value selects "(use config default)"; a value matching a
// SupportedPrograms entry pre-selects that canonical option; any non-enum
// value (legacy task data from before #658) is treated as the default so
// the user can re-pick a canonical option without losing edits to other
// fields. Save-side validation rejects non-enum Program writes outright.
func (s *TaskPane) setProgramFromValue(value string) {
	opts := make([]string, 0, len(tmux.SupportedPrograms)+1)
	opts = append(opts, programDefaultLabel)
	opts = append(opts, tmux.SupportedPrograms...)
	s.editProgramOptions = opts

	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		s.editProgramIdx = 0
		return
	}
	for i, p := range tmux.SupportedPrograms {
		if trimmed == p {
			s.editProgramIdx = i + 1
			return
		}
	}
	s.editProgramIdx = 0
}

// programValue returns the Program string corresponding to the current
// selector state: "" for the default option, or the canonical agent name
// otherwise.
func (s *TaskPane) programValue() string {
	if s.editProgramIdx <= 0 || s.editProgramIdx >= len(s.editProgramOptions) {
		return ""
	}
	return s.editProgramOptions[s.editProgramIdx]
}

// HasPendingCreate returns true if a new task was submitted and needs saving.
func (s *TaskPane) HasPendingCreate() bool {
	return s.pendingCreate
}

// ConsumePendingCreate returns the submitted create data and clears the pending flag.
// program is the user-supplied program override; empty means "use the caller's default".
func (s *TaskPane) ConsumePendingCreate() (name, prompt, cron, path, program string) {
	s.pendingCreate = false
	return s.editName.Value(), s.editPrompt.Value(), s.editCron.Value(), s.editPath.Value(), s.programValue()
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
	if s.selectedIdx >= 0 && s.selectedIdx < len(s.tasks) {
		tsk := s.tasks[s.selectedIdx]
		return &tsk
	}
	return nil
}

// ScrollUp moves the selection up one row. Used by shift+up and mouse wheel
// regardless of focus, so the user can browse the task list without first
// focusing the pane. No-op while a task is being edited or created so the
// background selection doesn't drift out from under the form.
func (s *TaskPane) ScrollUp() {
	if s.editing || s.creating {
		return
	}
	if s.selectedIdx > 0 {
		s.selectedIdx--
	}
}

// ScrollDown moves the selection down one row. See ScrollUp.
func (s *TaskPane) ScrollDown() {
	if s.editing || s.creating {
		return
	}
	if s.selectedIdx < len(s.tasks)-1 {
		s.selectedIdx++
	}
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
	if msg.String() == "ctrl+c" || msg.String() == "q" {
		return false
	}
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
	s.setProgramFromValue(tsk.Program)
	s.focusIndex = 0
	s.editing = true
	s.editError = ""
}

func (s *TaskPane) handleEditMode(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyTab:
		s.focusIndex = (s.focusIndex + 1) % 6
		s.updateEditFocus()
	case tea.KeyShiftTab:
		s.focusIndex = (s.focusIndex + 5) % 6
		s.updateEditFocus()
	case tea.KeyEsc, tea.KeyCtrlC:
		s.editing = false
		s.creating = false
		s.editError = ""
	case tea.KeyEnter:
		if s.focusIndex == 5 {
			if s.creating {
				if s.editName.Value() == "" {
					s.editError = "name is required"
					return true
				}
				if strings.TrimSpace(s.editPrompt.Value()) == "" {
					s.editError = "prompt must be non-empty"
					return true
				}
				if err := task.ValidateCronExpr(s.editCron.Value()); err != nil {
					s.editError = fmt.Sprintf("invalid cron: %v", err)
					return true
				}
				s.editError = ""
				s.pendingCreate = true
				s.creating = false
			} else {
				if s.editName.Value() == "" {
					s.editError = "name is required"
					return true
				}
				// Watch tasks (#782 phase 2) have no cron expression and may
				// have an empty prompt (each event defaults to the raw line).
				// The edit form gains watch-cmd/target-session fields in
				// phase 3; until then the cron field must stay empty so the
				// save can't produce a task with two triggers.
				isWatch := s.tasks[s.selectedIdx].WatchCmd != ""
				if isWatch {
					if strings.TrimSpace(s.editCron.Value()) != "" {
						s.editError = "this is a watch task; cron must stay empty (edit the trigger with af tasks update)"
						return true
					}
				} else {
					if strings.TrimSpace(s.editPrompt.Value()) == "" {
						s.editError = "prompt must be non-empty"
						return true
					}
					if err := task.ValidateCronExpr(s.editCron.Value()); err != nil {
						s.editError = fmt.Sprintf("invalid cron: %v", err)
						return true
					}
				}
				// Mirror the create path (app.handleTaskCreate): resolve
				// the user-entered path to an absolute form so an empty
				// or relative value behaves the same when the scheduler
				// fires as it does in the TUI trigger (#641).
				absPath, err := filepath.Abs(s.editPath.Value())
				if err != nil {
					s.editError = fmt.Sprintf("invalid path: %v", err)
					return true
				}
				s.editError = ""
				s.tasks[s.selectedIdx].Name = s.editName.Value()
				s.tasks[s.selectedIdx].Prompt = s.editPrompt.Value()
				s.tasks[s.selectedIdx].CronExpr = s.editCron.Value()
				s.tasks[s.selectedIdx].ProjectPath = absPath
				s.tasks[s.selectedIdx].Program = s.programValue()
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
		case 4:
			s.handleProgramKey(msg)
		}
	}
	return true
}

// handleProgramKey moves the selector cursor when the Program field has focus.
// Up/k and down/j navigate; other keys are ignored so the selector behaves
// like a list, not a free-text input (#492).
func (s *TaskPane) handleProgramKey(msg tea.KeyMsg) {
	if len(s.editProgramOptions) == 0 {
		return
	}
	switch msg.String() {
	case "up", "k", "left", "h":
		if s.editProgramIdx > 0 {
			s.editProgramIdx--
		}
	case "down", "j", "right", "l":
		if s.editProgramIdx < len(s.editProgramOptions)-1 {
			s.editProgramIdx++
		}
	}
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
		// Watch tasks have no cron expression; show their trigger instead.
		// Editing the watch fields in the TUI lands in phase 3 of #782.
		trigger := tsk.CronExpr
		if tsk.WatchCmd != "" {
			trigger = "watch: " + tsk.WatchCmd
		}
		var header string
		if tsk.Name != "" {
			header = fmt.Sprintf("%s %s  %s", status, tsk.Name, trigger)
		} else {
			header = fmt.Sprintf("%s %s", status, trigger)
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
		programLabel := tsk.Program
		if programLabel == "" {
			programLabel = programDefaultLabel
		}
		detail := fmt.Sprintf("    %s • last: %s", programLabel, lastRun)
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
	b.WriteString("\n")
	b.WriteString(labelStyle.Render("Program:"))
	b.WriteString("\n")
	b.WriteString(s.renderProgramSelector())
	b.WriteString("\n")

	submitLabel := " Save "
	if s.creating {
		submitLabel = " Create "
	}
	if s.focusIndex == 5 {
		b.WriteString("       " + focusedButtonStyle.Render(submitLabel))
	} else {
		b.WriteString("       " + buttonStyle.Render(submitLabel))
	}

	if s.editError != "" {
		errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
		b.WriteString("\n\n")
		b.WriteString(errorStyle.Render("! " + s.editError))
	}

	return b.String()
}

// renderProgramSelector renders the inline agent selector. Focused selection
// is highlighted in yellow with a ▸ marker; the unfocused row gets the
// dim/focus-hint treatment used by the rest of the edit form.
func (s *TaskPane) renderProgramSelector() string {
	focused := s.focusIndex == 4
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	dimSelectedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))

	var b strings.Builder
	for i, opt := range s.editProgramOptions {
		isSel := i == s.editProgramIdx
		switch {
		case isSel && focused:
			b.WriteString("    " + selectedStyle.Render("▸ "+opt))
		case isSel:
			b.WriteString("    " + dimSelectedStyle.Render("▸ "+opt))
		default:
			b.WriteString("    " + normalStyle.Render("  "+opt))
		}
		b.WriteString("\n")
	}
	if focused {
		hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
		b.WriteString("    " + hintStyle.Render("↑/↓ change agent"))
		b.WriteString("\n")
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
