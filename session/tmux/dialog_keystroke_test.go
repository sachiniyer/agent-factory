package tmux

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// Part 2 of #3579. The reported failure was not only that af pressed the wrong
// key — it was that the user could not tell. The create died with
//
//	failed to start instance: agent did not become ready: session died while
//	waiting for agent to start: tmux session no longer exists:
//	capture-pane: exit status 1
//
// which names the tmux command af used to NOTICE the death and nothing about
// the dialog af had answered a second earlier. These tests pin the distinct
// outcome: a pane that dies while af is answering a modal reports the dialog
// and the keys af sent.

// dyingDialogPane drives CheckAndHandleTrustPrompt against a live folder-trust
// picker whose agent quits the moment the dialog is answered — the production
// behavior of "No, exit", reproduced here for whichever option af confirms so
// the diagnostic is exercised independently of the selection fix.
func dyingDialogPane(t *testing.T) (*TmuxSession, *claudeFolderTrustPane) {
	t.Helper()
	pane := &claudeFolderTrustPane{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}}
	dead := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(joined, "has-session"):
				if dead {
					return errors.New("can't find session")
				}
				return nil
			case strings.Contains(joined, "send-keys"):
				for _, name := range injectedKeyNames([]string{joined}) {
					pane.key(name)
				}
				// The pane process exits on the answer, so the NEXT read is the
				// one that discovers the session is gone — exactly the ordering
				// the create path hit.
				if len(pane.committedLabels()) > 0 {
					dead = true
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "display-message") {
				return []byte("0 0 0"), nil
			}
			if dead {
				return nil, errors.New("exit status 1")
			}
			return []byte(pane.capture()), nil
		},
	}
	return newTmuxSession(toTmuxName("dying", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec), pane
}

// answerDyingDialog drives the pane until af has answered the dialog, letting
// time pass between checks so af's settle delay is crossed the way the daemon's
// poll interval crosses it.
func answerDyingDialog(t *testing.T, session *TmuxSession, pane *claudeFolderTrustPane) {
	t.Helper()
	advance := setClaudeTrustClock(t)
	for i := 0; i < 6 && len(pane.committedLabels()) == 0; i++ {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt())
	}
}

func TestCapturePaneContent_DeathWhileAnsweringADialogNamesTheDialogAndTheKeys(t *testing.T) {
	session, pane := dyingDialogPane(t)
	answerDyingDialog(t, session, pane)
	require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels(), "precondition: af answered the dialog")

	_, err := session.CapturePaneContent()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSessionGone,
		"callers tear sessions down on this sentinel; the added wording must not break errors.Is")

	message := err.Error()
	require.Contains(t, message, claudeFolderTrustDialogName, "the error must name the dialog af answered")
	require.Contains(t, message, "Down Enter", "the error must name the keys af sent")
	require.Contains(t, message, claudeTrustYesLabel, "the error must name the option af aimed at")
	require.Contains(t, message, "capture-pane: exit status 1",
		"the underlying cause stays in the message; it is simply no longer the whole of it")
}

// The create failure the user sees is this error wrapped twice, by
// task.WaitForReady ("session died while waiting for agent to start") and by
// task.WaitForReadyAndSendPrompt (ErrAgentReadiness). Both are plain %w wraps,
// so what is asserted here is what reaches the terminal.
func TestCapturePaneContent_DeathDiagnosticSurvivesTheCreatePathWrapping(t *testing.T) {
	session, pane := dyingDialogPane(t)
	answerDyingDialog(t, session, pane)
	_, err := session.CapturePaneContent()
	require.Error(t, err)

	wrapped := errors.New("agent did not become ready")
	surfaced := errors.Join(wrapped, errors.New("session died while waiting for agent to start: "+err.Error()))
	require.Contains(t, surfaced.Error(), claudeFolderTrustDialogName)
	require.NotContains(t, strings.TrimSpace(surfaced.Error()), "start: tmux session no longer exists",
		"the dialog clause must come BEFORE the sentinel, so the first thing after the cause is the dialog")
}

