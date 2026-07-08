package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// validateForm enforces the shared create/edit form contract, mirroring
// `af tasks add` (api/tasks.go). The trigger-type selector already guarantees
// at most one trigger, so validation reduces to: a name, a non-empty value
// for the selected trigger, and — for cron tasks only — a valid cron
// expression plus a non-empty prompt. Watch tasks may omit the prompt: each
// event defaults to the raw emitted line. The project path is validated for
// BOTH trigger types (#924): a corrupt or non-existent repo path used to be
// saved silently and only surfaced when the scheduler fired. Returns the
// inline error and the focus stop to render it under, or ("", -1) when valid.
func (s *TaskPane) validateForm() (string, int) {
	if strings.TrimSpace(s.editName.Value()) == "" {
		return "name is required", taskFocusName
	}
	if s.editTriggerIsWatch {
		if strings.TrimSpace(s.editWatch.Value()) == "" {
			return "watch command is required", taskFocusTriggerValue
		}
	} else {
		cron := strings.TrimSpace(s.editCron.Value())
		if cron == "" {
			return "cron expression is required", taskFocusTriggerValue
		}
		if err := task.ValidateCronExpr(cron); err != nil {
			return fmt.Sprintf("invalid cron: %v", err), taskFocusTriggerValue
		}
		if strings.TrimSpace(s.editPrompt.Value()) == "" {
			return "prompt must be non-empty", taskFocusPrompt
		}
	}

	// Expand a leading ~ (filepath.Abs does not), resolve to absolute, and
	// require a real git repo — the same check RunTask/the watcher apply at
	// fire time (git.IsGitRepo). Persist the normalized value back into the
	// field so what we validate is exactly what gets stored (#924).
	rawPath := strings.TrimSpace(s.editPath.Value())
	if rawPath == "" {
		return "project path is required", taskFocusPath
	}
	absPath, err := filepath.Abs(config.ExpandTilde(rawPath))
	if err != nil {
		return fmt.Sprintf("invalid path: %v", err), taskFocusPath
	}
	if !git.IsGitRepo(absPath) {
		return fmt.Sprintf("%s is not a git repository", absPath), taskFocusPath
	}
	s.editPath.SetValue(absPath)
	return "", -1
}

func (s *TaskPane) handleEditMode(msg tea.KeyMsg) bool {
	if s.editing && !s.creating {
		switch msg.String() {
		case "r":
			s.runSelectedTask()
			return true
		case "x":
			s.toggleSelectedTask()
			return true
		case "D":
			s.deleteSelectedTask()
			s.editing = false
			s.editError = ""
			s.editErrorField = -1
			return true
		}
	}
	switch msg.Type {
	case tea.KeyTab:
		s.focusIndex = (s.focusIndex + 1) % taskFocusCount
		s.updateEditFocus()
	case tea.KeyShiftTab:
		s.focusIndex = (s.focusIndex + taskFocusCount - 1) % taskFocusCount
		s.updateEditFocus()
	case tea.KeyEsc, tea.KeyCtrlC:
		s.editing = false
		s.creating = false
		s.editError = ""
		s.editErrorField = -1
	case tea.KeyEnter:
		// The prompt keeps Enter for newlines; every other field submits, so
		// the footer's blanket "enter save" holds wherever focus is (#1098 —
		// Enter on the Name field used to be a dead key).
		if s.focusIndex == taskFocusPrompt {
			s.editPrompt, _ = s.editPrompt.Update(msg)
			return true
		}
		if errMsg, errField := s.validateForm(); errMsg != "" {
			s.editError = errMsg
			s.editErrorField = errField
			// Land focus on the offending field so its inline error is in
			// view even when the clamped form has it scrolled off-screen.
			if errField >= 0 && errField != s.focusIndex {
				s.focusIndex = errField
				s.updateEditFocus()
			}
			return true
		}
		s.editError = ""
		s.editErrorField = -1
		cron, watch := s.triggerValues()
		if s.creating {
			s.pendingCreate = true
			s.creating = false
		} else {
			// Mirror the create path (app.handleTaskCreate): expand a
			// leading ~ then resolve to an absolute form so an empty or
			// relative value behaves the same when the scheduler fires
			// as it does in the TUI trigger (#641, #924). validateForm
			// already normalized editPath, so this is idempotent.
			absPath, err := filepath.Abs(config.ExpandTilde(s.editPath.Value()))
			if err != nil {
				s.editError = fmt.Sprintf("invalid path: %v", err)
				s.editErrorField = taskFocusPath
				return true
			}
			s.tasks[s.selectedIdx].Name = s.editName.Value()
			s.tasks[s.selectedIdx].Prompt = s.editPrompt.Value()
			s.tasks[s.selectedIdx].CronExpr = cron
			s.tasks[s.selectedIdx].WatchCmd = watch
			s.tasks[s.selectedIdx].TargetSession = s.editTarget.Value()
			s.tasks[s.selectedIdx].ProjectPath = absPath
			s.tasks[s.selectedIdx].Program = s.programValue()
			s.markTaskDirty(s.tasks[s.selectedIdx].ID)
			s.editing = false
		}
		return true
	default:
		switch s.focusIndex {
		case taskFocusName:
			s.editName, _ = s.editName.Update(msg)
		case taskFocusTrigger:
			s.handleTriggerKey(msg)
		case taskFocusTriggerValue:
			if s.editTriggerIsWatch {
				s.editWatch, _ = s.editWatch.Update(msg)
			} else {
				s.editCron, _ = s.editCron.Update(msg)
			}
		case taskFocusPrompt:
			s.editPrompt, _ = s.editPrompt.Update(msg)
		case taskFocusTarget:
			s.editTarget, _ = s.editTarget.Update(msg)
		case taskFocusPath:
			s.editPath, _ = s.editPath.Update(msg)
		case taskFocusProgram:
			s.handleProgramKey(msg)
		}
	}
	return true
}

