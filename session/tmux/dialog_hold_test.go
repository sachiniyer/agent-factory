package tmux

import (
	"crypto/sha256"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// The frames #7635's tests hold or clear on. Each verdict was read off the
// gate before the test was written: the first two hold as folder unreadable
// (the reworded question wrapped, at two widths), the third reads clear, and
// the gate answers the fourth with Enter on every poll.
const (
	heldUnreadableDialog = "  Quick safety check: Is this a project you\n  created or one you trust? (Like your own\n  code.\n\n" +
		"❯ No, exit\n  Yes, I trust this folder\n\nEnter to confirm · Esc to cancel\n"
	heldUnreadableDialogRewrapped = "  Quick safety check: Is this a project\n  you created or one you trust? (Like your\n  own code.\n\n" +
		"❯ No, exit\n  Yes, I trust this folder\n\nEnter to confirm · Esc to cancel\n"
	clearComposerPane    = "Welcome back\n\n❯ \n"
	answeredLegacyDialog = "Do you trust the files in this folder?\n❯ Yes  No\n\n" + legacyTrustFooter
)

// dialogHoldPane is a pane whose capture, process and size a test changes
// between polls.
type dialogHoldPane struct {
	content string
	// identity is tmux's answer to paneIdentityFormat: "pid width height".
	identity string
	keys     []string
}

// dialogHoldSession is a Claude session on pane, on the virtual trust clock.
func dialogHoldSession(t *testing.T, pane *dialogHoldPane) (*TmuxSession, func(time.Duration)) {
	t.Helper()
	advance := setClaudeTrustClock(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if joined := strings.Join(c.Args, " "); strings.Contains(joined, "send-keys") {
				pane.keys = append(pane.keys, joined)
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "pane_pid") {
				return []byte(pane.identity), nil
			}
			if strings.Contains(joined, "display-message") {
				return []byte("0 0 0"), nil
			}
			return []byte(pane.content), nil
		},
	}
	return newTmuxSession(toTmuxName("held", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec), advance
}

// raisedLines counts the log lines that report a hold being surfaced.
func raisedLines(log string) int {
	return strings.Count(log, "af is waiting on")
}

// #7635: a dialog the gate holds on an unchanged pane says nothing until the
// settle time has passed since the pane last changed, then says why, once. The
// gate goes on holding and sends no key throughout.
func TestIssue7635HeldUnreadableDialogIsSurfacedOnceWithItsReason(t *testing.T) {
	_, warnings, _ := captureTrustPromptLogs(t)
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12"}
	session, advance := dialogHoldSession(t, pane)
	start := claudeTrustNow()

	// One poll a second, as the daemon does, up to one second short of the bound.
	for elapsed := time.Duration(0); elapsed < claudeDialogHoldSettle; elapsed += time.Second {
		require.True(t, session.CheckAndHandleTrustPrompt(), "the dialog is still in the way")
		_, raised := session.DialogHoldNotice()
		require.False(t, raised, "nothing is said %s into a hold that may still resolve", elapsed)
		advance(time.Second)
	}
	require.Zero(t, raisedLines(warnings.String()))

	require.True(t, session.CheckAndHandleTrustPrompt())
	notice, raised := session.DialogHoldNotice()
	require.True(t, raised, "an unchanged held pane is surfaced once the settle time has passed")
	require.Equal(t, "a folder-trust dialog it can't read (pane 60×12)", notice.Subject)
	require.True(t, notice.Since.Equal(start), "the clock runs from the pane's last change")
	require.Equal(t,
		"af is waiting on a folder-trust dialog it can't read (pane 60×12). Widen the pane, or answer it in the session's agent tab.",
		notice.Reason())
	require.Equal(t, 1, raisedLines(warnings.String()), "one log line when the notice is raised")

	// Later polls of the same held pane: the same notice, and no second line.
	for i := 0; i < 30; i++ {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt())
		again, still := session.DialogHoldNotice()
		require.True(t, still)
		require.Equal(t, notice, again, "one held pane is one notice, not one per poll")
	}
	require.Equal(t, 1, raisedLines(warnings.String()), "raised once, not once per poll")
	require.Empty(t, pane.keys, "surfacing a hold sends no key")
}