// A death long after af last touched a dialog is not evidence about that
// dialog, and the error goes back to its plain form. The window is what keeps
// the diagnostic from attaching itself to every later failure of a session
// whose trust prompt was answered successfully at startup.
func TestSessionGoneError_StaleDialogKeystrokeIsNotAttributed(t *testing.T) {
	session := newTmuxSession(toTmuxName("stale", ""), ProgramClaude, NewMockPtyFactory(t), cmd_test.MockCmdExec{})

	base := time.Now()
	restore := setDialogKeystrokeClock(t, func() time.Time { return base })
	session.noteDialogKeystroke(claudeFolderTrustDialogName, claudeTrustAffirmativeLabel, "Down", "Enter")
	restore(func() time.Time { return base.Add(dialogDeathAttributionWindow + time.Second) })

	err := session.sessionGoneError("capture-pane", errors.New("exit status 1"))
	require.ErrorIs(t, err, ErrSessionGone)
	require.Equal(t, "tmux session no longer exists: capture-pane: exit status 1", err.Error(),
		"outside the window the message must be exactly what it always was")
}

// A dialog af never answered leaves the message untouched, which is what keeps
// every existing caller and log-reader working.
func TestSessionGoneError_NoDialogKeystrokeKeepsTheOriginalMessage(t *testing.T) {
	session := newTmuxSession(toTmuxName("plain", ""), ProgramClaude, NewMockPtyFactory(t), cmd_test.MockCmdExec{})
	err := session.sessionGoneError("capture-pane", errors.New("exit status 1"))
	require.Equal(t, "tmux session no longer exists: capture-pane: exit status 1", err.Error())
}

// A fresh pane process cannot have been killed by a key af sent to the previous
// one, so the record is dropped at that boundary.
func TestResetDialogKeystroke_DropsTheRecord(t *testing.T) {
	session := newTmuxSession(toTmuxName("reset", ""), ProgramClaude, NewMockPtyFactory(t), cmd_test.MockCmdExec{})
	session.noteDialogKeystroke(claudeFolderTrustDialogName, claudeTrustAffirmativeLabel, "Enter")
	session.resetDialogKeystroke()
	_, _, ok := session.recentDialogKeystroke()
	require.False(t, ok, "a proven runtime boundary must clear the previous pane's dialog record")
}

// setDialogKeystrokeClock installs a clock for the package-level indirection and
// returns a function that replaces it again, restoring the real clock on
// cleanup.
func setDialogKeystrokeClock(t *testing.T, now func() time.Time) func(func() time.Time) {
	t.Helper()
	previous := dialogKeystrokeNow
	t.Cleanup(func() { dialogKeystrokeNow = previous })
	dialogKeystrokeNow = now
	return func(next func() time.Time) { dialogKeystrokeNow = next }
}

// #3587 review, P2. Navigating a dialog is not answering one. If the pane dies
// after a movement key but before the confirming Enter — for instance while
// af is capturing to see where the cursor landed — the diagnostic must not
// claim af "answered" the dialog, and must not claim it chose an option. That
// would put a decision af never made into the one message an operator has to
// reconstruct the failure from.
func TestSessionGoneError_NavigationWithoutConfirmationIsNotAnAnsweredDialog(t *testing.T) {
	session := newTmuxSession(toTmuxName("nav", ""), ProgramClaude, NewMockPtyFactory(t), cmd_test.MockCmdExec{})
	session.noteDialogKeystroke(claudeFolderTrustDialogName, claudeTrustAffirmativeLabel, "Down")

	message := session.sessionGoneError("capture-pane", errors.New("exit status 1")).Error()
	require.Contains(t, message, "still navigating")
	require.Contains(t, message, "confirmed nothing")
	require.Contains(t, message, "Down")
	require.Contains(t, message, claudeTrustAffirmativeLabel)
	require.NotContains(t, message, "answered its",
		"af had not answered the dialog; it had only moved the cursor")
	require.NotContains(t, message, "to choose",
		"af chose nothing without the confirming Enter")

	// The confirming key changes the claim, and only then.
	session.noteDialogKeystroke(claudeFolderTrustDialogName, claudeTrustAffirmativeLabel, "Enter")
	answered := session.sessionGoneError("capture-pane", errors.New("exit status 1")).Error()
	require.Contains(t, answered, "answered its")
	require.Contains(t, answered, "Down Enter")
	require.Contains(t, answered, "to choose")
}

