package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// Regression tests for #3579.
//
// Claude Code 2.1.257 reordered the folder-trust dialog so that "No, exit" is
// the PRESELECTED option. af dismissed that dialog by tapping Enter, which was
// written when the affirmative option was the default — so af's own dismissal
// chose to quit the agent, and the create failed with the unrelated-looking
// "capture-pane: exit status 1".
//
// The gap that let it ship is what these tests close: claudeTrustPromptPresent
// asserted the dialog was PRESENT, and nothing asserted WHICH option the cursor
// was on when Enter was sent. So the fixtures below are not static strings —
// they are a live picker that moves its cursor in response to Down/Up and
// records the label under the cursor at the moment Enter arrives. A test that
// only counted keystrokes would pass on the buggy code.

const (
	claudeTrustYesLabel = "Yes, I trust this folder"
	claudeTrustNoLabel  = "No, exit"
)

// claudeFolderTrustPane models Claude Code's folder-trust modal as an actual
// picker: Down/Up move the cursor, Enter COMMITS whichever option owns it. The
// committed label is the oracle — pressing Enter on "No, exit" is what quits
// the agent in production, so a test that reaches that state has reproduced the
// defect regardless of how many keys af sent to get there.
type claudeFolderTrustPane struct {
	mu       sync.Mutex
	options  []string
	selected int
	// committed records the label under the cursor for every Enter af injected.
	committed []string
	// ordinals renders "N. " prefixes on the option rows, as some Claude Code
	// builds do. The label — never the ordinal — must be what af navigates by.
	ordinals bool
	// boxed wraps the modal in the box-drawing frame Claude Code draws around
	// it, so the parser is exercised against bordered rows and not just the
	// bare lines the issue's poller printed.
	boxed bool
	// staleFrames makes the N captures that FOLLOW a movement key return the
	// pre-movement frame, modelling the terminal repaint race between
	// `send-keys` returning and the redraw actually landing.
	staleFrames  int
	pendingStale int
	stale        string
	// frozen holds the LAST painted frame on screen no matter what the picker's
	// internal selection does, modelling an application whose redraw has not
	// reached the terminal yet.
	frozen bool
	// deaf is how many movement keys the picker DROPS before it starts acting on
	// them — Claude Code does exactly this in the instant after it paints the
	// dialog, before it is reading input.
	deaf int
}

func (p *claudeFolderTrustPane) freeze(frozen bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if frozen && p.stale == "" {
		p.stale = p.render()
	}
	p.frozen = frozen
}

func (p *claudeFolderTrustPane) render() string {
	var b strings.Builder
	write := func(line string) {
		if p.boxed {
			fmt.Fprintf(&b, "│ %-48s │\n", line)
			return
		}
		b.WriteString(line + "\n")
	}
	if p.boxed {
		b.WriteString("╭" + strings.Repeat("─", 50) + "╮\n")
	}
	write("Quick safety check:")
	write("Is this a project you created or one you trust?")
	write("")
	for idx, option := range p.options {
		row := "  "
		if idx == p.selected {
			row = "❯ "
		}
		if p.ordinals {
			row += fmt.Sprintf("%d. ", idx+1)
		}
		write(row + option)
	}
	write("")
	write("Enter to confirm · Esc to cancel")
	if p.boxed {
		b.WriteString("╰" + strings.Repeat("─", 50) + "╯\n")
	}
	return b.String()
}

// capture answers a capture-pane, honoring any pending repaint lag.
func (p *claudeFolderTrustPane) capture() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.committed) > 0 {
		// The dialog is gone once af answered it; the composer is what a pane
		// that accepted the affirmative option actually shows next.
		return "╭──────────────────────────────────────────╮\n│ > Try \"how do I…\"                        │\n╰──────────────────────────────────────────╯\n? for shortcuts\n"
	}
	if p.frozen {
		return p.stale
	}
	if p.pendingStale > 0 {
		p.pendingStale--
		return p.stale
	}
	return p.render()
}

