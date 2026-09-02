package tmux

import (
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
	var keys []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "send-keys") {
				return nil
			}
			keys = append(keys, joined)
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
	session := newTmuxSession(toTmuxName("trust", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec)
	for i := 0; i < polls; i++ {
		if !session.CheckAndHandleTrustPrompt() {
			break
		}
		if len(pane.committedLabels()) > 0 {
			break
		}
	}
	return keys
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
			name: "the cursor and the label are not one picker",
			content: "❯ Resume the previous conversation\n\nsome output\n\n" +
				claudeTrustYesLabel + " (quoted from the docs)\n",
			reason: "not two options of one picker",
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
	now := time.Now()
	previousNow, previousSleep := claudeTrustNow, claudeTrustSleep
	t.Cleanup(func() { claudeTrustNow, claudeTrustSleep = previousNow, previousSleep })
	claudeTrustNow = func() time.Time { return now }
	claudeTrustSleep = func(d time.Duration) { now = now.Add(d) }

	_, _, errLog := captureTrustPromptLogs(t)
	pane := claudeFolderTrustPane{
		options:     []string{claudeTrustNoLabel, claudeTrustYesLabel},
		staleFrames: 1 << 20, // the redraw never arrives
	}
	keys := driveClaudeTrustPane(t, &pane, 1)

	require.Empty(t, pane.committedLabels(), "af must not confirm a selection it could not verify")
	require.Equal(t, []string{"Down"}, injectedKeyNames(keys),
		"exactly one movement key, and no Enter behind it; got %v", keys)
	require.Empty(t, errLog.String(), "an unproven repaint is a retry, not an error")
}