// Regression for the codex_safety.go half of #3579's retrofit (commit ac8bcc78).
// af sends Down/Up into the Codex safety-check picker to reach "Keep waiting"
// and only then sends Enter. The retrofit recorded only the Enter, leaving the
// pre-existing navigation block unrecorded — so a death during navigation read
// as an anonymous startup death (fresh dialogInput) or as a death answered by
// a stale prior dialog inside the 30s window (notably Codex update), and a
// death after Enter under-reported the keys af actually sent. The fix records
// the navigation keys immediately after the send, mirroring claude_trust.go
// and codex_update.go. These tests pin the diagnostic over the same-dialog
// accumulation the contract already supports.

// TestSessionGoneError_CodexSafetyNavigationOnlyIsNotAnAnsweredDialog pins the
// navigation-only bail-out: a death between af's Down on the safety picker and
// its confirming Enter must read as navigation (not answering) and must name
// the safety dialog and the row af was moving toward.
func TestSessionGoneError_CodexSafetyNavigationOnlyIsNotAnAnsweredDialog(t *testing.T) {
	session := newTmuxSession(toTmuxName("safety-nav", ""), ProgramCodex, NewMockPtyFactory(t), cmd_test.MockCmdExec{})
	session.noteDialogKeystroke(codexSafetyDialogName, codexSafetyWaitLabel, "Down")

	err := session.sessionGoneError("capture-pane", errors.New("exit status 1"))
	require.ErrorIs(t, err, ErrSessionGone,
		"the added wording must not break the sentinel callers tear sessions down on")
	message := err.Error()
	require.Contains(t, message, "still navigating",
		"a navigation-only death must read as af still working on the dialog")
	require.Contains(t, message, "confirmed nothing",
		"without the confirming Enter, af chose nothing")
	require.Contains(t, message, "Down", "the navigation key af sent must be named")
	require.Contains(t, message, codexSafetyDialogName, "the diagnostic must name the safety dialog af was navigating")
	require.Contains(t, message, codexSafetyWaitLabel, "the diagnostic must name the row af was moving toward")
	require.NotContains(t, message, "answered its",
		"af had not answered the dialog; it had only navigated the safety picker")
	require.NotContains(t, message, "to choose",
		"af chose nothing without the confirming Enter")
}

// TestSessionGoneError_CodexSafetyEnterAccumulatesNavigationKeys pins the
// happy path: the confirming Enter is appended to — not recorded in place of —
// the navigation keys, so a death after Enter names the full key sequence af
// sent, satisfying the "names all of them" contract from dialog_keystroke.go.
func TestSessionGoneError_CodexSafetyEnterAccumulatesNavigationKeys(t *testing.T) {
	session := newTmuxSession(toTmuxName("safety-enter", ""), ProgramCodex, NewMockPtyFactory(t), cmd_test.MockCmdExec{})
	// Mirror the fix: record navigation immediately after the successful send.
	session.noteDialogKeystroke(codexSafetyDialogName, codexSafetyWaitLabel, "Down")
	// Mirror codex_safety.go: the subsequent Enter accumulates on the same dialog.
	session.noteDialogKeystroke(codexSafetyDialogName, codexSafetyWaitLabel, "Enter")

	err := session.sessionGoneError("capture-pane", errors.New("exit status 1"))
	require.ErrorIs(t, err, ErrSessionGone)
	message := err.Error()
	require.Contains(t, message, "answered its",
		"the confirming Enter makes this an answered dialog")
	require.Contains(t, message, codexSafetyDialogName)
	require.Contains(t, message, "Down Enter",
		"the navigation keys must accumulate alongside the confirming Enter so the diagnostic names all keys af sent")
	require.Contains(t, message, "to choose",
		"the confirming Enter turns the navigation into a chosen option")
	require.Contains(t, message, codexSafetyWaitLabel)
}