// key applies one injected keystroke to the picker.
func (p *claudeFolderTrustPane) key(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.committed) > 0 {
		return
	}
	if p.deaf > 0 && (name == "Down" || name == "Up") {
		p.deaf--
		return
	}
	switch name {
	case "Down":
		p.stale, p.pendingStale = p.render(), p.staleFrames
		p.selected = (p.selected + 1) % len(p.options)
	case "Up":
		p.stale, p.pendingStale = p.render(), p.staleFrames
		p.selected = (p.selected - 1 + len(p.options)) % len(p.options)
	case "Enter":
		p.committed = append(p.committed, p.options[p.selected])
	}
}

func (p *claudeFolderTrustPane) selectedIndex() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.selected
}

func (p *claudeFolderTrustPane) committedLabels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.committed...)
}

// driveClaudeTrustPane runs CheckAndHandleTrustPrompt against the live picker
// until it answers the dialog or the poll budget runs out, mirroring
// task.DismissTrustPrompt's repeated calls. It returns every send-keys command
// issued across the whole run.
func driveClaudeTrustPane(t *testing.T, pane *claudeFolderTrustPane, polls int) []string {
	t.Helper()
	advance := setClaudeTrustClock(t)
	session, keys := claudeTrustSession(t, pane)
	for i := 0; i < polls; i++ {
		// One daemon poll interval between checks. af declines to type into a
		// dialog it has only just seen (claudeTrustSettleDelay), so a driver
		// that never let time pass would never get a keystroke at all — which
		// is the production behavior, not a harness detail.
		advance(time.Second)
		if !session.CheckAndHandleTrustPrompt() {
			break
		}
		if len(pane.committedLabels()) > 0 {
			break
		}
	}
	return *keys
}

// claudeTrustSession binds a session to the live picker and returns it together
// with the send-keys commands it issues, so a test can poll it by hand.
func claudeTrustSession(t *testing.T, pane *claudeFolderTrustPane) (*TmuxSession, *[]string) {
	t.Helper()
	keys := &[]string{}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "send-keys") {
				return nil
			}
			*keys = append(*keys, joined)
			for _, name := range injectedKeyNames([]string{joined}) {
				pane.key(name)
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return []byte("0 0 0"), nil
			}
			return []byte(pane.capture()), nil
		},
	}
	return newTmuxSession(toTmuxName("trust", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec), keys
}

// This is the assertion the issue names as missing: not that a keystroke was
// sent, but WHICH option the cursor was on when Enter reached the dialog. It
// fails on master for every fixture whose preselected row is "No, exit".
func TestCheckAndHandleTrustPrompt_ClaudeFolderTrustCommitsTheAffirmativeOption(t *testing.T) {
	tests := []struct {
		name     string
		pane     claudeFolderTrustPane
		wantKeys []string
	}{
		{
			name: "No first, preselected — Claude Code 2.1.257/2.1.258 (#3579)",
			pane: claudeFolderTrustPane{
				options:  []string{claudeTrustNoLabel, claudeTrustYesLabel},
				selected: 0,
			},
			wantKeys: []string{"Down", "Enter"},
		},
		{
			name: "Yes first, preselected — the historical order",
			pane: claudeFolderTrustPane{
				options:  []string{claudeTrustYesLabel, claudeTrustNoLabel},
				selected: 0,
			},
			wantKeys: []string{"Enter"},
		},
		{
			name: "Yes first, cursor parked on No",
			pane: claudeFolderTrustPane{
				options:  []string{claudeTrustYesLabel, claudeTrustNoLabel},
				selected: 1,
			},
			wantKeys: []string{"Up", "Enter"},
		},
		{
			name: "No first, numbered rows",
			pane: claudeFolderTrustPane{
				options:  []string{claudeTrustNoLabel, claudeTrustYesLabel},
				selected: 0,
				ordinals: true,
			},
			wantKeys: []string{"Down", "Enter"},
		},
		{
			name: "No first, drawn inside the modal frame",
			pane: claudeFolderTrustPane{
				options:  []string{claudeTrustNoLabel, claudeTrustYesLabel},
				selected: 0,
				boxed:    true,
			},
			wantKeys: []string{"Down", "Enter"},
		},
		{
			name: "three options, affirmative last",
			pane: claudeFolderTrustPane{
				options:  []string{claudeTrustNoLabel, "No, and don't ask again", claudeTrustYesLabel},
				selected: 0,
				ordinals: true,
			},
			wantKeys: []string{"Down", "Down", "Enter"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pane := tt.pane
			keys := driveClaudeTrustPane(t, &pane, 6)
			require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels(),
				"af must confirm the affirmative option, exactly once; keystrokes were %v", keys)
			require.Equal(t, tt.wantKeys, injectedKeyNames(keys),
				"af must reach the affirmative row by reading the pane, not by a fixed key sequence")
		})
	}
}

