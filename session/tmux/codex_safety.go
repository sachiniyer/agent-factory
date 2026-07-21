package tmux

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

const codexSafetyModelVerificationPolls = 30

const (
	codexSafetyRetryLabel       = "Retry with a faster model"
	codexSafetyWaitLabel        = "Keep waiting"
	codexSafetyDismissWaitLabel = "Dismiss and keep waiting"
	codexSafetyLearnLabel       = "Learn more"
)

// codexSafetyBufferingState spans daemon Snapshot polls. Codex replaces its
// normal composer with the picker, so its default model status line is no
// longer visible by the time af has to decide. lastModel is therefore learned
// from normal panes before the picker appears and compared with the first
// normal footer after it closes.
type codexSafetyBufferingState struct {
	lastModel          string
	expectedModel      string
	verificationPolls  int
	awaitingModelCheck bool
	notified           bool
}

type codexSafetyDialog struct {
	labels        []string
	selectedIndex int
	targetIndex   int
	targetLabel   string
}

type codexPickerOption struct {
	label    string
	selected bool
}

// handleCodexSafetyBuffering detects and dismisses Codex's model-routing
// picker without making the faster-model action reachable by accident.
//
// This is called by CheckAndHandleTrustPrompt on the daemon's continuous poll,
// against arbitrary visible output. Detection is consequently anchored on the
// complete dialog chrome, navigation targets the literal label (never its
// displayed ordinal), and Enter is sent only after a second capture proves the
// intended row is selected. A later poll compares Codex's default status-line
// model with the pre-dialog value and surfaces the result in af's log.
func (t *TmuxSession) handleCodexSafetyBuffering(content string) bool {
	targetLabel, safetyPromptPresent, safetyPromptActive := t.inspectCodexSafetyPrompt(content)
	dialog, dialogPresent := parseCodexSafetyDialog(content, targetLabel, safetyPromptActive)
	model := codexStatusLineModel(content)
	state := &t.codexSafety

	if state.awaitingModelCheck {
		t.verifyCodexSafetyModel(model, dialogPresent)
		// Startup treats false as permission to deliver the queued prompt. The
		// accepted picker can take another frame to close, so keep blocking while
		// its live modal shell remains visible.
		return safetyPromptPresent
	}

	if !dialogPresent {
		if safetyPromptPresent {
			if !state.notified {
				log.WarningLog.Printf(
					"Codex session %q is stalled on additional safety checks, but af could not safely select %q from the rendered options",
					t.sanitizedName, targetLabel,
				)
				state.notified = true
			}
			return true
		}
		if model != "" {
			state.lastModel = model
		}
		state.notified = false
		return false
	}

	if model != "" {
		// Some terminal layouts can leave the status line visible below a
		// picker. Prefer that freshest baseline when it is available.
		state.lastModel = model
	}
	if !state.notified {
		log.WarningLog.Printf(
			"Codex session %q hit additional safety checks; selecting %q and preserving the current model",
			t.sanitizedName, dialog.targetLabel,
		)
		state.notified = true
	}

	keys := navigationKeys(dialog.selectedIndex, dialog.targetIndex)
	if len(keys) > 0 {
		if err := t.tapPromptKeys(keys...); err != nil {
			log.ErrorLog.Printf("could not navigate Codex additional safety checks for session %q: %v", t.sanitizedName, err)
			return true
		}

		// Selection is a terminal UI state, not a numbered form value. Read
		// the pane again and require the literal wait label to own the cursor
		// before Enter can possibly reach the picker (#2181).
		selectedContent, err := t.CapturePaneContent()
		if err != nil {
			log.ErrorLog.Printf("could not verify Codex additional safety-check selection for session %q: %v", t.sanitizedName, err)
			return true
		}
		selectedTarget, selectedPromptPresent, selectedPromptActive := t.inspectCodexSafetyPrompt(selectedContent)
		selectedDialog, stillPresent := parseCodexSafetyDialog(selectedContent, selectedTarget, selectedPromptActive)
		if !stillPresent {
			if selectedPromptPresent {
				log.ErrorLog.Printf(
					"refusing to accept Codex additional safety checks for session %q: could not prove the selected row while the picker remained visible",
					t.sanitizedName,
				)
				return true
			}
			if current := codexStatusLineModel(selectedContent); current != "" {
				state.lastModel = current
			}
			state.notified = false
			log.InfoLog.Printf("Codex session %q additional safety checks resolved before af accepted a choice", t.sanitizedName)
			return true
		}
		dialog = selectedDialog
	}

	if dialog.selectedIndex != dialog.targetIndex ||
		dialog.selectedIndex < 0 || dialog.labels[dialog.selectedIndex] != dialog.targetLabel {
		log.ErrorLog.Printf(
			"refusing to accept Codex additional safety checks for session %q: selected row is not %q",
			t.sanitizedName, dialog.targetLabel,
		)
		return true
	}

	if err := t.tapPromptKeys("Enter"); err != nil {
		log.ErrorLog.Printf("could not accept Codex additional safety checks for session %q: %v", t.sanitizedName, err)
		return true
	}
	state.expectedModel = state.lastModel
	state.verificationPolls = 0
	state.awaitingModelCheck = true
	return true
}

