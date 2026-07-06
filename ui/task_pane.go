package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
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

// taskPlaceholderStyle renders form placeholders faint so an example (the
// cron "e.g. 0 9 * * 1-5") can never be mistaken for a typed value.
var taskPlaceholderStyle = lipgloss.NewStyle().
	Faint(true).
	Foreground(lipgloss.AdaptiveColor{Light: "#B5B0B0", Dark: "#5C5757"})

// taskFormMoreStyle dims the ↑/↓ markers flagging fields scrolled out of a
// height-clamped edit form (#1098).
var taskFormMoreStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))

// Edit-form focus stops, in tab order. The form is grouped: Essentials
// (name, trigger, prompt) then Delivery (target session, path, program).
// The trigger is a two-step stop: a cron|watch type selector followed by the
// matching value input — only the selected trigger's field is shown, which
// makes the exactly-one-trigger contract (#782) structural instead of a
// validation error.
const (
	taskFocusName         = iota
	taskFocusTrigger      // trigger-type selector: cron | watch
	taskFocusTriggerValue // cron expression or watch command, per the selector
	taskFocusPrompt
	taskFocusTarget
	taskFocusPath
	taskFocusProgram
	taskFocusSave
	taskFocusCount
)

// TaskPane renders an inline task editor in the right pane.
type TaskPane struct {
	tasks       []task.Task
	selectedIdx int

	// Edit mode
	editing    bool
	editName   textinput.Model
	editPrompt textarea.Model
	// Trigger state: editTriggerIsWatch selects which of the two buffers is
	// shown, edited, and saved. Both buffers stay alive so flipping the
	// selector back and forth never loses typed input; save resolves to
	// exactly one via triggerValues.
	editTriggerIsWatch bool
	editCron           textinput.Model
	editWatch          textinput.Model
	editTarget         textinput.Model
	editPath           textinput.Model
	// Program selector state. editProgramOptions is the list of choices shown
	// inline (index 0 is always the "use config default" entry, followed by
	// tmux.SupportedPrograms). Per-task Program is restricted to the agent
	// enum (#658); per-task paths-with-flags are out of scope.
	editProgramOptions []string
	editProgramIdx     int
	editError          string // last validation error shown to the user
	editErrorField     int    // focus stop the error is rendered under (-1 = none)
	focusIndex         int
	// formScroll is the edit form's line offset when the pane is shorter than
	// the rendered form (#1098): renderEditMode windows the form so the
	// focused field stays in view instead of clipping off the top.
	formScroll int

	// Create mode
	creating       bool
	createPath     string
	pendingCreate  bool
	pendingTrigger bool

	width, height int
	dirty         bool
	// dirtyIDs records which tasks the user actually edited (toggle/enabled or
	// field edit) since the pane was loaded, keyed by task ID. saveContentPaneState
	// persists ONLY these, so a save can't overwrite an unmodified task whose
	// on-disk copy a concurrent writer (CLI/daemon) changed while the pane was
	// open — the lost-update in #1213. Deletions stay tracked separately in
	// `deleted` (mirrors ConsumeDeleted).
	dirtyIDs map[string]bool
	deleted  []task.Task
	hasFocus bool
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

// The TaskPane's task-list, selection, dirty-tracking, and focus/mode state
// accessors live in task_pane_state.go (SetTasks, GetTasks, SelectTask,
// ConsumeDirty, ConsumeDeleted, IsDirty, focus/mode getters). This file keeps
// the create/edit form, rendering, and key handling.

// initForm builds the shared create/edit field set. A nil task initializes an
// empty create form (path prefilled with defaultPath); otherwise the fields
// are seeded from the task being edited.
func (s *TaskPane) initForm(tsk *task.Task, defaultPath string) {
	name := textinput.New()
	name.Placeholder = "Task name"
	name.PlaceholderStyle = taskPlaceholderStyle
	name.CharLimit = 64
	name.Focus()

	prompt := textarea.New()
	prompt.ShowLineNumbers = false
	prompt.Prompt = ""
	prompt.Blur()
	prompt.FocusedStyle.CursorLine = lipgloss.NewStyle()
	prompt.FocusedStyle.Placeholder = taskPlaceholderStyle
	prompt.BlurredStyle.Placeholder = taskPlaceholderStyle
	prompt.CharLimit = 0
	prompt.MaxHeight = 0

	// The cron placeholder is an EXAMPLE, not a prefilled value: the "e.g."
	// prefix plus the faint placeholder style keep it visually distinct from
	// typed input, so an untouched field reads as empty (play-test on #1096).
	cron := textinput.New()
	cron.Placeholder = "e.g. 0 9 * * 1-5"
	cron.PlaceholderStyle = taskPlaceholderStyle
	cron.CharLimit = 64
	cron.Blur()

	watch := textinput.New()
	watch.Placeholder = "long-running cmd; 1 stdout line = 1 event"
	watch.PlaceholderStyle = taskPlaceholderStyle
	watch.CharLimit = 256
	watch.Blur()

	target := textinput.New()
	target.Placeholder = "(new session per run)"
	target.PlaceholderStyle = taskPlaceholderStyle
	target.CharLimit = 64
	target.Blur()

	path := textinput.New()
	path.PlaceholderStyle = taskPlaceholderStyle
	path.CharLimit = 256
	path.Blur()

	if tsk != nil {
		name.SetValue(tsk.Name)
		prompt.SetValue(tsk.Prompt)
		cron.SetValue(tsk.CronExpr)
		watch.SetValue(tsk.WatchCmd)
		target.SetValue(tsk.TargetSession)
		path.SetValue(tsk.ProjectPath)
		s.editTriggerIsWatch = tsk.IsWatch()
		s.setProgramFromValue(tsk.Program)
	} else {
		path.SetValue(defaultPath)
		s.editTriggerIsWatch = false
		s.setProgramFromValue("")
	}

	s.editName = name
	s.editPrompt = prompt
	s.editCron = cron
	s.editWatch = watch
	s.editTarget = target
	s.editPath = path
	s.focusIndex = taskFocusName
	s.editError = ""
	s.editErrorField = -1
	s.formScroll = 0
}

// EnterCreateMode initializes empty edit fields for creating a new task. An
// empty defaultPath (e.g. the in-pane "n" handler before any explicit create
// entry set s.createPath) falls back to the current working directory so the
// path field is prefilled with a sensible, usually-valid value rather than an
// empty string the #924 path validation would immediately reject.
func (s *TaskPane) EnterCreateMode(defaultPath string) {
	if defaultPath == "" {
		if cwd, err := os.Getwd(); err == nil {
			defaultPath = cwd
		}
	}
	s.createPath = defaultPath
	s.initForm(nil, defaultPath)
	s.creating = true
	s.hasFocus = true
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

// triggerValues resolves the two trigger buffers to the exactly-one contract:
// only the selected trigger type's value is returned; the inactive buffer is
// discarded regardless of content, so a save can never produce both.
func (s *TaskPane) triggerValues() (cron, watch string) {
	if s.editTriggerIsWatch {
		return "", strings.TrimSpace(s.editWatch.Value())
	}
	return strings.TrimSpace(s.editCron.Value()), ""
}

// HasPendingCreate returns true if a new task was submitted and needs saving.
func (s *TaskPane) HasPendingCreate() bool {
	return s.pendingCreate
}

// ConsumePendingCreate returns the submitted create data and clears the pending flag.
// program is the user-supplied program override; empty means "use the caller's default".
func (s *TaskPane) ConsumePendingCreate() (name, prompt, cron, watchCmd, targetSession, path, program string) {
	s.pendingCreate = false
	cronVal, watchVal := s.triggerValues()
	return s.editName.Value(), s.editPrompt.Value(),
		cronVal, watchVal,
		s.editTarget.Value(), s.editPath.Value(), s.programValue()
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

// ScrollUp moves the selection up one row. Used by scroll keys and mouse wheel
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
			s.markTaskDirty(s.tasks[s.selectedIdx].ID)
		}
		return true
	case "D":
		if len(s.tasks) > 0 {
			deleted := s.tasks[s.selectedIdx]
			s.deleted = append(s.deleted, deleted)
			s.tasks = append(s.tasks[:s.selectedIdx], s.tasks[s.selectedIdx+1:]...)
			// A task queued for deletion must not also be in the update set:
			// removeTaskThroughDaemon handles it, and an update on a just-removed
			// task would log a spurious not-found error.
			delete(s.dirtyIDs, deleted.ID)
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
	s.initForm(&tsk, "")
	s.editing = true
}

// EnterEditSelected drops straight into the edit form for the currently
// selected task, no-op'ing when the list is empty (so an empty overlay stays
// in list mode where `n` creates the first task). It bounds-guards the
// selected index that the unexported enterEditMode assumes, letting the
// overlay open a task directly into its config in a single action (#1249).
func (s *TaskPane) EnterEditSelected() {
	if len(s.tasks) == 0 {
		return
	}
	s.enterEditMode()
}

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

// String renders the task pane.
func (s *TaskPane) String() string {
	if s.editing || s.creating {
		return s.renderEditMode()
	}
	return s.renderListMode()
}

// watchTaskStatus derives the supervision state shown for a watch task from
// the fields the daemon persists (#782 phase 2): the watcher supervisor
// records "stopped" (script exited 0) and "errored" (crash loop) in
// LastRunStatus — since #797 the latter as "errored: <why>", which the
// errored detail row renders in full; any other value on an enabled watch
// task means the daemon (re-)arms its watcher on start/reload, i.e.
// "watching". A disabled watch task has no watcher, so it reads "stopped".
func watchTaskStatus(tsk task.Task) string {
	if !tsk.Enabled {
		return "stopped"
	}
	switch {
	case tsk.LastRunStatus == "stopped":
		return "stopped"
	case tsk.LastRunStatus == "errored" || strings.HasPrefix(tsk.LastRunStatus, "errored:"):
		return "errored"
	}
	return "watching"
}

// taskTriggerSummary is the one-line trigger column for a list row: the cron
// expression, or the watch command with its supervision status.
func taskTriggerSummary(tsk task.Task) string {
	if tsk.IsWatch() {
		return fmt.Sprintf("watch: %s [%s]", tsk.WatchCmd, watchTaskStatus(tsk))
	}
	if tsk.CronExpr == "" {
		return "(no trigger)"
	}
	return tsk.CronExpr
}

// taskDeliverySummary is the one-line delivery column: where a fire lands.
func taskDeliverySummary(tsk task.Task) string {
	if tsk.TargetSession != "" {
		return "→ " + tsk.TargetSession
	}
	return "new session"
}

func (s *TaskPane) renderListMode() string {
	tStyle := lipgloss.NewStyle().Bold(true).Foreground(AccentColor)
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFCC00"))
	enabledStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#36CFC9"))
	disabledStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9C9494"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	detailStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F7A7A"))
	erroredStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))

	var b strings.Builder
	b.WriteString(tStyle.Render("Tasks"))
	b.WriteString("\n\n")

	if len(s.tasks) == 0 {
		b.WriteString(disabledStyle.Render("  No tasks. Press n to create one."))
		b.WriteString("\n")
	}

	// Width available to the indented detail lines under the selected row.
	detailWidth := s.width - 8
	if detailWidth < 20 {
		detailWidth = 20
	}

	for i, tsk := range s.tasks {
		status := "[✓]"
		style := enabledStyle
		if !tsk.Enabled {
			status = "[✗]"
			style = disabledStyle
		}

		// One line per task: status, name, trigger, delivery — ellipsized to
		// the pane width so a long name/cron column marks its cut instead of
		// being hard-clamped.
		parts := []string{status}
		if tsk.Name != "" {
			parts = append(parts, tsk.Name)
		}
		parts = append(parts, taskTriggerSummary(tsk), taskDeliverySummary(tsk))
		header := strings.Join(parts, "  ")

		isSelected := i == s.selectedIdx
		if isSelected && s.hasFocus {
			b.WriteString(selectedStyle.Render(fitLine("▸ "+header, s.width)))
		} else {
			b.WriteString(style.Render(fitLine("  "+header, s.width)))
		}
		b.WriteString("\n")

		// A crash-looped watcher gets its full #797 failure summary on a
		// detail line — the only always-on detail, and only when errored.
		if tsk.IsWatch() && strings.HasPrefix(tsk.LastRunStatus, "errored:") {
			b.WriteString(erroredStyle.Render("      " + tsk.LastRunStatus))
			b.WriteString("\n")
		}

		// The selected row expands with prompt + agent + last-run detail.
		if isSelected {
			if snippet := promptSnippet(tsk.Prompt, detailWidth); snippet != "" {
				b.WriteString(detailStyle.Render("      " + snippet))
				b.WriteString("\n")
			}
			lastRun := "never"
			if tsk.LastRunAt != nil {
				lastRun = tsk.LastRunAt.Format("Jan 02 15:04")
			}
			programLabel := tsk.Program
			if programLabel == "" {
				programLabel = programDefaultLabel
			}
			detail := fmt.Sprintf("      %s • last: %s", programLabel, lastRun)
			if tsk.LastRunStatus != "" {
				// The full errored summary already has its own line above;
				// keep this column short.
				statusLabel := tsk.LastRunStatus
				if strings.HasPrefix(statusLabel, "errored:") {
					statusLabel = "errored"
				}
				detail += " (" + statusLabel + ")"
			}
			b.WriteString(detailStyle.Render(detail))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	if s.hasFocus {
		b.WriteString(hintStyle.Render(fitLine(
			"↑/↓ select • n new • enter edit • r run now • x toggle • D delete • esc back", s.width)))
	} else {
		b.WriteString(hintStyle.Render(fitLine("enter to focus and edit tasks", s.width)))
	}

	return b.String()
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
	b.WriteString(hintStyle.Render(fitLine("tab next • shift+tab prev • enter save • esc cancel", s.width)))

	return s.clampFormToHeight(b.String(), focusStart, focusEnd)
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

// promptSnippet collapses a prompt to a single line truncated to maxWidth,
// for the selected list row's detail line.
func promptSnippet(prompt string, maxWidth int) string {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return ""
	}
	line := strings.Join(fields, " ")
	runes := []rune(line)
	if maxWidth > 1 && len(runes) > maxWidth {
		return string(runes[:maxWidth-1]) + "…"
	}
	return line
}
