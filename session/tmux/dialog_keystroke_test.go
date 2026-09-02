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