// The repaint between `send-keys Down` and the redraw is asynchronous. A stale
// frame must never be read as "the cursor did not move" and answered with a
// second movement key, and it must never be answered with Enter either — the
// confirmation read is what makes the selection safe.
func TestCheckAndHandleTrustPrompt_ClaudeFolderTrustWaitsForTheRepaint(t *testing.T) {
	pane := claudeFolderTrustPane{
		options:     []string{claudeTrustNoLabel, claudeTrustYesLabel},
		selected:    0,
		staleFrames: 2,
	}
	keys := driveClaudeTrustPane(t, &pane, 6)
	require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels(),
		"a lagging repaint must not change which option af confirms; keystrokes were %v", keys)
	require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(keys),
		"af must wait for the redraw rather than re-sending the movement key; got %v", keys)
}

// injectedKeyNames flattens the recorded send-keys commands into the key names
// that reached the pane, in order.
func injectedKeyNames(cmds []string) []string {
	var keys []string
	for _, c := range cmds {
		fields := strings.Fields(c)
		for idx, field := range fields {
			if field == "send-keys" {
				// skip "send-keys -t <target>"
				keys = append(keys, fields[idx+3:]...)
				break
			}
		}
	}
	return keys
}

// dialogFixture renders a folder-trust modal without driving it, for the
// parser's own table.
func dialogFixture(pane claudeFolderTrustPane) string { return pane.render() }

