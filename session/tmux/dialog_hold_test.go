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
	// identity is tmux's answer to paneIdentityFormat: "pid width height
	// session_created".
	identity string
	keys     []string
	// identityReads counts tmux calls for the pane's identity.
	identityReads int
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
				pane.identityReads++
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
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12 1763553600"}
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
	require.Equal(t, "a folder-trust dialog it can't read (pane 60×12)", notice.Subject())
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
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 43 12 1763553600"}
	session, advance := dialogHoldSession(t, pane)

	require.True(t, session.CheckAndHandleTrustPrompt())
	advance(claudeDialogHoldSettle - time.Second)
	require.True(t, session.CheckAndHandleTrustPrompt())

	// The pane is narrowed one second before the bound: a new capture.
	advance(time.Second)
	pane.content, pane.identity = heldUnreadableDialogRewrapped, "4242 41 12 1763553600"
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
	require.Equal(t, "a folder-trust dialog it can't read (pane 41×12)", notice.Subject())

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
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12 1763553600"}
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
	pane := &dialogHoldPane{content: answeredLegacyDialog, identity: "4242 80 24 1763553600"}
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

// #7648: the pane is identified lazily, when a settled hold is raised — a
// hold that repaints costs no tmux call until then. A pane respawned under an
// identical capture inside the window is therefore the pane the notice names:
// the capture genuinely was unchanged for the whole settle, and the key names
// the incarnation read at the raise, never a process already retired. A
// respawn after the raise is caught by the recheck instead.
func TestIssue7648RespawnedPaneIsNamedByItsKey(t *testing.T) {
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12 1763553600"}
	session, advance := dialogHoldSession(t, pane)
	start := claudeTrustNow()

	require.True(t, session.CheckAndHandleTrustPrompt())
	advance(claudeDialogHoldSettle)
	pane.identity = "5151 60 12 1763553900" // a new pane process, the same bytes on screen
	require.True(t, session.CheckAndHandleTrustPrompt())
	notice, raised := session.DialogHoldNotice()
	require.True(t, raised, "the capture was unchanged for the whole settle")
	require.True(t, notice.Since.Equal(start))
	require.Equal(t,
		dialogHoldKey("5151@1763553900", sha256.Sum256([]byte(heldUnreadableDialog)), len(heldUnreadableDialog)),
		notice.Key, "the key names the pane that is there, not the retired process")
	require.Equal(t, 1, pane.identityReads, "one read, at the raise")
	require.Empty(t, pane.keys)
}

// #7648: a held pane whose capture keeps changing costs no tmux call beyond
// the gate's own capture. It is identified once it settles, when the notice
// is raised — never per repaint under inputMu.
func TestIssue7648RepaintingHeldPaneCostsNoTmuxCall(t *testing.T) {
	_, warnings, _ := captureTrustPromptLogs(t)
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12 1763553600"}
	session, advance := dialogHoldSession(t, pane)

	// Two frames the gate holds at the same clause, alternating on every
	// poll: the capture changes, the hold does not, and the clock starts over.
	frames := []string{heldUnreadableDialog, heldUnreadableDialogRewrapped}
	for i := 0; i < int(3*claudeDialogHoldSettle/time.Second); i++ {
		pane.content = frames[i%2]
		require.True(t, session.CheckAndHandleTrustPrompt(), "both frames are holds the gate does not answer")
		_, raised := session.DialogHoldNotice()
		require.False(t, raised, "a hold that keeps repainting never settles")
		advance(time.Second)
	}
	require.Zero(t, pane.identityReads, "a hold that never settles is never identified")
	require.Zero(t, raisedLines(warnings.String()))

	// Once it settles, one read binds the pane and raises the notice.
	pane.content = heldUnreadableDialog
	require.True(t, session.CheckAndHandleTrustPrompt())
	for i := 0; i < int(claudeDialogHoldSettle/time.Second); i++ {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt())
	}
	notice, raised := session.DialogHoldNotice()
	require.True(t, raised)
	require.Equal(t, "a folder-trust dialog it can't read (pane 60×12)", notice.Subject())
	require.Equal(t, 1, pane.identityReads, "one read, at the raise")
	require.Empty(t, pane.keys, "surfacing a hold sends no key")
}

