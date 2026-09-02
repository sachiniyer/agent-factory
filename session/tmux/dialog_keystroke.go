package tmux

import (
	"fmt"
	"strings"
	"time"
)

// A pane that dies moments after af injected a key into a modal is a DISTINCT
// outcome, not just another startup death (#3579).
//
// It used to be indistinguishable from any other one: af answered Claude Code's
// folder-trust dialog with Enter, Claude Code had made "No, exit" the
// preselected option, the agent obediently quit, and the create the user was
// waiting on failed with
//
//	agent did not become ready: session died while waiting for agent to start:
//	tmux session no longer exists: capture-pane: exit status 1
//
// — an error naming the tool af used to notice the death and nothing about the
// dialog af had answered a second earlier. The fix for the wrong keystroke is in
// claude_trust.go; this file makes the failure legible if af's input ever
// coincides with an agent's exit again, whatever the dialog and whatever the key.

// The dialogs af answers, named as a user would recognize them. These strings
// are USER-FACING — they appear in the error a failed session start reports —
// and keeping every one of them in a single list is what stops a new dismissal
// branch from inventing its own vocabulary.
const (
	claudeFolderTrustDialogName = "Claude folder-trust"
	// claudeLaunchGateDialogName covers the two Claude gates af answers by
	// accepting whatever Claude Code preselected: the MCP-server trust prompt
	// and the legacy folder-trust wording. af cannot say which of them it
	// answered from the keystroke alone, so it does not claim to.
	claudeLaunchGateDialogName = "Claude trust/MCP"
	// Codex's directory-trust modal preselects its affirmative option and af
	// accepts that preselection, so the option is named to keep the diagnostic
	// honest about what af confirmed.
	codexDirectoryTrustDialogName  = "Codex directory-trust"
	codexDirectoryTrustAffirmative = "Yes, continue"
	docTrustDialogName             = "documentation-link trust"
)

// dialogKeystroke is the last key af injected into a modal on this pane.
type dialogKeystroke struct {
	// dialog names the modal in the words a user would recognize.
	dialog string
	// choice is the option af was aiming at, when af aimed at one by label.
	choice string
	keys   []string
	at     time.Time
}

// dialogDeathAttributionWindow bounds how long a dialog keystroke stays
// relevant to a death.
//
// It is generous because it only decides WORDING, and the wording is a
// statement of fact — "the agent exited 12s after af sent Enter" — not a claim
// of cause. The failure it exists for is observed within a poll or two: the
// create path re-waits for readiness immediately after each dismissal. Too
// short would silently lose the diagnostic on a loaded box; too long only
// attaches a true, dated observation to an unrelated death.
const dialogDeathAttributionWindow = 30 * time.Second

// dialogKeystrokeNow is indirected for tests, which must be able to age a
// keystroke past the window without sleeping.
var dialogKeystrokeNow = time.Now

// noteDialogKeystroke records that af just injected keys into a modal. Callers
// pass the keys they actually sent, so the diagnostic can never describe a
// keystroke af did not make.
func (t *TmuxSession) noteDialogKeystroke(dialog, choice string, keys ...string) {
	t.dialogInputMu.Lock()
	defer t.dialogInputMu.Unlock()
	now := dialogKeystrokeNow()
	// Answering one dialog can take several keys — af navigates to a row and
	// then confirms it — and the diagnostic is only honest if it names all of
	// them. Keys accumulate while af is working on the SAME dialog inside the
	// window, and start over for a different one.
	if t.dialogInput.dialog != dialog || t.dialogInput.at.IsZero() ||
		now.Sub(t.dialogInput.at) > dialogDeathAttributionWindow {
		t.dialogInput = dialogKeystroke{dialog: dialog}
	}
	t.dialogInput.choice = choice
	t.dialogInput.keys = append(t.dialogInput.keys, keys...)
	if extra := len(t.dialogInput.keys) - maxRecordedDialogKeys; extra > 0 {
		// A dialog that ignores af's input is retried on every poll, so the tail
		// is what describes the pane's last moments; the head would just grow.
		t.dialogInput.keys = append([]string(nil), t.dialogInput.keys[extra:]...)
	}
	t.dialogInput.at = now
}