// #7635: the settle time is measured from the pane's last change. A held pane
// that changes, and is still held, starts over.
func TestIssue7635HeldPaneThatChangesRestartsTheClock(t *testing.T) {
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 43 12"}
	session, advance := dialogHoldSession(t, pane)

	require.True(t, session.CheckAndHandleTrustPrompt())
	advance(claudeDialogHoldSettle - time.Second)
	require.True(t, session.CheckAndHandleTrustPrompt())

	// The pane is narrowed one second before the bound: a new capture.
	advance(time.Second)
	pane.content, pane.identity = heldUnreadableDialogRewrapped, "4242 41 12"
	changedAt := claudeTrustNow()
	require.True(t, session.CheckAndHandleTrustPrompt())
	_, raised := session.DialogHoldNotice()
	require.False(t, raised, "the first pane's time does not count for the changed one")

	advance(claudeDialogHoldSettle - time.Second)
	require.True(t, session.CheckAndHandleTrustPrompt())
	_, raised = session.DialogHoldNotice()
	require.False(t, raised)

	advance(time.Second)
	require.True(t, session.CheckAndHandleTrustPrompt())
	notice, raised := session.DialogHoldNotice()
	require.True(t, raised)
	require.True(t, notice.Since.Equal(changedAt))
	require.Equal(t, "a folder-trust dialog it can't read (pane 41×12)", notice.Subject)

	// A raised notice is withdrawn by a change too, and comes back only after
	// the changed pane has settled.
	pane.content = heldUnreadableDialog
	require.True(t, session.CheckAndHandleTrustPrompt())
	_, raised = session.DialogHoldNotice()
	require.False(t, raised, "a pane that changed is not the pane the notice was about")
	require.Empty(t, pane.keys)
}

// #7635: the notice goes when the hold resolves — here the dialog is answered
// by hand and the pane reads clear.
func TestIssue7635ResolvedHoldClearsTheNotice(t *testing.T) {
	_, warnings, _ := captureTrustPromptLogs(t)
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12"}
	session, advance := dialogHoldSession(t, pane)

	require.True(t, session.CheckAndHandleTrustPrompt())
	advance(claudeDialogHoldSettle)
	require.True(t, session.CheckAndHandleTrustPrompt())
	_, raised := session.DialogHoldNotice()
	require.True(t, raised)

	pane.content = clearComposerPane
	require.False(t, session.CheckAndHandleTrustPrompt(), "the pane shows no dialog")
	_, raised = session.DialogHoldNotice()
	require.False(t, raised, "a resolved hold has no notice")

	// The same dialog again later is a new hold: its own clock, its own line.
	pane.content = heldUnreadableDialog
	advance(time.Minute)
	require.True(t, session.CheckAndHandleTrustPrompt())
	_, raised = session.DialogHoldNotice()
	require.False(t, raised, "a new hold starts its own clock")
	advance(claudeDialogHoldSettle)
	require.True(t, session.CheckAndHandleTrustPrompt())
	_, raised = session.DialogHoldNotice()
	require.True(t, raised)
	require.Equal(t, 2, raisedLines(warnings.String()), "two holds, two lines")
	require.Empty(t, pane.keys)
}

// #7635: a pane the gate answers is never surfaced, however long its capture
// stays the same. A poll in which af pressed a key is progress, not a hold.
func TestIssue7635AnsweredPaneNeverRaisesTheNotice(t *testing.T) {
	_, warnings, _ := captureTrustPromptLogs(t)
	pane := &dialogHoldPane{content: answeredLegacyDialog, identity: "4242 80 24"}
	session, advance := dialogHoldSession(t, pane)

	for i := 0; i < 5; i++ {
		require.True(t, session.CheckAndHandleTrustPrompt())
		_, raised := session.DialogHoldNotice()
		require.False(t, raised, "poll %d: an answered dialog is not a hold", i)
		advance(claudeDialogHoldSettle)
	}
	require.NotEmpty(t, pane.keys, "the gate answers this pane, as it does on master")
	require.Zero(t, raisedLines(warnings.String()))

	// And a pane with no dialog at all raises nothing.
	pane.content = clearComposerPane
	for i := 0; i < 3; i++ {
		require.False(t, session.CheckAndHandleTrustPrompt())
		advance(claudeDialogHoldSettle)
	}
	_, raised := session.DialogHoldNotice()
	require.False(t, raised)
	require.Zero(t, raisedLines(warnings.String()))
}