// The parser is what decides which key af presses, so it is tested on its own
// against both orders the dialog has shipped with — and against the shapes it
// must REFUSE rather than guess at. A refusal costs a retry and a loud log; a
// guess costs the user's agent (#3579).
func TestParseClaudeFolderTrustDialog(t *testing.T) {
	noFirst := dialogFixture(claudeFolderTrustPane{
		options: []string{claudeTrustNoLabel, claudeTrustYesLabel}, selected: 0})
	yesFirst := dialogFixture(claudeFolderTrustPane{
		options: []string{claudeTrustYesLabel, claudeTrustNoLabel}, selected: 0})
	yesFirstOnNo := dialogFixture(claudeFolderTrustPane{
		options: []string{claudeTrustYesLabel, claudeTrustNoLabel}, selected: 1})

	t.Run("No first: the affirmative row is below the cursor", func(t *testing.T) {
		dialog, err := parseClaudeFolderTrustDialog(noFirst)
		require.NoError(t, err)
		require.Equal(t, claudeTrustNoLabel, dialog.selectedLabel())
		require.False(t, dialog.onAffirmative())
		require.Equal(t, "Down", dialog.keyTowardAffirmative())
	})

	t.Run("Yes first, preselected: nothing to move", func(t *testing.T) {
		dialog, err := parseClaudeFolderTrustDialog(yesFirst)
		require.NoError(t, err)
		require.Equal(t, claudeTrustYesLabel, dialog.selectedLabel())
		require.True(t, dialog.onAffirmative())
	})

	t.Run("Yes first, cursor below it", func(t *testing.T) {
		dialog, err := parseClaudeFolderTrustDialog(yesFirstOnNo)
		require.NoError(t, err)
		require.False(t, dialog.onAffirmative())
		require.Equal(t, "Up", dialog.keyTowardAffirmative())
	})

	t.Run("ordinals and box borders are chrome, not identity", func(t *testing.T) {
		for _, pane := range []claudeFolderTrustPane{
			{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}, ordinals: true},
			{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}, boxed: true},
			{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}, boxed: true, ordinals: true},
		} {
			dialog, err := parseClaudeFolderTrustDialog(dialogFixture(pane))
			require.NoError(t, err)
			require.Equal(t, claudeTrustNoLabel, dialog.selectedLabel())
			require.Equal(t, "Down", dialog.keyTowardAffirmative())
		}
	})

	t.Run("ANSI colour does not hide the rows", func(t *testing.T) {
		coloured := strings.ReplaceAll(noFirst, "❯", "\x1b[1m❯\x1b[0m")
		coloured = strings.ReplaceAll(coloured, claudeTrustYesLabel, "\x1b[32m"+claudeTrustYesLabel+"\x1b[0m")
		dialog, err := parseClaudeFolderTrustDialog(coloured)
		require.NoError(t, err)
		require.Equal(t, "Down", dialog.keyTowardAffirmative())
	})

	// Every case below must be an ERROR, never a best guess.
	refusals := []struct {
		name    string
		content string
		reason  string
	}{
		{
			name:    "neither row found",
			content: "Quick safety check:\nIs this a project you created or one you trust?\n",
			reason:  "no row carries the \"❯\" selection cursor and none is labelled",
		},
		{
			name: "no cursor: a frame caught mid-repaint",
			content: "Quick safety check:\nIs this a project you created or one you trust?\n\n" +
				"  1. " + claudeTrustNoLabel + "\n  2. " + claudeTrustYesLabel + "\n",
			reason: "no row carries",
		},
		{
			name: "affirmative label present but no cursor anywhere",
			content: "Is this a project you created or one you trust?\n" +
				claudeTrustYesLabel + "\n",
			reason: "no row carries",
		},
		{
			name: "two cursors: af cannot tell which row is selected",
			content: "Is this a project you created or one you trust?\n\n" +
				"❯ " + claudeTrustNoLabel + "\n❯ " + claudeTrustYesLabel + "\n",
			reason: "2 rows carry",
		},
		{
			name: "the label also appears elsewhere on screen",
			content: "❯ " + claudeTrustNoLabel + "\n  " + claudeTrustYesLabel + "\n" +
				"  " + claudeTrustYesLabel + "\n",
			reason: "2 rows are labelled",
		},
		{
			name: "a label carrying trailing prose is not an option row",
			content: "❯ Resume the previous conversation\n" +
				claudeTrustYesLabel + " (quoted from the docs)\n" +
				"Enter to confirm · Esc to cancel\n",
			reason: "no row is labelled",
		},
		{
			name: "the cursor and the label are not one picker",
			content: "❯ Resume the previous conversation\n\nsome output\n\n" +
				claudeTrustYesLabel + "\n\nEnter to confirm · Esc to cancel\n",
			reason: "not in one unbroken block",
		},
		{
			name:    "one option is not a picker",
			content: "❯ " + claudeTrustYesLabel + "\n\nEnter to confirm · Esc to cancel\n",
			reason:  "is the only row in its block",
		},
		{
			name:    "no modal footer under the options",
			content: "❯ " + claudeTrustNoLabel + "\n  " + claudeTrustYesLabel + "\n\nsomething else\n",
			reason:  "not followed by the modal's",
		},
		{
			name: "a quoted dialog with the agent's composer painted below it",
			content: "❯ " + claudeTrustNoLabel + "\n  " + claudeTrustYesLabel + "\n\n" +
				"Enter to confirm · Esc to cancel\n\n" +
				"╭──────────────────────────────╮\n│ > Type your message here     │\n╰──────────────────────────────╯\n",
			reason: "content is painted below",
		},
	}
	for _, tt := range refusals {
		t.Run("refuses: "+tt.name, func(t *testing.T) {
			_, err := parseClaudeFolderTrustDialog(tt.content)
			require.Error(t, err, "af must refuse a dialog it cannot read, not guess at it")
			require.Contains(t, err.Error(), tt.reason)
		})
	}
}