// maxRecordedDialogKeys bounds the key list an error message may carry.
const maxRecordedDialogKeys = 8

// resetDialogKeystroke drops the record at a proven runtime boundary. A fresh
// pane process cannot have been killed by a key af sent to the previous one.
func (t *TmuxSession) resetDialogKeystroke() {
	t.dialogInputMu.Lock()
	defer t.dialogInputMu.Unlock()
	t.dialogInput = dialogKeystroke{}
}

// recentDialogKeystroke returns the keystroke and how long ago af sent it, or
// false when af has answered no dialog on this pane inside the window.
func (t *TmuxSession) recentDialogKeystroke() (dialogKeystroke, time.Duration, bool) {
	t.dialogInputMu.Lock()
	defer t.dialogInputMu.Unlock()
	if t.dialogInput.at.IsZero() {
		return dialogKeystroke{}, 0, false
	}
	elapsed := dialogKeystrokeNow().Sub(t.dialogInput.at)
	if elapsed < 0 || elapsed > dialogDeathAttributionWindow {
		return dialogKeystroke{}, 0, false
	}
	return t.dialogInput, elapsed, true
}

// sessionGoneError is the single constructor for "this tmux read failed because
// the pane is gone". It wraps ErrSessionGone exactly as the hand-written
// fmt.Errorf calls it replaced did — callers tear sessions down on that
// sentinel, and errors.Is must keep matching — and prefixes the dialog clause
// when af answered a modal moments before the pane vanished.
//
// op is the tmux operation that noticed, kept in the message because it is what
// existing log-readers and tests match on.
func (t *TmuxSession) sessionGoneError(op string, cause error) error {
	keystroke, elapsed, ok := t.recentDialogKeystroke()
	if !ok {
		return fmt.Errorf("%w: %s: %v", ErrSessionGone, op, cause)
	}
	// Navigating a dialog is not answering one. If af never sent the confirming
	// key, saying it "answered" the dialog — and worse, that it chose an option
	// — would describe a decision af did not make, in the one message an
	// operator has to reconstruct what happened.
	if !keystroke.confirmed() {
		return fmt.Errorf("the agent exited %s while af was still navigating its %s dialog, having sent %s%s and confirmed nothing; %w: %s: %v",
			describeDialogDeathDelay(elapsed), keystroke.dialog, strings.Join(keystroke.keys, " "),
			keystroke.towardClause(), ErrSessionGone, op, cause)
	}
	return fmt.Errorf("the agent exited %s after af answered its %s dialog by sending %s%s; %w: %s: %v",
		describeDialogDeathDelay(elapsed), keystroke.dialog, strings.Join(keystroke.keys, " "),
		keystroke.choiceClause(), ErrSessionGone, op, cause)
}

// confirmed reports whether af actually sent the key that commits a choice.
// Every dialog af answers is committed with Enter; the movement keys before it
// select nothing on their own.
func (k dialogKeystroke) confirmed() bool {
	for _, key := range k.keys {
		if key == "Enter" {
			return true
		}
	}
	return false
}

// describeDialogDeathDelay keeps the sub-millisecond case readable: an agent
// that quit before af could take another breath should not be reported as
// having exited "0s" after the key.
func describeDialogDeathDelay(elapsed time.Duration) string {
	if elapsed < time.Millisecond {
		return "less than 1ms"
	}
	return elapsed.Round(time.Millisecond).String()
}

func (k dialogKeystroke) choiceClause() string {
	if k.choice == "" {
		return ""
	}
	return fmt.Sprintf(" to choose %q", k.choice)
}

// towardClause states the option af was moving toward without claiming it got
// there, let alone chose it.
func (k dialogKeystroke) towardClause() string {
	if k.choice == "" {
		return ""
	}
	return fmt.Sprintf(" toward %q", k.choice)
}
