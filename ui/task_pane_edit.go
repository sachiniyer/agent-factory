package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"

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
		// The schedule picker owns the generated cron and its own validation
		// (a weekly needs a day, custom needs a valid expression); it round-
		// trips the same task.ValidateCronExpr gate the raw field used (#2057).
		if msg := s.schedule.validate(); msg != "" {
			return msg, taskFocusTriggerValue
		}
		if strings.TrimSpace(s.editPrompt.Value()) == "" {
			return "prompt must be non-empty", taskFocusPrompt
		}
	}

	// The cap field is editable only while the shape can carry one, so an
	// inapplicable buffer never reaches this check — and an applicable one is
	// parsed by the same rule the web form uses (task.ParseCapInput, pinned by
	// the shared vectors in task/testdata/cap_vectors.json).
	if s.capApplies() {
		if _, ok := task.ParseCapInput(s.editMaxRuns.Value()); !ok {
			return "max runs must be a non-negative integer", taskFocusMaxRuns
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
	absPath, err := ResolveTaskProjectPath(rawPath)
	if err != nil {
		return fmt.Sprintf("invalid path: %v", err), taskFocusPath
	}
	if isRemoteTaskTarget() {
		// The path is on the daemon's host: this machine's git cannot vouch for
		// it, and the daemon refuses a non-repository when it takes the task.
		s.editPath.SetValue(absPath)
		return "", -1
	}
	if err := git.CheckGitRepo(absPath); err != nil {
		if config.RepoProbeUnanswered(err) {
			// No path in this one: the error renders directly under the field
			// holding it, and the overlay is narrow enough that repeating it
			// truncates the actionable half away (play-tested).
			return "could not check — git did not answer; try again", taskFocusPath
		}
		return fmt.Sprintf("%s is not a git repository", absPath), taskFocusPath
	}
	s.editPath.SetValue(absPath)
	return "", -1
}

// textFocusStop reports whether the focused form stop accepts free-text input.
// The list verbs r/x/D route only at selector/button stops, so a text field
// always receives the rune as a typed character rather than triggering the
// verb: typing r, x, or D into Name, the watch command, the prompt, the
// target session, or the path must insert the character. The cron trigger
// value is a text stop too — although its numeric cells ignore letters, the
// schedule picker's Custom/raw-cron cell is a free-text input that r/x/D must
// reach — so the verbs never fire while the trigger value is focused.
func (s *TaskPane) textFocusStop() bool {
	return textStopAt(s.focusIndex, s.capApplies())
}

// textStopAt is the pure form of textFocusStop. capApplies is whether the form's
// current shape carries a run cap, which decides if the cap row is a text stop.
func textStopAt(focusIndex int, capApplies bool) bool {
	switch focusIndex {
	case taskFocusName, taskFocusTriggerValue, taskFocusPrompt, taskFocusTarget, taskFocusHost, taskFocusPath:
		return true
	case taskFocusMaxRuns:
		// The cap row is a live text input only when the shape carries one;
		// inapplicable it renders a reason line and routes verbs like a
		// selector, matching the input gate in handleEditMode (#4180).
		return capApplies
	}
	return false
}

// tabsToVerbStop counts the Tab presses from the focused stop to the next
// selector/button stop where the list verbs fire. It walks the same forward
// wraparound KeyTab applies in handleEditMode, so the count the footer
// advertises is derived from the real stop order — it cannot silently lie if
// a text stop is inserted, removed, or reordered. Only meaningful at text
// stops; it always terminates because non-text stops exist.
func (s *TaskPane) tabsToVerbStop() int {
	for n := 1; n <= taskFocusCount; n++ {
		if !textStopAt((s.focusIndex+n)%taskFocusCount, s.capApplies()) {
			return n
		}
	}
	return 0
}

func (s *TaskPane) handleEditMode(msg tea.KeyMsg) bool {
	// Retain unsaved form text after a failed refresh, but do not act on the
	// stale selected task until a successful refresh restores availability.
	// In recovery the verb glyphs are inert at every stop — no run, toggle,
	// or delete, and not typed either — so a stale keystroke can't act on a
	// task the pane cannot safely touch; ordinary letters still type.
	if s.unavailable != "" && s.editing && !s.creating {
		switch msg.String() {
		case "r", "x", "D":
			return true
		}
	}
	// The list verbs stay reachable from selector/button stops so the #1249
	// one-step edit flow does not hide them (#1288). They are routed ONLY at
	// non-text stops: when Name, the watch command, the prompt, the target
	// session, the path, or the cron picker's raw-cron cell is focused, r/x/D
	// must be typed characters, not triggers. D is deliberately absent here —
	// an edit-form delete is confirmed by the app layer (handleStateTasks)
	// exactly like list mode, so a text field can type a capital D and no
	// delete is ever immediate.
	if s.editing && !s.creating && !s.textFocusStop() {
		switch msg.String() {
		case "r":
			s.runSelectedTask()
			return true
		case "x":
			s.toggleSelectedTask()
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
		if s.unavailable != "" && s.editing && !s.creating {
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
			// A discard that removed the form's row closes the form, so this
			// branch should never run with the cursor out of range. The guard
			// keeps a future row-removal path from writing the form's stale
			// buffers into whatever row the cursor re-clamped onto (#4798
			// follow-up): consume the keystroke without saving.
			if !s.selectedTaskInRange() {
				return true
			}
			// Mirror the create path (app.handleTaskCreate): expand a
			// leading ~ then resolve to an absolute form so an empty or
			// relative value behaves the same when the scheduler fires
			// as it does in the TUI trigger (#641, #924). validateForm
			// already normalized editPath, so this is idempotent.
			absPath, err := ResolveTaskProjectPath(s.editPath.Value())
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
			s.tasks[s.selectedIdx].OnComplete = s.onCompleteValue()
			s.tasks[s.selectedIdx].MaxConcurrentRuns = s.capValue()
			s.tasks[s.selectedIdx].Host = strings.TrimSpace(s.editHost.Value())
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
				s.schedule.handleKey(msg)
			}
		case taskFocusPrompt:
			s.editPrompt, _ = s.editPrompt.Update(msg)
		case taskFocusTarget:
			s.editTarget, _ = s.editTarget.Update(msg)
		case taskFocusOnComplete:
			s.handleOnCompleteKey(msg)
		case taskFocusMaxRuns:
			// Same refusal as the lifecycle selector's: a shape that cannot
			// carry a cap must not accept digits for one either — its row
			// renders the reason instead of the input.
			if s.capApplies() {
				s.editMaxRuns, _ = s.editMaxRuns.Update(msg)
			}
		case taskFocusHost:
			s.editHost, _ = s.editHost.Update(msg)
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

// handleOnCompleteKey steps the spawned-session lifecycle selector, mirroring
// handleProgramKey: left/h and right/l move the choice (up/down too) and every
// other key is ignored, so the field behaves like a list rather than a text
// input.
//
// It refuses to move while a target session is set. The verb governs a session
// the task CREATES, and a target-session task delivers into one the user named
// for reuse, so there is nothing for it to govern (task.onCompleteApplies) — and
// a stored pair the daemon would reject (task.ValidateTrigger) must not be
// reachable from the form at all. The row says so in place of the options, so a
// dead key is explained rather than merely dead.
func (s *TaskPane) handleOnCompleteKey(msg tea.KeyMsg) {
	if !s.onCompleteApplies() || len(s.editOnCompleteOptions) == 0 {
		return
	}
	switch msg.String() {
	case "up", "k", "left", "h":
		if s.editOnCompleteIdx > 0 {
			s.editOnCompleteIdx--
		}
	case "down", "j", "right", "l":
		if s.editOnCompleteIdx < len(s.editOnCompleteOptions)-1 {
			s.editOnCompleteIdx++
		}
	}
}

func (s *TaskPane) updateEditFocus() {
	s.editName.Blur()
	s.editPrompt.Blur()
	s.editWatch.Blur()
	s.editTarget.Blur()
	s.editMaxRuns.Blur()
	s.editHost.Blur()
	s.editPath.Blur()
	// The schedule picker is active only while it is the focused trigger value
	// on a cron task; blur it otherwise so its raw-cron cursor stays hidden.
	s.schedule.setFocused(s.focusIndex == taskFocusTriggerValue && !s.editTriggerIsWatch)

	switch s.focusIndex {
	case taskFocusName:
		s.editName.Focus()
	case taskFocusTriggerValue:
		if s.editTriggerIsWatch {
			s.editWatch.Focus()
		}
	case taskFocusPrompt:
		s.editPrompt.Focus()
	case taskFocusTarget:
		s.editTarget.Focus()
	case taskFocusMaxRuns:
		s.editMaxRuns.Focus()
	case taskFocusHost:
		s.editHost.Focus()
	case taskFocusPath:
		s.editPath.Focus()
	}
}

func (s *TaskPane) renderEditMode() string {
	unavailable := s.unavailable != "" && s.editing && !s.creating
	t := CurrentTheme()
	editTitleStyle := DialogTitleStyle().
		MarginBottom(1)

	labelStyle := lipgloss.NewStyle().Bold(true)
	groupStyle := lipgloss.NewStyle().Bold(true).Foreground(t.Ink)
	hintStyle := DialogHintStyle()
	errorStyle := lipgloss.NewStyle().Foreground(t.Dead).Bold(true)

	buttonStyle := lipgloss.NewStyle().Foreground(t.Ink)
	focusedButtonStyle := buttonStyle.
		Background(t.Accent).
		Foreground(t.Surface)

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
	s.schedule.setWidth(s.width)
	s.editWatch.Width = inputWidth
	s.editTarget.Width = inputWidth
	s.editMaxRuns.Width = inputWidth
	s.editHost.Width = inputWidth
	s.editPath.Width = inputWidth

	// The prompt's role depends on the trigger: cron tasks require it, watch
	// tasks default each event to the raw emitted line.
	if s.editTriggerIsWatch {
		s.editPrompt.Placeholder = "Optional · {{line}} inserts the event"
	} else {
		s.editPrompt.Placeholder = "Enter task prompt…"
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
		b.WriteString(editTitleStyle.Render("New task"))
	} else if s.selectedTaskInRange() {
		tsk := s.tasks[s.selectedIdx]
		b.WriteString(editTitleStyle.Render(fmt.Sprintf("Edit task %s", tsk.ID)))
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
	// A watch task's "run now" is refused by the daemon (#1758); pressing r
	// lands its explanation here, under the trigger-type selector.
	b.WriteString(fieldErr(taskFocusTrigger))
	markEnd(taskFocusTrigger)
	markStart(taskFocusTriggerValue)
	if s.editTriggerIsWatch {
		b.WriteString(label("Watch:"))
		b.WriteString(s.editWatch.View())
	} else {
		// The picker renders its own multi-line block (type selector,
		// contextual inputs, preview, read-only cron) in place of the raw field.
		b.WriteString(s.schedule.render())
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
	markStart(taskFocusOnComplete)
	b.WriteString(label("On done:"))
	b.WriteString(s.renderOnCompleteSelector())
	b.WriteString("\n")
	markEnd(taskFocusOnComplete)
	markStart(taskFocusMaxRuns)
	b.WriteString(label("Max runs:"))
	b.WriteString(s.renderCapField())
	b.WriteString("\n")
	b.WriteString(fieldErr(taskFocusMaxRuns))
	markEnd(taskFocusMaxRuns)
	markStart(taskFocusHost)
	b.WriteString(label("Host:"))
	b.WriteString(s.editHost.View())
	b.WriteString("\n")
	b.WriteString(fieldErr(taskFocusHost))
	markEnd(taskFocusHost)
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
	if unavailable {
		b.WriteString(hintStyle.Render("Save unavailable"))
	} else if s.focusIndex == taskFocusSave {
		b.WriteString(focusedButtonStyle.Render(submitLabel))
	} else {
		b.WriteString(buttonStyle.Render(submitLabel))
	}
	b.WriteString("\n")
	markEnd(taskFocusSave)
	b.WriteString("\n")
	quitHint := configuredQuitHelp() + " quit"
	if s.creating {
		hint := "tab/shift+tab fields · enter create · esc cancel · " + quitHint
		if s.width > 0 && lipgloss.Width(hint) > s.width {
			hint = "tab fields · enter · esc cancel · " + quitHint
		}
		b.WriteString(ActionHint(fitLine(hint, s.width)))
	} else if unavailable {
		b.WriteString(s.unavailableEditNotice())
		b.WriteString("\n")
		b.WriteString(ActionHint(fitLine("tab fields · typing · esc back", s.width)))
	} else {
		hint := "tab/shift+tab fields · enter save"
		if s.width > 0 && lipgloss.Width(hint) > s.width {
			hint = "tab fields · enter save"
		}
		b.WriteString(ActionHint(fitLine(hint, s.width)))
		b.WriteString("\n")
		// The action row is contextual (#5538), the same contract the Trigger
		// selector's "←/→ switch" already follows: r/x/D fire only from
		// selector/button stops — at a text stop they are typed input (#4838) —
		// and the configured quit key is gated on !IsEditing in handleStateTasks
		// (#1961), so it is dead at every edit stop and never advertised.
		var tiers []string
		if s.textFocusStop() {
			// A text stop still names the run-now path (#1288: the overlay
			// opens straight onto Name, and the first screen must say how to
			// reach the verbs) — but only as the destination of the tab steps,
			// so nothing reads as a key that fires inside the field. The count
			// is computed, not fixed at one: several text stops sit between
			// selectors, so a bare "tab →" would promise a single press that
			// lands on another typing field.
			prefix := "tab → "
			if n := s.tabsToVerbStop(); n > 1 {
				prefix = fmt.Sprintf("tab×%d → ", n)
			}
			tiers = []string{
				prefix + "r run now · x toggle · D delete · esc list",
				prefix + "r run · x toggle · D del · esc list",
				prefix + "r/x/D · esc list",
				"esc list",
			}
			// A watch task can't be manually run (#1758): drop "r run" so the
			// editor never advertises an action that always fails.
			if s.selectedTaskIsWatch() {
				tiers = []string{
					prefix + "x toggle · D delete · esc list",
					prefix + "x toggle · D del · esc list",
					prefix + "x/D · esc list",
					"esc list",
				}
			}
		} else {
			// Three tiers, not two (#3630). The old ladder jumped straight from
			// the full row to a much shorter one, and of everything it dropped
			// it dropped esc's LABEL — the one key here whose destination
			// ("back to the list", not "out of the manager") is not guessable
			// from the glyph. The middle tier abbreviates the verbs and keeps
			// every destination.
			tiers = []string{
				"r run now · x toggle · D delete · esc list",
				"r run · x toggle · D del · esc list",
				"r run · x toggle · D del · esc",
			}
			// A watch task can't be manually run (#1758): drop "r run" so the
			// editor never advertises an action that always fails.
			if s.selectedTaskIsWatch() {
				tiers = []string{
					"x toggle · D delete · esc list",
					"x toggle · D del · esc list",
					"x toggle · D del · esc",
				}
			}
		}
		actions := tiers[len(tiers)-1]
		for _, tier := range tiers {
			if s.width <= 0 || lipgloss.Width(tier) <= s.width {
				actions = tier
				break
			}
		}
		b.WriteString(ActionHint(fitLine(actions, s.width)))
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
	footerRows := 1
	if s.unavailable != "" && s.editing && !s.creating {
		footerRows += strings.Count(s.unavailableEditNotice(), "\n") + 1
	}
	hint := strings.Join(lines[len(lines)-footerRows:], "\n")
	body := lines[:len(lines)-footerRows]
	visible := maxH - footerRows
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
	t := CurrentTheme()
	selectedStyle := lipgloss.NewStyle().Bold(true).Background(t.SurfaceRaised).Foreground(t.Ink)
	dimSelectedStyle := lipgloss.NewStyle().Foreground(t.Ink)
	normalStyle := lipgloss.NewStyle().Foreground(t.Ink)
	hintStyle := DialogHintStyle()

	option := func(name string, sel bool) string {
		switch {
		case sel && focused:
			return SelectionMarker("▸ ") + selectedStyle.Render(name)
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

// renderOnCompleteSelector renders the spawned-session lifecycle choice on one
// line, in the same shape as the Program selector: the current value, with ←/→
// stepping when focused.
//
// When the form names a target session the row renders the reason instead of the
// options. Showing "keep" there would be the worse lie of the two available:
// it reads as a policy this task carries, when in fact the task has no session
// of its own to apply one to.
func (s *TaskPane) renderOnCompleteSelector() string {
	t := CurrentTheme()
	hintStyle := DialogHintStyle()
	if !s.onCompleteApplies() {
		return hintStyle.Render(s.wrapFieldText("Target session is kept."))
	}

	focused := s.focusIndex == taskFocusOnComplete
	selectedStyle := lipgloss.NewStyle().Bold(true).Background(t.SurfaceRaised).Foreground(t.Ink)
	dimSelectedStyle := lipgloss.NewStyle().Foreground(t.Ink)

	value := task.OnCompleteKeep
	if s.editOnCompleteIdx >= 0 && s.editOnCompleteIdx < len(s.editOnCompleteOptions) {
		value = s.editOnCompleteOptions[s.editOnCompleteIdx]
	}
	if focused {
		picker := SelectionMarker("◂ ") + selectedStyle.Render(value) + SelectionMarker(" ▸")
		hint := "←/→ " + onCompleteHint(value)
		if s.width > 0 && 9+lipgloss.Width(picker)+3+lipgloss.Width(hint) > s.width {
			return picker + "\n         " + hintStyle.Render(s.wrapFieldText(hint))
		}
		return picker + hintStyle.Render("   "+hint)
	}
	return dimSelectedStyle.Render(value)
}

// renderCapField renders the watch-task concurrency-cap input, or the reason
// the current shape cannot carry one — the same inapplicable-row pattern as
// renderOnCompleteSelector: showing an editable field there would offer a cap
// ValidateTrigger refuses, so the row says why instead. The reason text is
// task.CapUnavailableReason — the same words the daemon's validator gives and
// the web modal shows (#4180).
func (s *TaskPane) renderCapField() string {
	if reason := task.CapUnavailableReason(s.editTriggerIsWatch, s.editTarget.Value()); reason != "" {
		return DialogHintStyle().Render(s.wrapFieldText(reason))
	}
	return s.editMaxRuns.View()
}

// Keep consequences readable in the actual modal content width, which is
// narrower than the terminal. Continuations align with the value after the
// nine-cell label; the form's focus range includes these lines so scrolling
// keeps the whole explanation visible when this field is selected.
func (s *TaskPane) wrapFieldText(text string) string {
	if s.width <= 0 {
		return text
	}
	width := max(1, s.width-9)
	return strings.ReplaceAll(xansi.Wrap(text, width, ""), "\n", "\n         ")
}

// onCompleteHint says what the SELECTED verb does to the session a run created,
// beside the picker that sets it. The words are the consequence, not the verb
// again: "kill" beside a bare "change" reads as reversible, and it is the one
// option here whose effect cannot be undone — it deletes the session's worktree
// and prunes the branch it owned (#2595). "permanently" carries that, rather
// than capitals, which this repo reserves for env vars and literal flag names.
func onCompleteHint(value string) string {
	return task.OnCompleteHint(value)
}

// renderProgramSelector renders the agent selector as a single line showing
// the current choice; ←/→ steps through the options when focused.
func (s *TaskPane) renderProgramSelector() string {
	focused := s.focusIndex == taskFocusProgram
	t := CurrentTheme()
	selectedStyle := lipgloss.NewStyle().Bold(true).Background(t.SurfaceRaised).Foreground(t.Ink)
	dimSelectedStyle := lipgloss.NewStyle().Foreground(t.Ink)
	hintStyle := DialogHintStyle()

	value := programDefaultLabel
	if s.editProgramIdx >= 0 && s.editProgramIdx < len(s.editProgramOptions) {
		value = s.editProgramOptions[s.editProgramIdx]
	}
	if focused {
		return SelectionMarker("◂ ") + selectedStyle.Render(value) + SelectionMarker(" ▸") + hintStyle.Render("   ←/→ change agent")
	}
	return dimSelectedStyle.Render(value)
}
