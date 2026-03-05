package overlay

import (
	"claude-squad/schedule"
	"fmt"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ScheduleListOverlay is an interactive list overlay for managing scheduled tasks.
type ScheduleListOverlay struct {
	schedules   []schedule.Schedule
	selectedIdx int

	// Edit mode
	editing    bool
	editPrompt textarea.Model
	editCron   textinput.Model
	editPath   textinput.Model
	focusIndex int // 0=prompt, 1=cron, 2=path, 3=save button

	width, height int
	closed        bool
	dirty         bool
	deleted       []schedule.Schedule // deleted schedules for systemd cleanup
}

// NewScheduleListOverlay creates a new schedule list overlay with the given schedules.
func NewScheduleListOverlay(schedules []schedule.Schedule) *ScheduleListOverlay {
	return &ScheduleListOverlay{
		schedules: schedules,
		width:     60,
		height:    20,
	}
}

// SetSize sets the width and height of the overlay.
func (s *ScheduleListOverlay) SetSize(width, height int) {
	s.width = width
	s.height = height
}

// SetWidth sets the width of the overlay.
func (s *ScheduleListOverlay) SetWidth(width int) {
	s.width = width
}

// IsClosed returns true if the overlay should be dismissed.
func (s *ScheduleListOverlay) IsClosed() bool {
	return s.closed
}

// IsDirty returns true if schedules were modified.
func (s *ScheduleListOverlay) IsDirty() bool {
	return s.dirty
}

// GetSchedules returns the current list of schedules.
func (s *ScheduleListOverlay) GetSchedules() []schedule.Schedule {
	return s.schedules
}

// GetDeleted returns schedules that were deleted (for systemd cleanup).
func (s *ScheduleListOverlay) GetDeleted() []schedule.Schedule {
	return s.deleted
}

// HandleKeyPress processes a key press and updates overlay state.
// Returns true if the overlay should close.
func (s *ScheduleListOverlay) HandleKeyPress(msg tea.KeyMsg) bool {
	if s.editing {
		return s.handleEditMode(msg)
	}
	return s.handleNormalMode(msg)
}

func (s *ScheduleListOverlay) handleNormalMode(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc", "ctrl+c":
		s.closed = true
		return true
	case "up", "k":
		if s.selectedIdx > 0 {
			s.selectedIdx--
		}
	case "down", "j":
		if s.selectedIdx < len(s.schedules)-1 {
			s.selectedIdx++
		}
	case "x":
		if len(s.schedules) > 0 {
			s.schedules[s.selectedIdx].Enabled = !s.schedules[s.selectedIdx].Enabled
			s.dirty = true
		}
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
	case "enter":
		if len(s.schedules) > 0 {
			s.enterEditMode()
		}
	}
	return false
}

func (s *ScheduleListOverlay) enterEditMode() {
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

func (s *ScheduleListOverlay) handleEditMode(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyTab:
		s.focusIndex = (s.focusIndex + 1) % 4
		s.updateEditFocus()
		return false
	case tea.KeyShiftTab:
		s.focusIndex = (s.focusIndex + 3) % 4
		s.updateEditFocus()
		return false
	case tea.KeyEsc:
		s.editing = false
		return false
	case tea.KeyEnter:
		if s.focusIndex == 3 {
			// Save changes
			s.schedules[s.selectedIdx].Prompt = s.editPrompt.Value()
			s.schedules[s.selectedIdx].CronExpr = s.editCron.Value()
			s.schedules[s.selectedIdx].ProjectPath = s.editPath.Value()
			s.dirty = true
			s.editing = false
			return false
		}
		if s.focusIndex == 0 {
			s.editPrompt, _ = s.editPrompt.Update(msg)
		}
		return false
	default:
		switch s.focusIndex {
		case 0:
			s.editPrompt, _ = s.editPrompt.Update(msg)
		case 1:
			s.editCron, _ = s.editCron.Update(msg)
		case 2:
			s.editPath, _ = s.editPath.Update(msg)
		}
		return false
	}
}

func (s *ScheduleListOverlay) updateEditFocus() {
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

// Render renders the schedule list overlay.
func (s *ScheduleListOverlay) Render(opts ...WhitespaceOption) string {
	if s.editing {
		return s.renderEditMode()
	}
	return s.renderListMode()
}

func (s *ScheduleListOverlay) renderListMode() string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	enabledStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#36CFC9"))
	disabledStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	detailStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))

	content := titleStyle.Render("Scheduled Tasks") + "\n\n"

	if len(s.schedules) == 0 {
		content += disabledStyle.Render("  No schedules. Press s to create one.") + "\n"
	}

	for i, sched := range s.schedules {
		status := "[✓]"
		style := enabledStyle
		if !sched.Enabled {
			status = "[✗]"
			style = disabledStyle
		}

		prompt := schedTruncate(sched.Prompt, 40)
		isSelected := i == s.selectedIdx

		line := fmt.Sprintf("%s %s  %s", status, sched.CronExpr, prompt)

		if isSelected {
			content += selectedStyle.Render("▸ "+line) + "\n"
		} else {
			content += style.Render("  "+line) + "\n"
		}

		// Show details for selected schedule
		if isSelected {
			lastRun := "never"
			if sched.LastRunAt != nil {
				lastRun = sched.LastRunAt.Format("Jan 02 15:04")
			}
			detail := fmt.Sprintf("    %s • last: %s", sched.Program, lastRun)
			if sched.LastRunStatus != "" {
				detail += " (" + sched.LastRunStatus + ")"
			}
			content += detailStyle.Render(detail) + "\n"
		}
	}

	content += "\n"
	content += hintStyle.Render("enter edit • x toggle • D delete • esc close")

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Padding(1, 2).
		Width(s.width)

	return style.Render(content)
}

func (s *ScheduleListOverlay) renderEditMode() string {
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2)

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
	s.editPrompt.SetWidth(inputWidth)
	if s.height > 0 {
		s.editPrompt.SetHeight(s.height / 4)
	}
	s.editCron.Width = inputWidth
	s.editPath.Width = inputWidth

	sched := s.schedules[s.selectedIdx]
	content := editTitleStyle.Render(fmt.Sprintf("Edit Schedule %s", sched.ID)) + "\n"
	content += labelStyle.Render("Prompt:") + "\n"
	content += s.editPrompt.View() + "\n\n"
	content += labelStyle.Render("Cron:") + "  " + s.editCron.View() + "\n"
	content += labelStyle.Render("Path:") + "  " + s.editPath.View() + "\n\n"

	submitButton := " Save "
	if s.focusIndex == 3 {
		submitButton = focusedButtonStyle.Render(submitButton)
	} else {
		submitButton = buttonStyle.Render(submitButton)
	}
	content += "       " + submitButton

	return style.Render(content)
}

func schedTruncate(str string, max int) string {
	runes := []rune(str)
	if len(runes) <= max {
		return str
	}
	return string(runes[:max-3]) + "..."
}