// A dialog af cannot read must cost a keystroke af does NOT send. The create's
// trust-prompt budget then fails the start loudly, which is recoverable; the
// alternative is #3579 — Enter on whatever happened to be selected.
func TestCheckAndHandleTrustPrompt_UnreadableClaudeFolderTrustDialogIsRefused(t *testing.T) {
	// The dialog is unmistakably present (claudeTrustPromptPresent matches) but
	// no row carries the cursor, so there is nothing af can aim at.
	content := "Quick safety check:\nIs this a project you created or one you trust?\n\n" +
		"  1. " + claudeTrustNoLabel + "\n  2. " + claudeTrustYesLabel + "\n\n" +
		"Enter to confirm · Esc to cancel\n"

	_, _, errLog := captureTrustPromptLogs(t)
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, content)
	require.True(t, handled, "the dialog is still in the way, so the caller may not type into the pane (#3302)")
	require.Empty(t, sentKeystrokes(cmds),
		"af must not press a key it cannot aim at the affirmative row; got %v", cmds)
	require.Contains(t, errLog.String(), claudeTrustYesLabel,
		"the refusal must name what af was trying to select")
}

// The MCP-server prompt and the legacy folder-trust wording render no
// "Yes, I trust this folder" row. Neither is what regressed, and both keep the
// historical Enter tap on Claude Code's own preselection.
func TestCheckAndHandleTrustPrompt_NonFolderClaudeGatesKeepTheEnterTap(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"MCP server trust", "New MCP server found. Do you trust this new MCP server?\n❯ 1. Yes\n  2. No\nEnter to confirm"},
		{"legacy folder-trust wording", "Do you trust the files in this folder?\n❯ Yes  No"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramClaude, tt.content)
			require.True(t, handled)
			require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)))
		})
	}
}

// A repaint that never lands must cost a poll, not a confirmation. af has no
// proof of where the cursor is, so it sends no Enter — and it does not re-send
// the movement key either, which is how a stale frame would make it overshoot.
// The window is driven on a virtual clock so the test does not sleep through it.
func TestCheckAndHandleTrustPrompt_ClaudeFolderTrustNeverRepaintsSoNothingIsConfirmed(t *testing.T) {
	setClaudeTrustClock(t)

	_, _, errLog := captureTrustPromptLogs(t)
	pane := claudeFolderTrustPane{
		options:     []string{claudeTrustNoLabel, claudeTrustYesLabel},
		staleFrames: 1 << 20, // the redraw never arrives
	}
	// Two polls: the first notes that the dialog appeared, the second is the
	// one that types into it.
	keys := driveClaudeTrustPane(t, &pane, 2)

	require.Empty(t, pane.committedLabels(), "af must not confirm a selection it could not verify")
	require.Equal(t, []string{"Down"}, injectedKeyNames(keys),
		"exactly one movement key, and no Enter behind it; got %v", keys)
	require.Empty(t, errLog.String(), "an unproven repaint is a retry, not an error")
}