// A raised notice does not outlive its pane. A pane respawned outside af —
// `tmux respawn-pane`, which Start never sees — can show the same bytes, and
// the capture's hash cannot tell the two apart. The pane is identified again
// once per settle time: the notice is withdrawn within one settle, and the
// replacement's hold is surfaced after its own, under its own key.
func TestIssue7635RaisedNoticeDoesNotOutliveItsPane(t *testing.T) {
	_, warnings, _ := captureTrustPromptLogs(t)
	pane := &dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12 1763553600"}
	session, advance := dialogHoldSession(t, pane)

	require.True(t, session.CheckAndHandleTrustPrompt())
	advance(claudeDialogHoldSettle)
	require.True(t, session.CheckAndHandleTrustPrompt())
	first, raised := session.DialogHoldNotice()
	require.True(t, raised)
	require.Equal(t, 1, pane.identityReads, "one read, at the raise — nothing before the pane settles")

	// The same pane, polled once a second for three settle times: confirmed
	// once per settle, the notice unchanged, one line in the log.
	polls := int(3 * claudeDialogHoldSettle / time.Second)
	for i := 0; i < polls; i++ {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt())
		again, still := session.DialogHoldNotice()
		require.True(t, still)
		require.Equal(t, first, again)
	}
	require.Equal(t, 1+3, pane.identityReads, "a raised notice costs one tmux call per settle time, not one per poll")
	require.Equal(t, 1, raisedLines(warnings.String()))

	// tmux not answering proves nothing: the notice stands.
	pane.identity = "garbage"
	advance(claudeDialogHoldSettle)
	require.True(t, session.CheckAndHandleTrustPrompt())
	_, raised = session.DialogHoldNotice()
	require.True(t, raised, "an unanswered recheck is not evidence the pane changed")

	// Respawned outside af, the same bytes on screen.
	pane.identity = "4242 60 12 1763557200"
	withdrawn := false
	var respawnSeenAt time.Time
	for elapsed := time.Duration(0); elapsed <= claudeDialogHoldSettle; elapsed += time.Second {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt())
		if _, raised = session.DialogHoldNotice(); !raised {
			withdrawn, respawnSeenAt = true, claudeTrustNow()
			break
		}
	}
	require.True(t, withdrawn, "the retired pane's notice is withdrawn within one settle time")

	advance(claudeDialogHoldSettle - time.Second)
	require.True(t, session.CheckAndHandleTrustPrompt())
	_, raised = session.DialogHoldNotice()
	require.False(t, raised, "the replacement's hold has its own settle")
	advance(time.Second)
	require.True(t, session.CheckAndHandleTrustPrompt())
	second, raised := session.DialogHoldNotice()
	require.True(t, raised)
	require.True(t, second.Since.Equal(respawnSeenAt))
	require.NotEqual(t, first.Key, second.Key, "and its own key, so a dismissal of the first does not silence it")
	require.Equal(t, 2, raisedLines(warnings.String()))
	require.Empty(t, pane.keys)
}