// handleTriggerKey moves the trigger-type selector: cron sits on the left,
// watch on the right. Other keys are ignored so the selector behaves like a
// list, not a free-text input.
func (s *TaskPane) handleTriggerKey(msg tea.KeyMsg) {
	switch msg.String() {
	case "left", "h", "up", "k":
		s.editTriggerIsWatch = false
	case "right", "l", "down", "j":
		s.editTriggerIsWatch = true
	}
}

// handleProgramKey moves the selector cursor when the Program field has focus.
// Left/h and right/l step the selection (up/down work too); other keys are
// ignored so the selector behaves like a list, not a free-text input (#492).
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
	s.editWatch.Blur()
	s.editTarget.Blur()
	s.editPath.Blur()

	switch s.focusIndex {
	case taskFocusName:
		s.editName.Focus()
	case taskFocusTriggerValue:
		if s.editTriggerIsWatch {
			s.editWatch.Focus()
		} else {
			s.editCron.Focus()
		}
	case taskFocusPrompt:
		s.editPrompt.Focus()
	case taskFocusTarget:
		s.editTarget.Focus()
	case taskFocusPath:
		s.editPath.Focus()
	}
}

func (s *TaskPane) renderEditMode() string {
	editTitleStyle := lipgloss.NewStyle().
		Foreground(AccentColor).
		Bold(true).
		MarginBottom(1)

	labelStyle := lipgloss.NewStyle().Bold(true)
	groupStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7F7A7A"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	buttonStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	focusedButtonStyle := buttonStyle.
		Background(AccentColor).
		Foreground(lipgloss.Color("0"))

	inputWidth := s.width - 9
	if inputWidth < 1 {
		inputWidth = 1
	}
	s.editName.Width = inputWidth
	promptWidth := s.width
	if promptWidth < 1 {
		promptWidth = 1
	}
	s.editPrompt.SetWidth(promptWidth)
	if s.height > 0 {
		s.editPrompt.SetHeight(s.height / 4)
	}
	s.editCron.Width = inputWidth
	s.editWatch.Width = inputWidth
	s.editTarget.Width = inputWidth
	s.editPath.Width = inputWidth

	// The prompt's role depends on the trigger: cron tasks require it, watch
	// tasks default each event to the raw emitted line.
	if s.editTriggerIsWatch {
		s.editPrompt.Placeholder = "(optional) {{line}} expands to the event line"
	} else {
		s.editPrompt.Placeholder = "Enter task prompt..."
	}

	label := func(text string) string {
		return labelStyle.Render(fmt.Sprintf("%-9s", text))
	}
	// fieldErr renders the inline validation message directly under the
	// field it belongs to, instead of a global error footer — ellipsized to
	// the pane width so a long message (e.g. a full path in "not a git
	// repository") marks its cut.
	fieldErr := func(field int) string {
		if s.editError != "" && s.editErrorField == field {
			return errorStyle.Render(fitLine("  ! "+s.editError, s.width)) + "\n"
		}
		return ""
	}

	var b strings.Builder
	// Track the focused stop's line range while building, so the form can be
	// windowed to the pane height with the focused field scrolled into view
	// (#1098): below ~15 terminal rows the form is taller than the overlay,
	// and without a window the top fields clip off-screen while still holding
	// focus. markStart/markEnd bracket each focus stop's lines (including its
	// inline error, so validation messages scroll into view too).
	focusStart, focusEnd := -1, -1
	markStart := func(stop int) {
		if stop == s.focusIndex {
			focusStart = strings.Count(b.String(), "\n")
		}
	}
	markEnd := func(stop int) {
		if stop == s.focusIndex {
			focusEnd = strings.Count(b.String(), "\n")
		}
	}

	if s.creating {
		b.WriteString(editTitleStyle.Render("New Task"))
	} else {
		tsk := s.tasks[s.selectedIdx]
		b.WriteString(editTitleStyle.Render(fmt.Sprintf("Edit Task %s", tsk.ID)))
	}
	b.WriteString("\n")

	b.WriteString(groupStyle.Render("Essentials"))
	b.WriteString("\n")
	markStart(taskFocusName)
	b.WriteString(label("Name:"))
	b.WriteString(s.editName.View())
	b.WriteString("\n")
	b.WriteString(fieldErr(taskFocusName))
	markEnd(taskFocusName)
	markStart(taskFocusTrigger)
	b.WriteString(label("Trigger:"))
	b.WriteString(s.renderTriggerSelector())
	b.WriteString("\n")
	markEnd(taskFocusTrigger)
	markStart(taskFocusTriggerValue)
	if s.editTriggerIsWatch {
		b.WriteString(label("Watch:"))
		b.WriteString(s.editWatch.View())
	} else {
		b.WriteString(label("Cron:"))
		b.WriteString(s.editCron.View())
	}
	b.WriteString("\n")
	b.WriteString(fieldErr(taskFocusTriggerValue))
	markEnd(taskFocusTriggerValue)
	markStart(taskFocusPrompt)
	b.WriteString(labelStyle.Render("Prompt:"))
	if s.editTriggerIsWatch {
		b.WriteString(hintStyle.Render(" (optional)"))
	}
	b.WriteString("\n")
	b.WriteString(s.editPrompt.View())
	b.WriteString("\n")
	b.WriteString(fieldErr(taskFocusPrompt))
	markEnd(taskFocusPrompt)
	b.WriteString("\n")

	b.WriteString(groupStyle.Render("Delivery"))
	b.WriteString("\n")
	markStart(taskFocusTarget)
	b.WriteString(label("Target:"))
	b.WriteString(s.editTarget.View())
	b.WriteString("\n")
	markEnd(taskFocusTarget)
	markStart(taskFocusPath)
	b.WriteString(label("Path:"))
	b.WriteString(s.editPath.View())
	b.WriteString("\n")
	b.WriteString(fieldErr(taskFocusPath))
	markEnd(taskFocusPath)
	markStart(taskFocusProgram)
	b.WriteString(label("Program:"))
	b.WriteString(s.renderProgramSelector())
	b.WriteString("\n")
	markEnd(taskFocusProgram)
	b.WriteString("\n")

	submitLabel := " Save "
	if s.creating {
		submitLabel = " Create "
	}
	markStart(taskFocusSave)
	if s.focusIndex == taskFocusSave {
		b.WriteString(focusedButtonStyle.Render(submitLabel))
	} else {
		b.WriteString(buttonStyle.Render(submitLabel))
	}
	b.WriteString("\n")
	markEnd(taskFocusSave)
	b.WriteString("\n")
	if s.creating {
		hint := "tab/shift+tab fields • enter create • esc cancel"
		if s.width > 0 && lipgloss.Width(hint) > s.width {
			hint = "tab fields • enter • esc cancel"
		}
		b.WriteString(hintStyle.Render(fitLine(hint, s.width)))
	} else {
		hint := "tab/shift+tab fields • enter save"
		if s.width > 0 && lipgloss.Width(hint) > s.width {
			hint = "tab fields • enter save"
		}
		b.WriteString(hintStyle.Render(fitLine(hint, s.width)))
		b.WriteString("\n")
		actions := "r run now • x toggle • D delete • esc list"
		if s.width > 0 && lipgloss.Width(actions) > s.width {
			actions = "r run • x toggle • D del • esc list"
		}
		b.WriteString(hintStyle.Render(fitLine(actions, s.width)))
	}

	return fitBlockToSize(s.clampFormToHeight(b.String(), focusStart, focusEnd), s.width, 0, 0)
}