// setClaudeTrustClock installs a virtual clock for the repaint window and
// returns a knob that advances it, so a test can age a pending movement without
// sleeping through it.
func setClaudeTrustClock(t *testing.T) func(time.Duration) {
	t.Helper()
	if claudeTrustVirtualNow != nil {
		return claudeTrustVirtualAdvance
	}
	now := time.Now()
	previousNow, previousSleep := claudeTrustNow, claudeTrustSleep
	claudeTrustVirtualNow = &now
	claudeTrustVirtualAdvance = func(d time.Duration) { now = now.Add(d) }
	t.Cleanup(func() {
		claudeTrustNow, claudeTrustSleep = previousNow, previousSleep
		claudeTrustVirtualNow, claudeTrustVirtualAdvance = nil, nil
	})
	claudeTrustNow = func() time.Time { return now }
	claudeTrustSleep = func(d time.Duration) { now = now.Add(d) }
	return claudeTrustVirtualAdvance
}

// claudeTrustVirtualNow/Advance let a test install the virtual clock and then
// hand the SAME clock to driveClaudeTrustPane, rather than shadowing it with a
// second one whose time never moves.
var (
	claudeTrustVirtualNow     *time.Time
	claudeTrustVirtualAdvance func(time.Duration)
)

// #3587 review, P1. `send-keys` returning is not proof the key landed, and a
// terminal that has not repainted is not proof it did not. If af re-sent the
// movement key on the next poll against a frame that had not caught up, the
// picker's real cursor — already moved — would be pushed BACK off the
// affirmative row; a redraw landing between the verification and confirmation
// captures could then let Enter reach "No, exit" and reproduce #3579 through
// the very code that fixes it.
//
// So a movement key af did not see land makes this handler read-only until a
// frame proves the cursor moved.
func TestCheckAndHandleTrustPrompt_ClaudeFolderTrustDoesNotResendAnUnverifiedMove(t *testing.T) {
	advance := setClaudeTrustClock(t)
	_, _, errLog := captureTrustPromptLogs(t)

	pane := &claudeFolderTrustPane{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}}
	session, keys := claudeTrustSession(t, pane)

	// The terminal stops repainting the moment af starts driving the picker.
	require.True(t, session.CheckAndHandleTrustPrompt()) // notes when the dialog appeared
	advance(claudeTrustSettleDelay)
	pane.freeze(true)
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{"Down"}, injectedKeyNames(*keys))
	require.Equal(t, 1, pane.selectedIndex(),
		"precondition: the picker's INTERNAL selection moved, even though the screen still shows the old row")

	for i := 0; i < 5; i++ {
		require.True(t, session.CheckAndHandleTrustPrompt(), "the dialog is still in the way")
	}
	require.Equal(t, []string{"Down"}, injectedKeyNames(*keys),
		"a second movement key would push the cursor back off %q; got %v", claudeTrustYesLabel, *keys)
	require.Empty(t, pane.committedLabels(), "nothing may be confirmed while af cannot see the cursor")

	// The redraw finally lands, inside the window. af has its proof, and
	// confirms — with no second movement key anywhere in the sequence.
	pane.freeze(false)
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(*keys))
	require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels())
	require.Empty(t, errLog.String())
}