func (t *TmuxSession) verifyCodexSafetyModel(model string, dialogPresent bool) {
	state := &t.codexSafety
	if dialogPresent || model == "" {
		state.verificationPolls++
		if state.verificationPolls < codexSafetyModelVerificationPolls {
			return
		}
		log.WarningLog.Printf(
			"Codex session %q additional safety checks were answered, but af could not read a post-dialog model footer after %d polls",
			t.sanitizedName, state.verificationPolls,
		)
		state.finishModelVerification(model)
		return
	}

	switch {
	case state.expectedModel == "":
		log.WarningLog.Printf(
			"Codex session %q additional safety checks were answered, but af had no pre-dialog model footer to compare with current model %q",
			t.sanitizedName, model,
		)
	case model != state.expectedModel:
		log.ErrorLog.Printf(
			"Codex session %q model changed after additional safety checks: before %q, after %q; possible unintended downgrade",
			t.sanitizedName, state.expectedModel, model,
		)
	default:
		log.InfoLog.Printf(
			"Codex session %q additional safety checks: verified model unchanged: %s",
			t.sanitizedName, model,
		)
	}
	state.finishModelVerification(model)
}

func (s *codexSafetyBufferingState) finishModelVerification(model string) {
	if model == "" {
		model = s.lastModel
	}
	*s = codexSafetyBufferingState{lastModel: model}
}

// parseCodexSafetyDialog admits the observed Codex 0.144.6 picker and the
// current upstream rewording. Both forms require their full prose/chrome plus
// all three exact actions. The selected and target rows are found by label;
// numeric prefixes are parsed only as inert picker syntax.
func parseCodexSafetyDialog(content, targetLabel string, promptActive bool) (codexSafetyDialog, bool) {
	clean := normalizeCodexPane(content)
	if !promptActive {
		return codexSafetyDialog{}, false
	}

	// The visible transcript above the modal is arbitrary agent output and can
	// contain numbered lists. Only the final contiguous numbered block can be
	// the picker: the recognized modal footer permits no numbered output after
	// its option rows.
	var (
		pickerOptions  []codexPickerOption
		currentOptions []codexPickerOption
	)
	for _, line := range strings.Split(clean, "\n") {
		label, selected, ok := codexPickerRow(line)
		if ok {
			currentOptions = append(currentOptions, codexPickerOption{label: label, selected: selected})
			continue
		}
		if len(currentOptions) > 0 {
			pickerOptions = append(pickerOptions[:0], currentOptions...)
			currentOptions = currentOptions[:0]
		}
	}
	if len(currentOptions) > 0 {
		pickerOptions = append(pickerOptions[:0], currentOptions...)
	}

	dialog := codexSafetyDialog{selectedIndex: -1, targetIndex: -1, targetLabel: targetLabel}
	selectedCount := 0
	for _, option := range pickerOptions {
		dialog.labels = append(dialog.labels, option.label)
		idx := len(dialog.labels) - 1
		if option.selected {
			dialog.selectedIndex = idx
			selectedCount++
		}
		if option.label == targetLabel {
			dialog.targetIndex = idx
		}
	}

	if selectedCount != 1 || dialog.targetIndex < 0 || len(dialog.labels) != 3 {
		return codexSafetyDialog{}, false
	}
	seen := make(map[string]bool, len(dialog.labels))
	for _, label := range dialog.labels {
		seen[label] = true
	}
	if !seen[codexSafetyRetryLabel] || !seen[targetLabel] || !seen[codexSafetyLearnLabel] {
		return codexSafetyDialog{}, false
	}
	return dialog, true
}

// inspectCodexSafetyPrompt separates transcript text that quotes the picker
// from Codex's active bottom-pane ListSelectionView. Upstream's selection view
// implements no cursor position, so Codex hides the terminal cursor while it is
// active; the ordinary composer exposes one. The exact dialog shell remains a
// fail-closed startup blocker if the cursor query fails, but it is never
// actionable without the positive hidden-cursor signal.
func (t *TmuxSession) inspectCodexSafetyPrompt(content string) (targetLabel string, promptPresent, promptActive bool) {
	targetLabel, candidate := codexSafetyPromptTarget(content)
	if !candidate {
		return "", false, false
	}
	cursor, err := t.readPaneCursorState()
	if err != nil {
		return targetLabel, true, false
	}
	if cursor.Visible {
		return "", false, false
	}
	return targetLabel, true, true
}