// TestSessionGoneError_CodexSafetyNavigationOverwritesStaleCrossDialogRecord
// pins failure mode A2: a stale cross-dialog record (in practice Codex update
// dismissed shortly before the safety picker appeared, inside the 30s window)
// must be overwritten the instant af records the safety navigation, so a
// navigation-only death is attributed to the safety dialog af was actually
// working on, not misattributed to the prior Codex update dialog.
// noteDialogKeystroke resets dialogInput when the dialog name differs, which is
// the property the fix leans on to repair A2.
func TestSessionGoneError_CodexSafetyNavigationOverwritesStaleCrossDialogRecord(t *testing.T) {
	session := newTmuxSession(toTmuxName("safety-stale", ""), ProgramCodex, NewMockPtyFactory(t), cmd_test.MockCmdExec{})
	// Seed the stale record the launch-time Codex update picker leaves behind
	// when it was dismissed shortly before the safety picker appeared, inside
	// the 30s attribution window.
	session.noteDialogKeystroke(codexUpdateDialogName, codexUpdateSkipLabel, "Down", "Enter")
	// af navigates the safety picker. With the fix this is recorded, and
	// because the dialog name differs from the stale record, noteDialogKeystroke
	// resets dialogInput before appending — the prior Codex update record is gone.
	session.noteDialogKeystroke(codexSafetyDialogName, codexSafetyWaitLabel, "Down")

	err := session.sessionGoneError("capture-pane", errors.New("exit status 1"))
	require.ErrorIs(t, err, ErrSessionGone)
	message := err.Error()
	require.Contains(t, message, codexSafetyDialogName,
		"the safety navigation record must overwrite the stale prior-dialog record and name the dialog af was actually working on")
	require.NotContains(t, message, codexUpdateDialogName,
		"the stale Codex update record must not be misattributed to a safety-picker death")
	require.Contains(t, message, "still navigating",
		"a navigation-only death reads as navigation, not as answering")
	require.Contains(t, message, "Down")
}

// TestSessionGoneError_CodexSafetySecondPickerResetsCompletedPriorRecord pins the
// #4740 review follow-up: two Codex safety-check pickers share the same
// user-facing dialog name, so noteDialogKeystroke's accumulate-same-dialog rule
// would otherwise fold a second picker's navigation onto the prior picker's
// completed (Enter-confirmed) record. A death during the new picker's
// selection-verification capture would then find the prior Enter via
// confirmed() and read as af having answered the new picker, misattributing
// keys from two separate interactions. The handler drops the completed prior
// record before recording navigation for the new one.
func TestSessionGoneError_CodexSafetySecondPickerResetsCompletedPriorRecord(t *testing.T) {
	session := newTmuxSession(toTmuxName("safety-second", ""), ProgramCodex, NewMockPtyFactory(t), cmd_test.MockCmdExec{})
	// Seed the completed record the first safety picker leaves behind when af
	// answered it (Down + Enter) shortly before the second picker appears,
	// inside the 30s attribution window.
	session.noteDialogKeystroke(codexSafetyDialogName, codexSafetyWaitLabel, "Down", "Enter")
	// The handler recognizes a second picker is starting and drops the completed
	// prior record before recording the new navigation.
	session.resetCompletedCodexSafetyKeystroke()
	session.noteDialogKeystroke(codexSafetyDialogName, codexSafetyWaitLabel, "Down")

	record, _, ok := session.recentDialogKeystroke()
	require.True(t, ok, "af was navigating the second picker; there must be a recent keystroke")
	require.Equal(t, []string{"Down"}, record.keys,
		"the second picker's navigation must start a fresh record, not accumulate onto the prior completed picker's Down Enter")
	require.False(t, record.confirmed(),
		"the prior picker's Enter must not survive into the new picker's record")

	err := session.sessionGoneError("capture-pane", errors.New("exit status 1"))
	require.ErrorIs(t, err, ErrSessionGone)
	message := err.Error()
	require.Contains(t, message, "still navigating",
		"a navigation-only death on the second picker must read as navigation, not as af having answered it")
	require.NotContains(t, message, "Down Enter",
		"the prior picker's Enter must not be misattributed to a death on the new picker")
}