// #3587 review round 2, P1. The earlier version of this fix released the hold
// on a TIMER — "the cursor never moved in 3s, so the key must have been
// dropped, re-send it". That inference is not available: a key the agent has
// BUFFERED but not yet processed looks exactly like a key it never received,
// and the two want opposite actions.
//
// Getting it wrong is not a small cost, because this picker WRAPS. Measured on
// claude 2.1.258:
//
//	start:         ❯ No, exit
//	after Down #1: ❯ Yes, I trust this folder
//	after Down #2: ❯ No, exit          <- back to the option that quits
//
// So a duplicate movement key does not clamp harmlessly; it undoes the move,
// and a repaint landing between af's verification and confirmation captures
// would then let Enter reach "No, exit" — #3579 again. af therefore never
// re-sends: it holds, reports, and lets the create fail loudly.
func TestCheckAndHandleTrustPrompt_ClaudeFolderTrustNeverResendsAMovementKey(t *testing.T) {
	advance := setClaudeTrustClock(t)
	_, _, errLog := captureTrustPromptLogs(t)

	// A picker that never acts on a movement key, however many arrive.
	pane := &claudeFolderTrustPane{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}, deaf: 1 << 20}
	session, keys := claudeTrustSession(t, pane)

	require.True(t, session.CheckAndHandleTrustPrompt()) // records when the dialog appeared
	advance(claudeTrustSettleDelay)
	require.True(t, session.CheckAndHandleTrustPrompt()) // settled: af sends its one movement key
	require.Equal(t, []string{"Down"}, injectedKeyNames(*keys), "precondition: af sent one movement key")

	// However long af waits, and however many polls run, it sends nothing more.
	for i := 0; i < 8; i++ {
		advance(claudeTrustHoldReportAfter)
		require.True(t, session.CheckAndHandleTrustPrompt(), "the dialog is still in the way")
	}
	require.Equal(t, []string{"Down"}, injectedKeyNames(*keys),
		"elapsed time is not evidence the key was dropped, and this picker wraps; got %v", *keys)
	require.Empty(t, pane.committedLabels(), "nothing may be confirmed while af cannot see the cursor")

	// The operator is told why the session is stuck — once, not every poll.
	require.Contains(t, errLog.String(), "af will not send another")
	require.Equal(t, 1, strings.Count(errLog.String(), "af will not send another"),
		"the hold is reported once per dialog, not on every tick of the daemon poll")
}

// Claude Code drops keys sent in the instant after it paints this dialog, before
// it is reading input — measured on 2.1.258 at roughly one create in six. Since
// a dropped key can never be safely re-sent (above), the only remedy is not to
// send it too early: af waits for the dialog to settle before it types.
func TestCheckAndHandleTrustPrompt_ClaudeFolderTrustWaitsForTheDialogToSettle(t *testing.T) {
	advance := setClaudeTrustClock(t)
	pane := &claudeFolderTrustPane{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}}
	session, keys := claudeTrustSession(t, pane)

	require.True(t, session.CheckAndHandleTrustPrompt(), "a dialog af has only just seen is still in the way")
	require.Empty(t, injectedKeyNames(*keys),
		"af must not type into a dialog Claude Code has only just painted; got %v", *keys)

	// Still too soon.
	advance(claudeTrustSettleDelay / 2)
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Empty(t, injectedKeyNames(*keys))

	// Settled: now af drives it.
	advance(claudeTrustSettleDelay)
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(*keys))
	require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels())
}

// #3587 review, P2. RestoreWithResult respawns a missing session through Start
// on the SAME TmuxSession. Cross-poll state describes one pane process: a
// movement key pending against the dead pane must not hold its replacement, and
// a refusal already reported for the dead pane must not silence the same
// refusal for the new one.
func TestStart_ResetsClaudeTrustStateAtTheProvenRuntimeBoundary(t *testing.T) {
	session := newTmuxSession(toTmuxName("respawn", ""), ProgramClaude, NewMockPtyFactory(t), cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			for _, arg := range c.Args {
				if arg == "has-session" {
					// Determinate absence: Start proceeds past the existence gate.
					return errors.New("can't find session")
				}
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, errors.New("no server running") },
	})
	session.claudeTrust = claudeTrustState{refusalLogged: true, pendingFrom: claudeTrustNoLabel}

	// Start fails somewhere after the resets — which is fine; the boundary is
	// the name being proven absent, not the launch succeeding.
	_ = session.Start(t.TempDir())

	require.Equal(t, claudeTrustState{}, session.claudeTrust,
		"a new pane process inherits no pending movement and no silenced refusal")
}