// codexSafetyPromptTarget recognizes the whole modal shell independently from
// its option rows. That lets af surface a changed/unhandled picker without
// injecting any key, while parseCodexSafetyDialog remains the stricter action
// gate. The unique footer must end the visible pane, apart from the one normal
// Codex model status line that some terminal layouts retain below the picker.
// Any other trailing output rejects the capture.
func codexSafetyPromptTarget(content string) (string, bool) {
	clean := normalizeCodexPane(content)
	flat := strings.Join(strings.Fields(clean), " ")
	const oldMessage = "Additional safety checks This request requires additional safety checks, which can take extra time. Hang tight or retry with a faster model for a quicker response, though it may be less capable of handling complex requests."
	const oldFooter = "Press enter to confirm or esc to go back"
	if strings.Contains(flat, oldMessage) && codexSafetyFooterEndsPane(clean, flat, oldFooter) {
		return codexSafetyWaitLabel, true
	}

	const newMessage = "Our systems are thinking a bit more about this request before responding. Hang tight or retry with a faster model for a quicker response, though it may be less capable of handling complex requests."
	const newFooter = "No action is required. Codex will keep waiting, and this menu will close when the response is ready."
	if strings.Contains(flat, newMessage) && codexSafetyFooterEndsPane(clean, flat, newFooter) {
		return codexSafetyDismissWaitLabel, true
	}
	return "", false
}

func codexSafetyFooterEndsPane(clean, flat, footer string) bool {
	if strings.HasSuffix(flat, footer) {
		return true
	}

	lines := strings.Split(clean, "\n")
	for idx := len(lines) - 1; idx >= 0; idx-- {
		statusLine := strings.TrimSpace(lines[idx])
		if statusLine == "" {
			continue
		}
		if codexStatusLineModel(statusLine) == "" {
			return false
		}
		status := strings.Join(strings.Fields(statusLine), " ")
		return strings.HasSuffix(flat, footer+" "+status)
	}
	return false
}

func codexPickerRow(line string) (label string, selected bool, ok bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "›") {
		selected = true
		line = strings.TrimSpace(strings.TrimPrefix(line, "›"))
	}
	dot := strings.Index(line, ". ")
	if dot <= 0 {
		return "", false, false
	}
	for _, r := range line[:dot] {
		if r < '0' || r > '9' {
			return "", false, false
		}
	}
	label = strings.TrimSpace(line[dot+2:])
	return label, selected, label != ""
}

func navigationKeys(selected, target int) []string {
	if selected < 0 || target < 0 || selected == target {
		return nil
	}
	key, count := "Down", target-selected
	if count < 0 {
		key, count = "Up", -count
	}
	keys := make([]string, count)
	for idx := range keys {
		keys[idx] = key
	}
	return keys
}

// codexStatusLineModel reads Codex's default `model-with-reasoning ·
// current-dir` status line. Requiring the path-shaped second segment and
// searching only the bottom few visible rows avoids learning a model from
// ordinary transcript prose that happens to contain a middle dot.
func codexStatusLineModel(content string) string {
	lines := strings.Split(normalizeCodexPane(content), "\n")
	seen := 0
	for idx := len(lines) - 1; idx >= 0 && seen < 6; idx-- {
		line := strings.TrimSpace(lines[idx])
		if line == "" {
			continue
		}
		seen++
		parts := strings.Split(line, " · ")
		if len(parts) < 2 {
			continue
		}
		directory := strings.TrimSpace(parts[1])
		if directory != "~" && !strings.HasPrefix(directory, "~/") && !strings.HasPrefix(directory, "/") {
			continue
		}
		model := strings.TrimSpace(parts[0])
		if model != "" && !strings.ContainsAny(model, "›\r\n") {
			return model
		}
	}
	return ""
}

func normalizeCodexPane(content string) string {
	return ansiCSISequence.ReplaceAllString(strings.ReplaceAll(content, "\r\n", "\n"), "")
}

func (t *TmuxSession) tapPromptKeys(keys ...string) error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	args := []string{"send-keys", "-t", exactTarget(t.sanitizedName)}
	args = append(args, keys...)
	if err := t.runTmuxBounded(ctx, args...); err != nil {
		joined := strings.Join(keys, " ")
		if ctx.Err() != nil {
			return fmt.Errorf("%w: send-keys %s after %s", ErrTmuxTimeout, joined, tmuxCommandTimeout)
		}
		if !t.ExistsOrUnknown() {
			return fmt.Errorf("%w: send-keys %s", ErrSessionGone, joined)
		}
		return fmt.Errorf("error sending %s keystroke(s): %w", joined, err)
	}
	return nil
}