// TestHandleCodexSafetyBuffering_SecondPickerResetsPriorCompletedRecord is the
// end-to-end guard for the #4740 review follow-up: it drives the real
// CheckAndHandleTrustPrompt through two safety pickers the way the daemon's
// Snapshot poll does, and asserts the recorded dialog keystroke after the
// second picker carries only the second picker's keys — not the accumulated
// keys of both. Without the reset, the second picker's navigation appends to
// the first picker's completed (Down Enter) record and a later death reads the
// first picker's Enter as af answering the second.
func TestHandleCodexSafetyBuffering_SecondPickerResetsPriorCompletedRecord(t *testing.T) {
	const normalPane = `• Working

  gpt-5.6-sol max · ~/agent-factory`
	session, _ := runTrustPromptSequence(t, ProgramCodex,
		normalPane,
		codexSafetyBufferingDialog,
		codexSafetyBufferingKeepWaitingSelected,
		normalPane,
		codexSafetyBufferingDialog,
		codexSafetyBufferingKeepWaitingSelected,
		normalPane,
	)

	require.False(t, session.CheckAndHandleTrustPrompt(), "a normal Codex pane is not a modal")
	require.True(t, session.CheckAndHandleTrustPrompt(), "the first safety picker must be handled")
	require.False(t, session.CheckAndHandleTrustPrompt(),
		"the first picker's model verification observes; it does not inject another key")
	require.True(t, session.CheckAndHandleTrustPrompt(), "the second safety picker must be handled")
	require.False(t, session.CheckAndHandleTrustPrompt(),
		"the second picker's model verification observes; it does not inject another key")

	record, _, ok := session.recentDialogKeystroke()
	require.True(t, ok, "af just answered the second safety picker; there must be a recent keystroke")
	require.Equal(t, codexSafetyDialogName, record.dialog,
		"the recorded dialog must be the safety-check, not a stale prior dialog")
	require.Equal(t, codexSafetyWaitLabel, record.choice,
		"the recorded choice must be the row af navigated to and accepted on the second picker")
	require.Equal(t, []string{"Down", "Enter"}, record.keys,
		"the second picker's record must carry only its own keys, not the accumulated keys of both pickers")
}

// TestHandleCodexSafetyBuffering_RecordsNavigationKeysForDiagnostic is the
// end-to-end guard against the same regression recurring: it drives the real
// CheckAndHandleTrustPrompt through the safety picker the way the daemon's
// Snapshot poll does, and asserts the recorded dialog keystroke carries BOTH
// the navigation key and the Enter — i.e. the fix actually calls
// noteDialogKeystroke from inside the handler on the navigation block, not
// only on the confirming Enter. Without the fix the record carries only Enter.
func TestHandleCodexSafetyBuffering_RecordsNavigationKeysForDiagnostic(t *testing.T) {
	const normalPane = `• Working

  gpt-5.6-sol max · ~/agent-factory`
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		normalPane,
		codexSafetyBufferingDialog,
		codexSafetyBufferingKeepWaitingSelected,
		normalPane,
	)

	require.False(t, session.CheckAndHandleTrustPrompt(), "a normal Codex pane is not a modal")
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the safety-buffering picker must be handled")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands), "navigate to the label, then accept; proves both keys were sent to the pane")

	// The dialog-input record is what sessionGoneError would read if a later
	// tmux read discovered the pane gone. Assert it carries the full sequence.
	record, _, ok := session.recentDialogKeystroke()
	require.True(t, ok, "af just answered the safety picker; there must be a recent keystroke")
	require.Equal(t, codexSafetyDialogName, record.dialog,
		"the recorded dialog must be the safety-check, not a stale prior dialog")
	require.Equal(t, codexSafetyWaitLabel, record.choice,
		"the recorded choice must be the row af navigated to and accepted")
	require.Equal(t, []string{"Down", "Enter"}, record.keys,
		"both the navigation key and the confirming Enter must be recorded, mirroring the sibling dialogs")
}

// TestSessionGoneError_CodexSafetyAbandonedNavigationClearedWhenPickerCloses pins
// the #4740 review follow-up at the inline thread anchored on codex_safety.go
// line 245: when a safety picker closes itself after af has sent Down but before
// its confirming Enter, the recorded navigation key is unconfirmed — and because
// two safety pickers share the same dialog name, noteDialogKeystroke's
// same-dialog accumulate rule would otherwise fold a later picker's keys onto
// the abandoned one. The handler drops the abandoned record at proven closure.
func TestSessionGoneError_CodexSafetyAbandonedNavigationClearedWhenPickerCloses(t *testing.T) {
	session := newTmuxSession(toTmuxName("safety-abandon", ""), ProgramCodex, NewMockPtyFactory(t), cmd_test.MockCmdExec{})
	// af navigated the safety picker but the picker closed before af sent Enter,
	// so the record is unconfirmed.
	session.noteDialogKeystroke(codexSafetyDialogName, codexSafetyWaitLabel, "Down")
	record, _, ok := session.recentDialogKeystroke()
	require.True(t, ok, "precondition: a navigation keystroke must already be recorded")
	require.False(t, record.confirmed(),
		"precondition: the navigation record must be unconfirmed, since no Enter followed the Down")

	// The handler sees positive evidence the picker closed before af could confirm
	// and drops the abandoned navigation record at that boundary.
	session.resetAbandonedCodexSafetyKeystroke()

	_, _, ok = session.recentDialogKeystroke()
	require.False(t, ok,
		"the abandoned safety navigation record must be cleared at picker closure so a later picker starts fresh")
}