// #7635 re-review R1: a pane af cannot identify is never flagged. With no
// incarnation there is no key that names a pane, and a key that names none
// could match any later one. Once tmux answers, the hold the unchanged
// capture has been in is surfaced against the pane it was just read from.
func TestIssue7635UnidentifiedPaneRaisesNoNotice(t *testing.T) {
	_, warnings, _ := captureTrustPromptLogs(t)
	for _, unreadable := range []string{"garbage", "", "0 0 0", "4242 60 12", "4242 sixty 12 1763553600"} {
		pane := &dialogHoldPane{content: heldUnreadableDialog, identity: unreadable}
		session, advance := dialogHoldSession(t, pane)
		start := claudeTrustNow()
		for i := 0; i < 4; i++ {
			require.True(t, session.CheckAndHandleTrustPrompt(), "the gate holds whatever tmux says about the pane")
			_, raised := session.DialogHoldNotice()
			require.False(t, raised, "identity %q: an unidentified pane is not flagged", unreadable)
			advance(claudeDialogHoldSettle)
		}
		require.Zero(t, raisedLines(warnings.String()))

		pane.identity = "4242 60 12 1763553600"
		require.True(t, session.CheckAndHandleTrustPrompt())
		notice, raised := session.DialogHoldNotice()
		require.True(t, raised, "identified at last: the notice names the pane it was just read from")
		require.True(t, notice.Since.Equal(start), "the capture was unchanged the whole time")
		require.Regexp(t, "^[0-9a-f]{32}$", notice.Key)
		require.Empty(t, pane.keys)
		warnings.Reset()
	}
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

	since, started, _ = r.observe(at.Add(5*time.Second), sumA, 1)
	require.False(t, started, "the same capture keeps its clock")
	require.True(t, since.Equal(at))

	// A different length with the same hash is still a different capture.
	since, started, _ = r.observe(at.Add(6*time.Second), sumA, 2)
	require.True(t, started)
	require.True(t, since.Equal(at.Add(6*time.Second)))

	pane10 := paneIdentity{pid: "10", created: "c", width: 60, height: 12}
	require.False(t, r.raise(at.Add(20*time.Second), at, pane10, DialogHoldOther), "a clock that was restarted cannot raise")

	require.True(t, r.raise(at.Add(20*time.Second), since, pane10, DialogHoldFolderUnreadable))
	notice, raised := r.current()
	require.True(t, raised)
	require.Equal(t, DialogHoldNotice{
		Kind: DialogHoldFolderUnreadable, Width: 60, Height: 12, Since: since,
		Key: dialogHoldKey("10@c", sumA, 2),
	}, notice)
	require.False(t, r.raise(at.Add(21*time.Second), since, pane10, DialogHoldOther), "one hold raises once")
	_, _, raised = r.observe(at.Add(22*time.Second), sumA, 2)
	require.True(t, raised)

	// A changed capture withdraws the notice.
	since, started, raised = r.observe(at.Add(30*time.Second), sumB, 2)
	require.True(t, started)
	require.False(t, raised)
	_, raised = r.current()
	require.False(t, raised)

	// An unknown incarnation raises nothing and binds nothing, however often.
	for _, unknown := range []paneIdentity{{}, {pid: "10"}, {created: "c"}} {
		require.Empty(t, unknown.incarnation())
		require.False(t, r.raise(at.Add(45*time.Second), since, unknown, DialogHoldOther))
	}
	// The raise binds the pane it read: the notice names that pane.
	require.True(t, r.raise(at.Add(45*time.Second), since, pane10, DialogHoldOther))
	notice, raised = r.current()
	require.True(t, raised)
	require.Equal(t, dialogHoldKey("10@c", sumB, 2), notice.Key)

	// A recheck that finds another pane under the same bytes withdraws the
	// notice and binds the replacement: its hold re-settles against it.
	pane11 := paneIdentity{pid: "11", created: "c", width: 60, height: 12}
	r.recheck(at.Add(50*time.Second), pane11)
	_, raised = r.current()
	require.False(t, raised, "the retired pane's notice is withdrawn")
	since, started, _ = r.observe(at.Add(51*time.Second), sumB, 2)
	require.False(t, started)
	require.True(t, since.Equal(at.Add(50*time.Second)), "restarted when the new process was seen")

	// A third process before the raise restarts the clock against what is
	// there; the pane the next read names goes into the key.
	pane12 := paneIdentity{pid: "12", created: "c", width: 60, height: 12}
	require.False(t, r.raise(at.Add(61*time.Second), since, pane12, DialogHoldOther))
	since, started, _ = r.observe(at.Add(62*time.Second), sumB, 2)
	require.False(t, started)
	require.True(t, since.Equal(at.Add(61*time.Second)))
	require.True(t, r.raise(at.Add(72*time.Second), since, pane12, DialogHoldOther))
	notice, raised = r.current()
	require.True(t, raised)
	require.Equal(t, dialogHoldKey("12@c", sumB, 2), notice.Key)

	r.clear()
	_, raised = r.current()
	require.False(t, raised)
	_, started, _ = r.observe(at.Add(80*time.Second), sumB, 2)
	require.True(t, started, "a cleared recorder remembers no capture")
}

// heldMCPServerDialog is a frame the gate holds at its MCP-server clause, as
// unreadable: 2.1.322's server dialog wrapped by a 32-column pane, under a
// draft whose last row quotes the folder-trust question. Review R2: the first
// subject reader skipped the MCP clauses and called it a folder-trust dialog.
const heldMCPServerDialog = "╭──────────────────────────────╮\n│ ! ls                         │\n│ files in this folder?        │\n" +
	"New MCP server found in this\nproject: probe\n\nMCP servers may execute code or\naccess system resources. All\n" +
	"tool calls require approval.\nLearn more in the MCP\ndocumentation.\n\n  Use this MCP server\n  Use this and all future MCP\n" +
	"  servers in this project\n❯ Continue without using this\nMCP server\n\nEnter to confirm · Esc to cancel"

