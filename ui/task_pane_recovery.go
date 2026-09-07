package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// RestoreCreateMode keeps every field after a rejected create. SetTasks closes
// the form only once the daemon has committed the mutation.
func (s *TaskPane) RestoreCreateMode() {
	s.creating = true
	s.pendingCreate = false
	s.hasFocus = true
	s.updateEditFocus()
}

// SetUnavailable distinguishes a rejected load from an empty response.
func (s *TaskPane) SetUnavailable(err error) bool {
	previous := s.unavailable
	s.unavailable = ""
	if err != nil {
		s.unavailable = err.Error()
	}
	return previous != s.unavailable
}

// renderListRecovery keeps the task manager's identity and exit affordance
// pinned around the centered P4 empty/failure treatment. These states are still
// list mode: n and Esc remain live, and the modal must advertise that fact.
func (s *TaskPane) renderListRecovery(condition, detail, action string, failed bool) string {
	const headerRows = 2
	const footerRows = 1
	bodyHeight := s.height - headerRows - footerRows
	if bodyHeight < 0 {
		bodyHeight = 0
	}

	var b strings.Builder
	b.WriteString(DialogTitleStyle().Render("Tasks"))
	b.WriteString("\n\n")
	body := DialogRecoveryScreen(layout.Rect{W: s.width, H: bodyHeight}, condition, detail, action, failed)
	b.WriteString(body)
	if body != "" {
		b.WriteString("\n")
	}
	b.WriteString(DialogHintStyle().Render(fitLine(s.listModeHint(), s.width)))
	return fitBlockToSize(b.String(), s.width, s.height, footerRows)
}

func (s *TaskPane) listModeHint() string {
	if !s.hasFocus {
		return "enter to focus and edit tasks"
	}

	hint := "↑/↓ select · n new · enter edit · r run now · x toggle · D delete · esc back"
	short := "r run now · x toggle · D delete · ? back · esc"
	// A watch task can't be manually run (#1758): drop "r run now" so the
	// hint never advertises an action that always fails.
	if s.selectedTaskIsWatch() {
		hint = "↑/↓ select · n new · enter edit · x toggle · D delete · esc back"
		short = "x toggle · D delete · ? back · esc"
	}
	if !s.showActions {
		hint = "enter edit · n new · ? actions · esc back"
		short = "enter edit · ? actions · esc"
	}
	if s.width > 0 && lipgloss.Width(hint) > s.width {
		return short
	}
	return hint
}
