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
	editPrompt textarea.Model
	editCron   textinput.Model
	editPath   textinput.Model
	focusIndex int // 0=prompt, 1=cron, 2=path, 3=save button

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
	}
}

// IsEditing returns true if in edit mode.
func (s *SchedulePane) IsEditing() bool {
	return s.editing
}

// HandleKeyPress processes a key press. Returns true if consumed.
func (s *SchedulePane) HandleKeyPress(msg tea.KeyMsg) bool {
	if !s.hasFocus {
		return false
	}
	if s.editing {
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
	}
	return true
}

func (s *SchedulePane) enterEditMode() {
	sched := s.schedules[s.selectedIdx]

	prompt := textarea.New()
	prompt.ShowLineNumbers = false
	prompt.Prompt = ""
	prompt.Focus()
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

	s.editPrompt = prompt
	s.editCron = cron
	s.editPath = path
	s.focusIndex = 0
	s.editing = true
}

func (s *SchedulePane) handleEditMode(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyTab:
		s.focusIndex = (s.focusIndex + 1) % 4
		s.updateEditFocus()
	case tea.KeyShiftTab:
		s.focusIndex = (s.focusIndex + 3) % 4
		s.updateEditFocus()
	case tea.KeyEsc:
		s.editing = false
	case tea.KeyEnter:
		if s.focusIndex == 3 {
			s.schedules[s.selectedIdx].Prompt = s.editPrompt.Value()
			s.schedules[s.selectedIdx].CronExpr = s.editCron.Value()
			s.schedules[s.selectedIdx].ProjectPath = s.editPath.Value()
			s.dirty = true
			s.editing = false
			return true
		}
		if s.focusIndex == 0 {
			s.editPrompt, _ = s.editPrompt.Update(msg)
		}
	default:
		switch s.focusIndex {
		case 0:
			s.editPrompt, _ = s.editPrompt.Update(msg)
		case 1:
			s.editCron, _ = s.editCron.Update(msg)
		case 2:
			s.editPath, _ = s.editPath.Update(msg)
		}
	}
	return true
}

func (s *SchedulePane) updateEditFocus() {
	if s.focusIndex == 0 {
		s.editPrompt.Focus()
		s.editCron.Blur()
		s.editPath.Blur()
	} else if s.focusIndex == 1 {
		s.editPrompt.Blur()
		s.editCron.Focus()
		s.editPath.Blur()
	} else if s.focusIndex == 2 {
		s.editPrompt.Blur()
		s.editCron.Blur()
		s.editPath.Focus()
	} else {
		s.editPrompt.Blur()
		s.editCron.Blur()
		s.editPath.Blur()
	}
}

// String renders the schedule pane.
func (s *SchedulePane) String() string {
	if s.editing {
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

	var b strings.Builder
	b.WriteString(tStyle.Render("Scheduled Tasks"))
	b.WriteString("\n\n")

	if len(s.schedules) == 0 {
		b.WriteString(disabledStyle.Render("  No schedules. Press s to create one."))
		b.WriteString("\n")
	}

	for i, sched := range s.schedules {
		status := "[✓]"
		style := enabledStyle
		if !sched.Enabled {
			status = "[✗]"
			style = disabledStyle
		}

		prompt := schedPaneTruncate(sched.Prompt, 40)
		isSelected := i == s.selectedIdx
		line := fmt.Sprintf("%s %s  %s", status, sched.CronExpr, prompt)

		if isSelected && s.hasFocus {
			b.WriteString(selectedStyle.Render("▸ " + line))
		} else {
			b.WriteString(style.Render("  " + line))
		}
		b.WriteString("\n")

		if isSelected {
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
	}

	b.WriteString("\n")
	if s.hasFocus {
		b.WriteString(hintStyle.Render("enter edit • x toggle • D delete • esc back"))
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
	s.editPrompt.SetWidth(inputWidth)
	if s.height > 0 {
		s.editPrompt.SetHeight(s.height / 4)
	}
	s.editCron.Width = inputWidth
	s.editPath.Width = inputWidth

	sched := s.schedules[s.selectedIdx]

	var b strings.Builder
	b.WriteString(editTitleStyle.Render(fmt.Sprintf("Edit Schedule %s", sched.ID)))
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

	submitButton := " Save "
	if s.focusIndex == 3 {
		b.WriteString("       " + focusedButtonStyle.Render(submitButton))
	} else {
		b.WriteString("       " + buttonStyle.Render(submitButton))
	}

	return b.String()
}

func schedPaneTruncate(str string, max int) string {
	runes := []rune(str)
	if len(runes) <= max {
		return str
	}
	return string(runes[:max-3]) + "..."
}
