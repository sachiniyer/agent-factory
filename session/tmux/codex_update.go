package tmux

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/log"
)

const codexUpdateSkipLabel = "Skip"

var codexUpdateOSCSequence = regexp.MustCompile("\\x1b\\][^\\x07\\x1b]*(?:\\x07|\\x1b\\\\)")

// codexUpdatePromptState spans pane polls while Codex repaints its startup
// picker. Movement and confirmation are separate states: a stale frame after a
// Down key must not trigger a second Down, and a stale frame after Enter must not
// trigger a second confirmation.
type codexUpdatePromptState struct {
	selectionPending bool
	skipConfirmed    bool
}

type codexUpdateDialog struct {
	selectedLabel string
}

// handleCodexUpdatePrompt dismisses Codex's startup update picker without ever
// making its default "Update now" action reachable. Picker ordinals are not
// form values, so af navigates by arrow key, captures the pane again, and sends
// Enter only after the literal "Skip" row is visibly selected (#4712).
func (t *TmuxSession) handleCodexUpdatePrompt(content string) bool {
	dialog, promptPresent, promptActionable := t.inspectCodexUpdatePrompt(content)
	state := &t.codexUpdate

	if state.skipConfirmed {
		if promptPresent || !t.codexUpdatePickerProvenClosed(content) {
			return true
		}
		*state = codexUpdatePromptState{}
		return false
	}

	if state.selectionPending {
		switch {
		case promptActionable && codexUpdateSkipSelected(dialog):
			state.selectionPending = false
			return t.confirmCodexUpdateSkip(dialog)
		case promptPresent || !t.codexUpdatePickerProvenClosed(content):
			return true
		default:
			*state = codexUpdatePromptState{}
			return false
		}
	}

	if !promptActionable {
		return promptPresent
	}
	if codexUpdateSkipSelected(dialog) {
		return t.confirmCodexUpdateSkip(dialog)
	}

	var keys []string
	switch dialog.selectedLabel {
	case "Update now":
		keys = []string{"Down"}
	case "Skip until next version":
		keys = []string{"Up"}
	}
	if len(keys) == 0 {
		// The complete picker is present, but its selected row or safe target
		// could not be established. Holding is safer than guessing at input.
		return true
	}
	if err := t.tapPromptKeys(keys...); err != nil {
		log.ErrorLog.Printf("could not navigate Codex update picker for session %q: %v", t.sanitizedName, err)
		return true
	}
	t.noteDialogKeystroke(codexUpdateDialogName, codexUpdateSkipLabel, keys...)
	state.selectionPending = true

	// Terminal painting is asynchronous. A second capture is the earliest point
	// at which Enter can be authorized; if it is stale, the next poll verifies
	// again without repeating the movement key.
	selectedContent, err := t.CapturePaneContent()
	if err != nil {
		log.ErrorLog.Printf("could not verify Codex update-picker selection for session %q: %v", t.sanitizedName, err)
		return true
	}
	selected, selectedPresent, selectedActionable := t.inspectCodexUpdatePrompt(selectedContent)
	if selectedActionable && codexUpdateSkipSelected(selected) {
		state.selectionPending = false
		return t.confirmCodexUpdateSkip(selected)
	}
	if selectedPresent || !t.codexUpdatePickerProvenClosed(selectedContent) {
		return true
	}
	*state = codexUpdatePromptState{}
	return false
}

func (t *TmuxSession) confirmCodexUpdateSkip(dialog codexUpdateDialog) bool {
	if !codexUpdateSkipSelected(dialog) {
		return true
	}
	if err := t.tapPromptKeys("Enter"); err != nil {
		log.ErrorLog.Printf("could not dismiss Codex update picker for session %q: %v", t.sanitizedName, err)
		return true
	}
	t.noteDialogKeystroke(codexUpdateDialogName, codexUpdateSkipLabel, "Enter")
	t.codexUpdate.skipConfirmed = true
	return true
}