// #7635: the clock belongs to the pane process it started against. A pane
// respawned under an identical capture does not inherit it.
func TestIssue7635RespawnedPaneDoesNotInheritTheClock(t *testing.T) {
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12"}
	session, advance := dialogHoldSession(t, pane)

	require.True(t, session.CheckAndHandleTrustPrompt())
	advance(claudeDialogHoldSettle)
	pane.identity = "5151 60 12" // a new pane process, the same bytes on screen
	respawnSeenAt := claudeTrustNow()
	require.True(t, session.CheckAndHandleTrustPrompt())
	_, raised := session.DialogHoldNotice()
	require.False(t, raised, "the old process's time is not the new one's")

	advance(claudeDialogHoldSettle)
	require.True(t, session.CheckAndHandleTrustPrompt())
	notice, raised := session.DialogHoldNotice()
	require.True(t, raised)
	require.True(t, notice.Since.Equal(respawnSeenAt))

	// tmux not answering for the size leaves the reason without one.
	fresh := &dialogHoldPane{content: heldUnreadableDialog, identity: "garbage"}
	other, advanceOther := dialogHoldSession(t, fresh)
	require.True(t, other.CheckAndHandleTrustPrompt())
	advanceOther(claudeDialogHoldSettle)
	require.True(t, other.CheckAndHandleTrustPrompt())
	notice, raised = other.DialogHoldNotice()
	require.True(t, raised)
	require.Equal(t, "a folder-trust dialog it can't read", notice.Subject)
}

// The recorder alone, with no pane: what it keeps, and when it raises.
func TestDialogHoldRecorder(t *testing.T) {
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	sumA, sumB := sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b"))

	var r dialogHoldRecorder
	_, raised := r.current()
	require.False(t, raised)

	since, started, raised := r.observe(at, sumA, 1)
	require.True(t, started)
	require.False(t, raised)
	require.True(t, since.Equal(at))
	r.bindPane(since, "10")

	since, started, _ = r.observe(at.Add(5*time.Second), sumA, 1)
	require.False(t, started, "the same capture keeps its clock")
	require.True(t, since.Equal(at))

	// A different length with the same hash is still a different capture.
	since, started, _ = r.observe(at.Add(6*time.Second), sumA, 2)
	require.True(t, started)
	require.True(t, since.Equal(at.Add(6*time.Second)))
	r.bindPane(since, "10")
	r.bindPane(at, "99") // a clock that no longer runs binds nothing
	require.False(t, r.raise(at.Add(20*time.Second), at, "10", "x"), "a clock that was restarted cannot raise")

	require.True(t, r.raise(at.Add(20*time.Second), since, "10", "a dialog"))
	notice, raised := r.current()
	require.True(t, raised)
	require.Equal(t, DialogHoldNotice{Subject: "a dialog", Since: since}, notice)
	require.False(t, r.raise(at.Add(21*time.Second), since, "10", "again"), "one hold raises once")
	_, _, raised = r.observe(at.Add(22*time.Second), sumA, 2)
	require.True(t, raised)

	// A changed capture withdraws the notice.
	since, started, raised = r.observe(at.Add(30*time.Second), sumB, 2)
	require.True(t, started)
	require.False(t, raised)
	_, raised = r.current()
	require.False(t, raised)

	// Another pane process under the same capture restarts the clock.
	r.bindPane(since, "10")
	require.False(t, r.raise(at.Add(45*time.Second), since, "11", "a dialog"))
	since, started, _ = r.observe(at.Add(46*time.Second), sumB, 2)
	require.False(t, started)
	require.True(t, since.Equal(at.Add(45*time.Second)), "restarted when the new process was seen")
	require.True(t, r.raise(at.Add(60*time.Second), since, "11", "a dialog"))

	r.clear()
	_, raised = r.current()
	require.False(t, raised)
	_, started, _ = r.observe(at.Add(61*time.Second), sumB, 2)
	require.True(t, started, "a cleared recorder remembers no capture")
}

// #7635: the reason names the hold the way the gate read it. A footer no
// supported build paints — a future rewording — is a dialog af can't prove
// whole, and a dialog af reads but will not answer is said to be that.
func TestIssue7635HoldSubjectFollowsTheGatesOwnReading(t *testing.T) {
	const question = "Quick safety check: Is this a project you created or one you trust? (Like your own code.\n\n"
	const options = "❯ No, exit\n  Yes, I trust this folder\n\n"
	for _, tt := range []struct{ name, frame, want string }{
		{"the question wrapped by a narrow pane", heldUnreadableDialog, "a folder-trust dialog it can't read"},
		{"a footer reworded by a future build", question + options + "Enter to confirm • Esc to cancel\n", "a folder-trust dialog it can't prove whole"},
		{"a footer only begun", question + options + "Enter to confirm · Esc\n", "a folder-trust dialog it can't prove whole"},
		{"a whole dialog with no cursor row to aim at",
			"Quick safety check:\nIs this a project you created or one you trust?\n\n  1. No, exit\n  2. Yes, I trust this folder\n\nEnter to confirm · Esc to cancel\n",
			"a Claude Code dialog it won't answer on its own"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, claudeDialogHoldSubject(claudeScreenOf(tt.frame)))
		})
	}
}