// clampFormToHeight windows the rendered edit form to the pane height, keeping
// the focused field's lines in view (#1098). The key-hint footer stays pinned
// as the last visible line so tab/esc are always discoverable, and dim ↑/↓
// markers flag fields scrolled out of the window. focusStart/focusEnd are the
// focused stop's line range, end-exclusive; a form that already fits renders
// unchanged.
func (s *TaskPane) clampFormToHeight(content string, focusStart, focusEnd int) string {
	maxH := s.height
	lines := strings.Split(content, "\n")
	if maxH <= 0 || len(lines) <= maxH {
		return content
	}
	if maxH < 3 {
		maxH = 3
	}
	hint := lines[len(lines)-1]
	body := lines[:len(lines)-1]
	visible := maxH - 1
	if visible > len(body) {
		// The raised floor can exceed a short body (degenerate heights); a
		// window larger than the body would slice past its end.
		visible = len(body)
	}

	off := s.formScroll
	if off > len(body)-visible {
		off = len(body) - visible
	}
	if off < 0 {
		off = 0
	}
	if focusStart >= 0 {
		// Bottom edge first, then top: when the range is taller than the
		// window the field's first line wins.
		if focusEnd > off+visible {
			off = focusEnd - visible
		}
		if focusStart < off {
			off = focusStart
		}
	}
	s.formScroll = off

	win := make([]string, visible)
	copy(win, body[off:off+visible])
	inFocusRange := func(line int) bool {
		return focusStart >= 0 && line >= focusStart && line < focusEnd
	}
	if off > 0 && !inFocusRange(off) {
		win[0] = taskFormMoreStyle.Render("  ↑ more")
	}
	if last := off + visible - 1; last < len(body)-1 && !inFocusRange(last) {
		win[visible-1] = taskFormMoreStyle.Render("  ↓ more")
	}
	return strings.Join(append(win, hint), "\n")
}