// A frame af cannot parse at all is not evidence that a pending movement key
// did nothing either, so it holds rather than refusing and starting over.
func TestCheckAndHandleTrustPrompt_ClaudeFolderTrustHoldsThroughAnUnparseableFrame(t *testing.T) {
	advance := setClaudeTrustClock(t)
	pane := &claudeFolderTrustPane{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}}
	session, keys := claudeTrustSession(t, pane)

	require.True(t, session.CheckAndHandleTrustPrompt())
	advance(claudeTrustSettleDelay)
	pane.freeze(true)
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{"Down"}, injectedKeyNames(*keys))

	// A capture caught mid-repaint: the dialog's text is there, but no row
	// carries the cursor.
	pane.mu.Lock()
	pane.stale = "Quick safety check:\nIs this a project you created or one you trust?\n\n" +
		"  " + claudeTrustNoLabel + "\n  " + claudeTrustYesLabel + "\n\nEnter to confirm · Esc to cancel\n"
	pane.mu.Unlock()
	advance(time.Second)
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{"Down"}, injectedKeyNames(*keys),
		"an unreadable frame must not release the pending movement; got %v", *keys)
}

// pollStaticPane drives CheckAndHandleTrustPrompt over unchanging content for
// several daemon poll intervals, so af's settle delay is crossed and the pane
// is genuinely offered as something af could type into. A single call would
// prove nothing now that af declines to touch a dialog it has only just seen.
func pollStaticPane(t *testing.T, content string, polls int) (handled bool, cmds []string) {
	t.Helper()
	advance := setClaudeTrustClock(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			cmds = append(cmds, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return []byte("0 0 0"), nil
			}
			return []byte(content), nil
		},
	}
	session := newTmuxSession(toTmuxName("quoted", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec)
	for i := 0; i < polls; i++ {
		advance(time.Second)
		handled = session.CheckAndHandleTrustPrompt()
	}
	return handled, cmds
}

// #3587 review, P1. This handler runs on the daemon's continuous poll against
// ARBITRARY agent output, and Down/Up/Enter typed into a working composer
// cannot be taken back. Output that quotes the dialog — including this repo's
// own source and issue text, which is how #1952 and #2638 happened — must
// inject nothing, however long it stays on screen.
func TestCheckAndHandleTrustPrompt_QuotedClaudeFolderTrustDialogInjectsNothing(t *testing.T) {
	dialog := "Quick safety check: Is this a project you created or one you trust?\n\n" +
		"❯ " + claudeTrustNoLabel + "\n  " + claudeTrustYesLabel + "\n\n" +
		"Enter to confirm · Esc to cancel\n"

	for _, tt := range []struct{ name, content string }{
		{
			// The agent is showing the dialog inside its own transcript, with
			// its composer painted below — the shape every live pane has.
			name: "quoted above the agent's composer",
			content: dialog + "\n╭────────────────────────────────────╮\n" +
				"│ > Type your message here           │\n╰────────────────────────────────────╯\n? for shortcuts\n",
		},
		{
			name:    "quoted with trailing prose",
			content: dialog + "\nThat is the dialog af answers at startup.\n",
		},
		{
			name: "the label mentioned in a sentence next to the composer cursor",
			content: "Quick safety check: Is this a project you created or one you trust?\n" +
				"❯ I will select \"" + claudeTrustYesLabel + "\" for you\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "af has not OBSERVED a dialog-free pane, so it may not report one (#3302)")
			require.Empty(t, sentKeystrokes(cmds),
				"no key may be injected into a pane that only quotes the dialog; got %v", cmds)
		})
	}
}

// The same guard from the other side: a pane showing the REAL dialog, polled
// the same way, is driven. Without this, the test above could pass because af
// had stopped answering dialogs entirely.
func TestCheckAndHandleTrustPrompt_RealClaudeFolderTrustDialogIsStillDriven(t *testing.T) {
	pane := claudeFolderTrustPane{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}}
	keys := driveClaudeTrustPane(t, &pane, 6)
	require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels())
	require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(keys))
}