// inspectCodexUpdatePrompt requires both the provider-owned picker structure and
// Codex's hidden-cursor modal state. The structure alone may appear verbatim in
// agent output; the hidden cursor alone belongs to every ListSelectionView.
func (t *TmuxSession) inspectCodexUpdatePrompt(content string) (codexUpdateDialog, bool, bool) {
	dialog, candidate := codexUpdatePromptCandidate(content)
	if !candidate {
		return codexUpdateDialog{}, false, false
	}
	cursor, err := t.readPaneCursorState()
	if err != nil {
		return dialog, true, false
	}
	if cursor.Visible {
		return codexUpdateDialog{}, false, false
	}
	return dialog, true, dialog.selectedLabel != "" && codexUpdateFooterEndsPane(content)
}

// codexUpdatePromptCandidate recognizes both the original "Update available!"
// screen and its current "Update available · old → new" rendering. The selected
// literal remains visible when a short pane clips the other two rows; pairing it
// with the update header and release-notes URL identifies that compact form.
func codexUpdatePromptCandidate(content string) (codexUpdateDialog, bool) {
	clean := normalizeCodexUpdatePane(content)
	compact := compactCodexUpdatePane(clean)
	if !(strings.Contains(compact, "Updateavailable!") || strings.Contains(compact, "Updateavailable·")) ||
		!strings.Contains(compact, "Releasenotes:https://github.com/openai/codex/releases/latest") {
		return codexUpdateDialog{}, false
	}

	// Short panes scroll the picker so only its highlighted row is visible, and
	// may wrap that row. The selected literal is enough: the update screen owns a
	// fixed three-row order, af moves one row toward Skip, then separately proves
	// Skip itself owns the cursor before it can confirm.
	var selectedLabels []string
	for _, line := range strings.Split(clean, "\n") {
		label, selected, ok := codexPickerRow(line)
		if !ok || !selected {
			continue
		}
		switch {
		case strings.HasPrefix(label, "Update now"):
			selectedLabels = append(selectedLabels, "Update now")
		case label == codexUpdateSkipLabel:
			selectedLabels = append(selectedLabels, codexUpdateSkipLabel)
		case strings.HasPrefix(label, "Skip until next version"):
			selectedLabels = append(selectedLabels, "Skip until next version")
		}
	}
	if len(selectedLabels) != 1 {
		// Header plus the adjacent release-notes needle is picker-only evidence,
		// but a partial repaint has no safe navigation target yet. Report the
		// prompt as present so delivery remains blocked; inspectCodexUpdatePrompt
		// makes it actionable only after exactly one selected row is visible.
		return codexUpdateDialog{}, true
	}
	return codexUpdateDialog{selectedLabel: selectedLabels[0]}, true
}

func codexUpdateFooterEndsPane(content string) bool {
	compact := compactCodexUpdatePane(content)
	return strings.HasSuffix(compact, "Pressentertocontinue") ||
		strings.HasSuffix(compact, "entercontinue·escskip")
}

func codexUpdateSkipSelected(dialog codexUpdateDialog) bool {
	return dialog.selectedLabel == codexUpdateSkipLabel
}

// codexUpdatePickerProvenClosed accepts either the ordinary visible-cursor
// composer or the next positively recognized launch modal. Directory trust also
// hides the cursor, so cursor visibility alone would leave the update state
// blocking that modal forever instead of handing it to its guarded handler.
func (t *TmuxSession) codexUpdatePickerProvenClosed(content string) bool {
	return CodexTrustPromptPresent(content) || t.codexPickerProvenClosed()
}

func compactCodexUpdatePane(content string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, normalizeCodexUpdatePane(content))
}

func normalizeCodexUpdatePane(content string) string {
	return codexUpdateOSCSequence.ReplaceAllString(normalizeCodexPane(content), "")
}

func (t *TmuxSession) resetCodexUpdateState() {
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
	t.codexUpdate = codexUpdatePromptState{}
}