// renderTriggerSelector renders the inline cron|watch type selector on one
// line. The selected type gets the ▸ marker (yellow when focused, dim
// otherwise), matching the Program selector's treatment.
func (s *TaskPane) renderTriggerSelector() string {
	focused := s.focusIndex == taskFocusTrigger
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	dimSelectedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))

	option := func(name string, sel bool) string {
		switch {
		case sel && focused:
			return selectedStyle.Render("▸ " + name)
		case sel:
			return dimSelectedStyle.Render("▸ " + name)
		default:
			return normalStyle.Render("  " + name)
		}
	}
	out := option("cron", !s.editTriggerIsWatch) + "  " + option("watch", s.editTriggerIsWatch)
	if focused {
		out += hintStyle.Render("   ←/→ switch")
	}
	return out
}

// renderProgramSelector renders the agent selector as a single line showing
// the current choice; ←/→ steps through the options when focused.
func (s *TaskPane) renderProgramSelector() string {
	focused := s.focusIndex == taskFocusProgram
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	dimSelectedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))

	value := programDefaultLabel
	if s.editProgramIdx >= 0 && s.editProgramIdx < len(s.editProgramOptions) {
		value = s.editProgramOptions[s.editProgramIdx]
	}
	if focused {
		return selectedStyle.Render("◂ "+value+" ▸") + hintStyle.Render("   ←/→ change agent")
	}
	return dimSelectedStyle.Render(value)
}