// TestHandleCodexSafetyBuffering_AbandonedNavigationClearedWhenPickerCloses is
// the end-to-end guard for the #4740 inline follow-up at codex_safety.go line 245:
// the picker-closes-before-Enter path the daemon's Snapshot poll exercises leaves
// af's navigation key (Down) recorded but unconfirmed. With the fix the handler
// clears that record at proven closure, so the diagnostic reads the closed pane
// as a plain death rather than attributing the abandoned Down to it.
func TestHandleCodexSafetyBuffering_AbandonedNavigationClearedWhenPickerCloses(t *testing.T) {
	const normalPane = "gpt-5.6-sol max · ~/agent-factory"
	session, commands := runTrustPromptFrames(t, ProgramCodex,
		trustPromptFrame{content: normalPane},
		trustPromptFrame{content: codexSafetyBufferingDialog},
		trustPromptFrame{content: normalPane, cursorVisible: true},
		trustPromptFrame{content: normalPane, cursorVisible: true},
	)

	require.False(t, session.CheckAndHandleTrustPrompt(), "a normal Codex pane is not a modal")
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the safety picker must be navigated even when it closes before Enter")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
	}, sentKeystrokes(*commands), "Enter must never reach a picker that closed itself")
	_, _, ok := session.recentDialogKeystroke()
	require.False(t, ok,
		"the abandoned navigation record must be cleared so a later safety picker starts fresh rather than accumulating onto the closed one")
	require.False(t, session.CheckAndHandleTrustPrompt(),
		"the released session is an ordinary pane again")
}

// TestHandleCodexSafetyBuffering_SecondPickerResetsAbandonedPriorRecord is the
// end-to-end guard against the same-dialog fold the #4740 inline follow-up at
// codex_safety.go line 245 names: the first picker closes itself after af's Down
// and before its Enter, then a second safety picker appears inside the 30s
// attribution window. Without the abandoned-record clear, the second picker's
// navigation appends onto the first picker's Down and a later death on the new
// picker attributes keys from both. With the clear the second picker starts a
// fresh record carrying only its own Down Enter.
func TestHandleCodexSafetyBuffering_SecondPickerResetsAbandonedPriorRecord(t *testing.T) {
	const normalPane = "gpt-5.6-sol max · ~/agent-factory"
	session, _ := runTrustPromptFrames(t, ProgramCodex,
		trustPromptFrame{content: normalPane},
		trustPromptFrame{content: codexSafetyBufferingDialog},
		trustPromptFrame{content: normalPane, cursorVisible: true},
		trustPromptFrame{content: normalPane, cursorVisible: true},
		trustPromptFrame{content: codexSafetyBufferingDialog},
		trustPromptFrame{content: codexSafetyBufferingKeepWaitingSelected},
		trustPromptFrame{content: normalPane},
	)

	require.False(t, session.CheckAndHandleTrustPrompt(), "a normal Codex pane is not a modal")
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the first safety picker must be navigated; Enter must never reach a picker that closed itself")
	require.False(t, session.CheckAndHandleTrustPrompt(),
		"the first picker's release is an ordinary pane again")

	require.True(t, session.CheckAndHandleTrustPrompt(), "the second safety picker must be handled")
	require.False(t, session.CheckAndHandleTrustPrompt(),
		"the second picker's model verification observes; it does not inject another key")

	record, _, ok := session.recentDialogKeystroke()
	require.True(t, ok, "af just answered the second safety picker; there must be a recent keystroke")
	require.Equal(t, codexSafetyDialogName, record.dialog,
		"the recorded dialog must be the safety-check, not a stale prior dialog")
	require.Equal(t, codexSafetyWaitLabel, record.choice,
		"the recorded choice must be the row af navigated to and accepted on the second picker")
	require.Equal(t, []string{"Down", "Enter"}, record.keys,
		"the second picker's record must carry only its own keys — the first picker's abandoned Down must not have accumulated onto it")
}
