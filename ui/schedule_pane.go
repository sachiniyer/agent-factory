package ui

import (
	"claude-squad/schedule"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SchedulePane renders an inline schedule editor in the right pane.
type SchedulePane struct {
	schedules   []schedule.Schedule
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
	deleted       []schedule.Schedule
	hasFocus      bool
}

// NewSchedulePane creates a new schedule pane.
func NewSchedulePane() *SchedulePane {
	return &SchedulePane{}
}

// SetSize sets the display dimensions.
func (s *SchedulePane) SetSize(width, height int) {
	s.width = width
	s.height = height
}

// SetSchedules sets the schedule data.
func (s *SchedulePane) SetSchedules(schedules []schedule.Schedule) {
	s.schedules = schedules
	s.dirty = false
	s.deleted = nil
	s.editing = false
	if s.selectedIdx >= len(s.schedules) && s.selectedIdx > 0 {
		s.selectedIdx = len(s.schedules) - 1
	}
}

// GetSchedules returns the current schedules.
func (s *SchedulePane) GetSchedules() []schedule.Schedule {
	return s.schedules
}

// GetDeleted returns deleted schedules for cleanup.
func (s *SchedulePane) GetDeleted() []schedule.Schedule {
	return s.deleted
}

// IsDirty returns true if schedules were modified.
func (s *SchedulePane) IsDirty() bool {
	return s.dirty
}

// HasFocus returns whether the pane has input focus.
func (s *SchedulePane) HasFocus() bool {
	return s.hasFocus
}

// SetFocus sets the focus state.
func (s *SchedulePane) SetFocus(focus bool) {
	s.hasFocus = focus
	if !focus {
		s.editing = false
		s.creating = false
	}
}

// IsEditing returns true if in edit mode.
func (s *SchedulePane) IsEditing() bool {
	return s.editing
}

// IsCreating returns true if in create mode.
func (s *SchedulePane) IsCreating() bool {
	return s.creating
}

// EnterCreateMode initializes empty edit fields for creating a new schedule.
func (s *SchedulePane) EnterCreateMode(defaultPath string) {
	s.createPath = defaultPath

	name := textinput.New()
	name.Placeholder = "Schedule name"
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

// HasPendingCreate returns true if a new schedule was submitted and needs saving.
func (s *SchedulePane) HasPendingCreate() bool {
	return s.pendingCreate
}

// ConsumePendingCreate returns the submitted create data and clears the pending flag.
func (s *SchedulePane) ConsumePendingCreate() (name, prompt, cron, path string) {
	s.pendingCreate = false
	return s.editName.Value(), s.editPrompt.Value(), s.editCron.Value(), s.editPath.Value()
}

// SetPendingTrigger marks the currently selected schedule to be triggered.
func (s *SchedulePane) SetPendingTrigger() {
	if len(s.schedules) > 0 {
		s.pendingTrigger = true
	}
}

// HasPendingTrigger returns true if a schedule was triggered to run immediately.
func (s *SchedulePane) HasPendingTrigger() bool {
	return s.pendingTrigger
}

// ConsumePendingTrigger returns the triggered schedule and clears the flag.
func (s *SchedulePane) ConsumePendingTrigger() *schedule.Schedule {
	s.pendingTrigger = false
	if s.selectedIdx < len(s.schedules) {
		sched := s.schedules[s.selectedIdx]
		return &sched
	}
	return nil
}

// HandleKeyPress processes a key press. Returns true if consumed.
func (s *SchedulePane) HandleKeyPress(msg tea.KeyMsg) bool {
	if !s.hasFocus {
		return false
	}
	if s.editing || s.creating {
		return s.handleEditMode(msg)
	}
	return s.handleNormalMode(msg)
}

func (s *SchedulePane) handleNormalMode(msg tea.KeyMsg) bool {
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
		if s.selectedIdx < len(s.schedules)-1 {
			s.selectedIdx++
		}
		return true
	case "x":
		if len(s.schedules) > 0 {
			s.schedules[s.selectedIdx].Enabled = !s.schedules[s.selectedIdx].Enabled
			s.dirty = true
		}
		return true
	case "D":
		if len(s.schedules) > 0 {
			deleted := s.schedules[s.selectedIdx]
			s.deleted = append(s.deleted, deleted)
			s.schedules = append(s.schedules[:s.selectedIdx], s.schedules[s.selectedIdx+1:]...)
			s.dirty = true
			if s.selectedIdx >= len(s.schedules) && s.selectedIdx > 0 {
				s.selectedIdx--
			}
		}
		return true
	case "enter":
		if len(s.schedules) > 0 {
			s.enterEditMode()
		}
		return true
	case "r":
		if len(s.schedules) > 0 {
			s.pendingTrigger = true
		}
		return true
	case "n":
		s.EnterCreateMode(s.createPath)
		return true
	}
	return true
}

func (s *SchedulePane) enterEditMode() {
	sched := s.schedules[s.selectedIdx]

	name := textinput.New()
	name.SetValue(sched.Name)
	name.CharLimit = 64
	name.Focus()

	prompt := textarea.New()
	prompt.ShowLineNumbers = false
	prompt.Prompt = ""
	prompt.Blur()
	prompt.FocusedStyle.CursorLine = lipgloss.NewStyle()
	prompt.CharLimit = 0
	prompt.MaxHeight = 0
	prompt.SetValue(sched.Prompt)

	cron := textinput.New()
	cron.SetValue(sched.CronExpr)
	cron.CharLimit = 64
	cron.Blur()

	path := textinput.New()
	path.SetValue(sched.ProjectPath)
	path.CharLimit = 256
	path.Blur()

	s.editName = name
	s.editPrompt = prompt
	s.editCron = cron
	s.editPath = path
	s.focusIndex = 0
	s.editing = true
}

func (s *SchedulePane) handleEditMode(msg tea.KeyMsg) bool {
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
				s.schedules[s.selectedIdx].Name = s.editName.Value()
				s.schedules[s.selectedIdx].Prompt = s.editPrompt.Value()
				s.schedules[s.selectedIdx].CronExpr = s.editCron.Value()
				s.schedules[s.selectedIdx].ProjectPath = s.editPath.Value()
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

func (s *SchedulePane) updateEditFocus() {
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

// String renders the schedule pane.
func (s *SchedulePane) String() string {
	if s.editing || s.creating {
		return s.renderEditMode()
	}
	return s.renderListMode()
}

func (s *SchedulePane) renderListMode() string {
	tStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	enabledStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#36CFC9"))
	disabledStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	detailStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	promptStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF"))
	sepLineStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#3C3C3C"))

	var b strings.Builder
	b.WriteString(tStyle.Render("Scheduled Tasks"))
	b.WriteString("\n\n")

	if len(s.schedules) == 0 {
		b.WriteString(disabledStyle.Render("  No schedules. Press s to create one."))
		b.WriteString("\n")
	}

	// Available width for word-wrapping prompt text (account for indent)
	wrapWidth := s.width - 6
	if wrapWidth < 20 {
		wrapWidth = 20
	}

	for i, sched := range s.schedules {
		if i > 0 {
			// Visual separator between schedules
			sep := strings.Repeat("─", wrapWidth)
			b.WriteString("  " + sepLineStyle.Render(sep) + "\n")
		}

		status := "[✓]"
		style := enabledStyle
		if !sched.Enabled {
			status = "[✗]"
			style = disabledStyle
		}

		isSelected := i == s.selectedIdx
		var header string
		if sched.Name != "" {
			header = fmt.Sprintf("%s %s  %s", status, sched.Name, sched.CronExpr)
		} else {
			header = fmt.Sprintf("%s %s", status, sched.CronExpr)
		}

		if isSelected && s.hasFocus {
			b.WriteString(selectedStyle.Render("▸ " + header))
		} else {
			b.WriteString(style.Render("  " + header))
		}
		b.WriteString("\n")

		// Full prompt text, word-wrapped
		wrapped := schedPaneWordWrap(sched.Prompt, wrapWidth)
		for _, line := range wrapped {
			b.WriteString(promptStyle.Render("    " + line))
			b.WriteString("\n")
		}

		// Program and last run info for all items
		lastRun := "never"
		if sched.LastRunAt != nil {
			lastRun = sched.LastRunAt.Format("Jan 02 15:04")
		}
		detail := fmt.Sprintf("    %s • last: %s", sched.Program, lastRun)
		if sched.LastRunStatus != "" {
			detail += " (" + sched.LastRunStatus + ")"
		}
		b.WriteString(detailStyle.Render(detail))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if s.hasFocus {
		b.WriteString(hintStyle.Render("n new • enter edit • r run now • x toggle • D delete • esc back"))
	} else {
		b.WriteString(hintStyle.Render("enter to focus and edit schedules"))
	}

	return b.String()
}

func (s *SchedulePane) renderEditMode() string {
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
		b.WriteString(editTitleStyle.Render("New Schedule"))
	} else {
		sched := s.schedules[s.selectedIdx]
		b.WriteString(editTitleStyle.Render(fmt.Sprintf("Edit Schedule %s", sched.ID)))
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
		submitLabel = " Schedule "
	}
	if s.focusIndex == 4 {
		b.WriteString("       " + focusedButtonStyle.Render(submitLabel))
	} else {
		b.WriteString("       " + buttonStyle.Render(submitLabel))
	}

	return b.String()
}

// schedPaneWordWrap wraps text to fit within maxWidth, breaking on word boundaries.
func schedPaneWordWrap(text string, maxWidth int) []string {
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
