package tmux

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7271: Claude Code's reworded folder-trust dialog is
// read from the pane's trailing region, the way the legacy, MCP and 2.1.322
// dialogs already are.
//
//   - A dialog af cannot read as rows — wrapped by a narrow pane, or still
//     being painted — holds: the check reports true and sends nothing. Before,
//     a wrapped question matched nothing, the check reported the pane clear,
//     and the create typed the user's prompt into the modal (#3579 class).
//   - The dialog's text with something that is not the dialog painted below it
//     — the agent's composer, more output — is no dialog: the check reports
//     false and sends nothing, so the create delivers the prompt to the agent.
//     That replaces #3587's hold-on-quote.
//   - Every escape capture-pane -e emits is stripped before matching,
//     including OSC 8 hyperlinks.
//
// No capture of the folder-trust dialog exists in testdata: taking one means
// running a real Claude Code against a config whose trusted-project list it
// then writes. So the dialog below is SYNTHETIC. Its bytes are modelled on the
// measured 2.1.322 MCP-server captures (testdata/claude_2_1_322_mcp_server_
// dialog*.ansi): a coloured top rule, a bold title styled word by word, body
// text with an OSC 8 link, option rows indented four with "  ❯ " on the
// selected one, only the selected row styled, and an italic footer styled word
// by word — with each row's closing SGR re-emitted at the start of the next
// painted row, as tmux serializes it. Its rows wrap the way the measured
// renderer wraps them (TestSyntheticClaudeWrapMatchesMeasuredCaptures pins
// that against the 40-, 50- and 80-column captures). The wording other than
// the question, the option labels and the footer is illustrative.

const (
	syntheticFolderTrustTitle     = "Accessing workspace:"
	syntheticFolderTrustPath      = "/home/dev/af-trees/wt"
	syntheticFolderTrustQuestion  = "Is this a project you created or one you trust?"
	syntheticFolderTrustParagraph = "Quick safety check: " + syntheticFolderTrustQuestion +
		" (Like your own code, a well-known open source project, or work from your team)." +
		" If not, take a moment to review what's in this folder first."
	syntheticFolderTrustNote   = "Claude Code'll be able to read, edit, and execute files here."
	syntheticFolderTrustGuide  = "Security guide"
	syntheticFolderTrustFooter = "Enter to confirm · Esc to cancel"
)

// syntheticFolderTrustFrame is one synthetic capture and where its rows are.
type syntheticFolderTrustFrame struct {
	text string
	// cursorRow is the line index of the ❯ row; footerRow is the line index of
	// the footer's first row.
	cursorRow, footerRow int
	// readable is whether every row af reads by — the question, each option,
	// the footer — fits on one pane row at this width.
	readable bool
}