// #7635: the kind names the clause of the gate that held. A footer no
// supported build paints — a future rewording — is a dialog af can't prove
// whole, an MCP dialog is named as one, and a dialog af reads but will not
// answer is said to be that. The mapping was checked against the gate mirror
// on every held frame of hc_adv5 and comp7619; these are one frame of each.
func TestIssue7635HoldKindFollowsTheGatesOwnClause(t *testing.T) {
	const question = "Quick safety check: Is this a project you created or one you trust? (Like your own code.\n\n"
	const options = "❯ No, exit\n  Yes, I trust this folder\n\n"
	for _, tt := range []struct {
		name, frame string
		want        DialogHoldKind
		subject     string
	}{
		{"the question wrapped by a narrow pane", heldUnreadableDialog,
			DialogHoldFolderUnreadable, "a folder-trust dialog it can't read"},
		{"a footer reworded by a future build", question + options + "Enter to confirm • Esc to cancel\n",
			DialogHoldFolderPartial, "a folder-trust dialog it can't prove whole"},
		{"a footer only begun", question + options + "Enter to confirm · Esc\n",
			DialogHoldFolderPartial, "a folder-trust dialog it can't prove whole"},
		{"the MCP server dialog wrapped, under a draft quoting the folder question", heldMCPServerDialog,
			DialogHoldMCPUnreadable, "an MCP server dialog it can't read"},
		{"a whole dialog with no cursor row to aim at",
			"Quick safety check:\nIs this a project you created or one you trust?\n\n  1. No, exit\n  2. Yes, I trust this folder\n\nEnter to confirm · Esc to cancel\n",
			DialogHoldRefused, "a Claude Code dialog it won't answer on its own"},
		{"no dialog at all", clearComposerPane, DialogHoldOther, "a Claude Code dialog it can't read"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			kind := claudeDialogHoldKind(claudeScreenOf(tt.frame))
			require.Equal(t, tt.want, kind)
			require.Equal(t, tt.subject, DialogHoldSubject(kind, 0, 0))
		})
	}
}

// #7635 review R3: what af shows for a hold is built from a closed set — the
// kind's words and two integers. A kind outside the set, and a size that is
// not a plausible pane, add nothing of their own to the text.
func TestIssue7635HoldTextIsBuiltFromAClosedSet(t *testing.T) {
	require.Equal(t, "a folder-trust dialog it can't read (pane 60×12)", DialogHoldSubject(DialogHoldFolderUnreadable, 60, 12))
	require.Equal(t,
		"af is waiting on an MCP server dialog it can't prove whole (pane 80×24). Widen the pane, or answer it in the session's agent tab.",
		DialogHoldReason(DialogHoldMCPPartial, 80, 24))

	const generic = "a Claude Code dialog it can't read"
	for _, kind := range []DialogHoldKind{"", "folder-unreadable\n\x1b[31mURGENT", "a newer build's kind", "FOLDER-UNREADABLE"} {
		require.Equal(t, DialogHoldOther, KnownDialogHoldKind(kind))
		require.Equal(t, generic, DialogHoldSubject(kind, 0, 0), "an unknown kind %q reads as the generic hold", kind)
	}
	for _, size := range [][2]int{{0, 0}, {60, 0}, {0, 12}, {-1, 12}, {60, -12}, {10000, 12}, {60, 1 << 30}} {
		require.Equal(t, "a folder-trust dialog it can't read", DialogHoldSubject(DialogHoldFolderUnreadable, size[0], size[1]),
			"size %v is not printed", size)
	}
	for kind := range dialogHoldSubjects {
		require.Equal(t, kind, KnownDialogHoldKind(kind))
	}
}

// #7635 review R4: the key names the held pane and nothing else, so a daemon
// that restarts computes the same key for the pane it had already flagged.
func TestIssue7635HoldKeyIsStableForTheSamePane(t *testing.T) {
	raisedKey := func(pane *dialogHoldPane) string {
		t.Helper()
		session, advance := dialogHoldSession(t, pane)
		require.True(t, session.CheckAndHandleTrustPrompt())
		advance(claudeDialogHoldSettle)
		require.True(t, session.CheckAndHandleTrustPrompt())
		notice, raised := session.DialogHoldNotice()
		require.True(t, raised)
		return notice.Key
	}
	// Two TmuxSession values on the same pane: before and after a restart.
	before := raisedKey(&dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12 1763553600"})
	after := raisedKey(&dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12 1763553600"})
	require.Equal(t, before, after, "the same pane showing the same bytes keeps its key")
	require.Regexp(t, "^[0-9a-f]{32}$", before)

	require.NotEqual(t, before, raisedKey(&dialogHoldPane{content: heldUnreadableDialog, identity: "5151 60 12 1763553600"}),
		"another pane process is another hold")
	// Re-review R1: the same process id in a pane started again — a sandbox
	// re-provisioned into a fresh PID namespace — is another incarnation.
	require.NotEqual(t, before, raisedKey(&dialogHoldPane{content: heldUnreadableDialog, identity: "4242 60 12 1763557200"}),
		"the same pid in a tmux session created at another time is another hold")
	require.NotEqual(t, before, raisedKey(&dialogHoldPane{content: heldUnreadableDialogRewrapped, identity: "4242 60 12 1763553600"}),
		"a pane showing something else is another hold")
}