// syntheticWrap breaks text into rows of at most width cells at spaces, the
// greedy word wrap the measured captures show.
func syntheticWrap(text string, width int) [][]string {
	var rows [][]string
	var row []string
	cells := 0
	for _, word := range strings.Fields(text) {
		n := utf8.RuneCountInString(word)
		if len(row) > 0 && cells+1+n > width {
			rows, row, cells = append(rows, row), nil, 0
		}
		if len(row) > 0 {
			cells++
		}
		row, cells = append(row, word), cells+n
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// syntheticOSC8 wraps word in an OSC 8 hyperlink terminated by term, the form
// testdata/claude_2_1_322_mcp_server_dialog.ansi carries for its link.
func syntheticOSC8(word, term string) string {
	return "\x1b]8;id=af7271;https://code.claude.com/docs/en/security" + term + word + "\x1b]8;;" + term
}

// renderSyntheticClaudeFolderTrust paints the synthetic dialog at a pane width,
// with options in render order and selected owning the cursor. link, when set,
// is the OSC 8 terminator (ST or BEL) for a hyperlink around one word of the
// question and one of the affirmative label.
func renderSyntheticClaudeFolderTrust(width int, options []string, selected int, link string) syntheticFolderTrustFrame {
	frame := syntheticFolderTrustFrame{cursorRow: -1, readable: true}
	var lines []string
	carry := ""
	emit := func(line string) {
		if line != "" {
			line, carry = carry+line, ""
		}
		lines = append(lines, line)
	}
	// styled opens every word with open and closes all but the last with
	// close: tmux re-emits that last close at the start of the next painted
	// row (carry), exactly as the measured captures show.
	styled := func(words []string, open, close string) string {
		out := make([]string, len(words))
		for i, word := range words {
			out[i] = open + word
			if i < len(words)-1 {
				out[i] += close
			}
		}
		carry = close
		return strings.Join(out, " ")
	}
	linked := func(words []string, target string) []string {
		out := append([]string(nil), words...)
		if link == "" {
			return out
		}
		for i, word := range out {
			if word == target {
				out[i] = syntheticOSC8(word, link)
				return out
			}
		}
		return out
	}

	emit("")
	emit("\x1b[38;5;220m" + strings.Repeat("─", width))
	carry = "\x1b[39m"
	for _, row := range syntheticWrap(syntheticFolderTrustTitle, width-4) {
		emit("  " + styled(row, "\x1b[1m\x1b[38;5;220m", "\x1b[0m"))
	}
	emit("")
	for _, row := range syntheticWrap(syntheticFolderTrustPath, width-4) {
		emit("  " + strings.Join(row, " "))
	}
	emit("")
	questionOnOneRow := false
	for _, row := range syntheticWrap(syntheticFolderTrustParagraph, width-4) {
		questionOnOneRow = questionOnOneRow || strings.Contains(strings.Join(row, " "), syntheticFolderTrustQuestion)
		emit("  " + strings.Join(linked(row, "created"), " "))
	}
	frame.readable = frame.readable && questionOnOneRow
	emit("")
	for _, row := range syntheticWrap(syntheticFolderTrustNote, width-4) {
		emit("  " + strings.Join(row, " "))
	}
	emit("")
	for _, row := range syntheticWrap(syntheticFolderTrustGuide, width-4) {
		emit("  " + syntheticOSC8(strings.Join(row, " "), "\x1b\\"))
	}
	emit("")
	for idx, option := range options {
		rows := syntheticWrap(option, width-5)
		frame.readable = frame.readable && len(rows) == 1
		for r, row := range rows {
			if option == claudeTrustAffirmativeLabel {
				row = linked(row, "trust")
			}
			switch {
			case idx != selected:
				emit("    " + strings.Join(row, " "))
			case r == 0:
				frame.cursorRow = len(lines)
				emit("  \x1b[38;5;153m❯\x1b[39m " + styled(row, "\x1b[38;5;153m", "\x1b[39m"))
			default:
				emit("    " + styled(row, "\x1b[38;5;153m", "\x1b[39m"))
			}
		}
	}
	emit("")
	footer := syntheticWrap(syntheticFolderTrustFooter, width-4)
	frame.readable = frame.readable && len(footer) == 1
	frame.footerRow = len(lines)
	for _, row := range footer {
		emit("  " + styled(row, "\x1b[3m\x1b[38;5;246m", "\x1b[0m"))
	}
	// The measured captures end in the pane's blank rows, with no reset.
	for len(lines) < 24 {
		lines = append(lines, "")
	}
	frame.text = strings.Join(lines, "\n") + "\n"
	return frame
}

// syntheticTerminalEscape strips what capture-pane -e emits (SGR and OSC 8),
// for checking the model against the measured captures. It is the test's own
// copy, so it compiles against any version of the code under test.
var syntheticTerminalEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b\n]*(?:\x07|\x1b\\)`)

// The synthetic model is only evidence if it wraps where Claude Code does. The
// measured 2.1.322 captures at 40, 50 and 80 columns show the renderer's wrap:
// body rows at the width less four, option labels at the width less five. This
// pins the model's wrap against every body and option row in them.
func TestSyntheticClaudeWrapMatchesMeasuredCaptures(t *testing.T) {
	const body = "MCP servers may execute code or access system resources. All tool calls require approval. Learn more in the MCP documentation."
	for _, tt := range []struct {
		fixture string
		width   int
	}{
		{mcpServerDialogFixture, 80},
		{mcpServerDialog50Fixture, 50},
		{mcpServerDialog40Fixture, 40},
	} {
		t.Run(fmt.Sprintf("%d columns", tt.width), func(t *testing.T) {
			var visible []string
			for _, line := range strings.Split(syntheticTerminalEscape.ReplaceAllString(readTrustFixture(t, tt.fixture), ""), "\n") {
				visible = append(visible, strings.TrimRight(line, " "))
			}
			var want []string
			for _, row := range syntheticWrap(body, tt.width-4) {
				want = append(want, "  "+strings.Join(row, " "))
			}
			for _, option := range claudeMCPServerOptionLabels {
				for r, row := range syntheticWrap(option, tt.width-5) {
					prefix := "    "
					if option == claudeMCPServerDeclineLabel && r == 0 {
						prefix = "  ❯ "
					}
					want = append(want, prefix+strings.Join(row, " "))
				}
			}
			for _, row := range want {
				require.Contains(t, visible, row,
					"the synthetic wrap must paint this row the way the measured %d-column capture does", tt.width)
			}
		})
	}
}

// Every width af can be given, from a phone client's narrow pane to a wide
// terminal: a dialog whose rows fit is answered by label, and one whose rows
// wrap is held — reported in the way, sent nothing, and its refusal logged once.
// Before #7271, every width that wraps the question failed open: the check
// reported the pane clear and the create typed the prompt into the modal.
func TestCheckAndHandleTrustPrompt_FolderTrustAtEveryPaneWidthIsAnsweredOrHeld(t *testing.T) {
	for width := 24; width <= 120; width++ {
		for _, tt := range []struct {
			name     string
			selected int
			wantKeys []string
		}{
			{"No preselected", 0, []string{"Down", "Enter"}},
			{"Yes selected", 1, []string{"Enter"}},
		} {
			options := []string{claudeTrustNoLabel, claudeTrustYesLabel}
			frame := renderSyntheticClaudeFolderTrust(width, options, tt.selected, "")
			t.Run(fmt.Sprintf("%d columns, %s", width, tt.name), func(t *testing.T) {
				pane := &claudeFolderTrustPane{options: options, selected: tt.selected, width: width}
				if frame.readable {
					keys := driveClaudeTrustPane(t, pane, 6)
					require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels(),
						"a dialog whose rows fit must be answered by label; keystrokes were %v", keys)
					require.Equal(t, tt.wantKeys, injectedKeyNames(keys))
					return
				}
				requireFolderTrustHeld(t, pane, 6)
			})
		}
	}
}

// requireFolderTrustHeld polls a dialog af cannot read and requires every poll
// to report it in the way, with no key sent and the refusal logged once.
func requireFolderTrustHeld(t *testing.T, pane *claudeFolderTrustPane, polls int) {
	t.Helper()
	advance := setClaudeTrustClock(t)
	_, _, errLog := captureTrustPromptLogs(t)
	session, keys := claudeTrustSession(t, pane)
	for i := 0; i < polls; i++ {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt(),
			"poll %d: a folder-trust dialog af cannot read is still in the way; reporting the pane clear types the prompt into it", i+1)
	}
	require.Empty(t, *keys, "af must send nothing to a dialog it cannot read as rows")
	require.Empty(t, pane.committedLabels())
	require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer the "+claudeFolderTrustDialogName),
		"the refusal is logged once per dialog, not on every poll:\n%s", errLog.String())
}

// The issue's harness: a 46-column pane, where Claude Code wraps the question.
// It holds while narrow, and once the pane is wide again — a phone client
// detaches, the next client resizes it — af answers it by label.
func TestCheckAndHandleTrustPrompt_NarrowFolderTrustHoldsThenIsAnsweredOnceItFits(t *testing.T) {
	options := []string{claudeTrustNoLabel, claudeTrustYesLabel}
	require.False(t, renderSyntheticClaudeFolderTrust(46, options, 0, "").readable,
		"precondition: 46 columns wraps the dialog")

	advance := setClaudeTrustClock(t)
	_, _, errLog := captureTrustPromptLogs(t)
	pane := &claudeFolderTrustPane{options: options, width: 46}
	session, keys := claudeTrustSession(t, pane)
	for i := 0; i < 3; i++ {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt(), "poll %d: the wrapped dialog is in the way", i+1)
	}
	require.Empty(t, *keys)
	require.Contains(t, errLog.String(), "refusing to answer the "+claudeFolderTrustDialogName)

	pane.mu.Lock()
	pane.width = 80
	pane.mu.Unlock()
	for i := 0; i < 6 && len(pane.committedLabels()) == 0; i++ {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt())
	}
	require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels(),
		"the held dialog is answered once it reads as rows; keystrokes were %v", *keys)
	require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(*keys))
}

// Claude Code paints the dialog top-down, so a capture can catch any prefix of
// it, the last row possibly cut short. Readiness waits for a ❯, so the prefixes
// that matter are those through the cursor row and on. Each one holds and sends
// nothing — at a width that fits and at one that wraps. Before #7271 the
// prefix ending at "❯ No, exit" matched no clause and the pane read clear.
func TestCheckAndHandleTrustPrompt_FolderTrustProperPrefixHolds(t *testing.T) {
	for _, width := range []int{46, 80} {
		for _, selected := range []int{0, 1} {
			frame := renderSyntheticClaudeFolderTrust(width, []string{claudeTrustNoLabel, claudeTrustYesLabel}, selected, "")
			lines := strings.Split(frame.text, "\n")
			prefixes := map[string]string{
				"cut just after the ❯ glyph": strings.Join(lines[:frame.cursorRow], "\n") + "\n  \x1b[38;5;153m❯",
				"cut inside the footer": strings.Join(lines[:frame.footerRow], "\n") +
					"\n  \x1b[3m\x1b[38;5;246mEnter\x1b[0m \x1b[3m\x1b[38;5;246mto\x1b[0m \x1b[3m\x1b[38;5;246mcon",
			}
			for k := frame.cursorRow + 1; k <= frame.footerRow; k++ {
				prefixes[fmt.Sprintf("rows 0-%d", k-1)] = strings.Join(lines[:k], "\n") + "\n"
			}
			for name, content := range prefixes {
				t.Run(fmt.Sprintf("%d columns, cursor on %d, %s", width, selected, name), func(t *testing.T) {
					handled, cmds := pollStaticPane(t, content, 4)
					require.True(t, handled, "a dialog still being painted is in the way:\n%s", content)
					require.Empty(t, sentKeystrokes(cmds), "nothing may be sent to a dialog that is not fully painted")
				})
			}
		}
	}
}

// The dialog's text with something that is not the dialog painted below it is
// an observed dialog-free pane: the agent is showing the dialog in its own
// transcript — a diff, a grep of this repo, a review — above its composer. The
// check reports no dialog, so the create delivers the prompt to the agent, and
// it sends no trust-handler key. #3587 held such a pane instead (refusing to
// answer, 20 times over, until the create failed); the legacy, MCP and 2.1.322
// clauses already report it clear. The composer below is the measured one
// (testdata/claude_2_1_322_composer_after_mcp_decline.ansi).
func TestCheckAndHandleTrustPrompt_FolderTrustQuotedAboveTheComposerIsNoDialog(t *testing.T) {
	composer := readTrustFixture(t, mcpServerComposerFixture)
	for _, width := range []int{46, 80, 120} {
		frame := renderSyntheticClaudeFolderTrust(width, []string{claudeTrustNoLabel, claudeTrustYesLabel}, 0, "")
		for name, content := range map[string]string{
			"the agent's composer below": strings.TrimRight(frame.text, "\n") + "\n" + composer,
			"more output below":          strings.TrimRight(frame.text, "\n") + "\n\n  That is the dialog af answers at startup.\n",
		} {
			t.Run(fmt.Sprintf("%d columns, %s", width, name), func(t *testing.T) {
				handled, cmds := pollStaticPane(t, content, 4)
				require.False(t, handled, "a quoted dialog with the agent painted below it is not in the way")
				require.Empty(t, sentKeystrokes(cmds), "no trust-handler key may reach a pane that only quotes the dialog; got %v", cmds)
			})
		}
	}
}

// capture-pane -e keeps OSC 8 hyperlinks, terminated by ST or BEL (#7271 R3).
// A linked word inside the question or the affirmative label must not hide the
// dialog: before #7271 only CSI was stripped, the phrases did not match, and
// the check reported the pane clear.
func TestCheckAndHandleTrustPrompt_FolderTrustWithLinkedWordsIsAnswered(t *testing.T) {
	for name, term := range map[string]string{"ST": "\x1b\\", "BEL": "\a"} {
		t.Run(name, func(t *testing.T) {
			pane := &claudeFolderTrustPane{
				options: []string{claudeTrustNoLabel, claudeTrustYesLabel},
				width:   80,
				link:    term,
			}
			require.Contains(t, pane.render(), "\x1b]8;", "precondition: the frame carries OSC 8 links")
			keys := driveClaudeTrustPane(t, pane, 6)
			require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels(), "keystrokes were %v", keys)
			require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(keys))
		})
	}
}
