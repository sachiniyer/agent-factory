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
	// the footer — fits on one pane row at this width, and is on screen.
	readable bool
	// contentRows is how many rows the dialog paints, before the pane's blank
	// rows below it.
	contentRows int
}

// syntheticPane fits a frame into a pane height rows tall, the way a terminal
// shows output taller than itself: the dialog's last rows stay at the bottom
// and its top scrolls off (#7280). A shorter dialog keeps its blank rows below.
// readable then also requires the question to be on screen.
func syntheticPane(frame syntheticFolderTrustFrame, height int) syntheticFolderTrustFrame {
	lines := strings.Split(strings.TrimSuffix(frame.text, "\n"), "\n")[:frame.contentRows]
	if over := len(lines) - height; over > 0 {
		lines = lines[over:]
		frame.cursorRow -= over
		frame.footerRow -= over
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	frame.text = strings.Join(lines, "\n") + "\n"
	var visible []string
	for _, line := range lines {
		visible = append(visible, strings.TrimSpace(syntheticTerminalEscape.ReplaceAllString(line, "")))
	}
	frame.readable = frame.readable && strings.Contains(strings.Join(visible, " "), syntheticFolderTrustQuestion)
	return frame
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
	// emitStyled paints prefix and then words, each opened with open and all
	// but the last closed with close: tmux re-emits that last close at the
	// start of the NEXT painted row (carry), as the measured captures show.
	emitStyled := func(prefix string, words []string, open, close string) {
		out := make([]string, len(words))
		for i, word := range words {
			out[i] = open + word
			if i < len(words)-1 {
				out[i] += close
			}
		}
		emit(prefix + strings.Join(out, " "))
		carry = close
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
		emitStyled("  ", row, "\x1b[1m\x1b[38;5;220m", "\x1b[0m")
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
				emitStyled("  \x1b[38;5;153m❯\x1b[39m ", row, "\x1b[38;5;153m", "\x1b[39m")
			default:
				emitStyled("    ", row, "\x1b[38;5;153m", "\x1b[39m")
			}
		}
	}
	emit("")
	footer := syntheticWrap(syntheticFolderTrustFooter, width-4)
	frame.readable = frame.readable && len(footer) == 1
	frame.footerRow = len(lines)
	for _, row := range footer {
		emitStyled("  ", row, "\x1b[3m\x1b[38;5;246m", "\x1b[0m")
	}
	frame.contentRows = len(lines)
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

// The model carries each styled row's last close onto the start of the NEXT
// painted row, as tmux serializes it, so the stripped text is not the only
// thing the model gets right (#7280, item 3). The measured 2.1.322 capture
// opens its title row with the rule's close, its first body row with the
// title's, and its footer with the ❯ row's.
func TestSyntheticClaudeCarriedSGRMatchesMeasuredCapture(t *testing.T) {
	opening := regexp.MustCompile(`^(?:\x1b\[[0-?]*[ -/]*[@-~])*`)
	footerOf := func(lines []string) int {
		for i, line := range lines {
			if strings.Contains(syntheticTerminalEscape.ReplaceAllString(line, ""), "Enter to confirm") {
				return i
			}
		}
		return -1
	}
	measured := strings.Split(readTrustFixture(t, mcpServerDialogFixture), "\n")
	frame := renderSyntheticClaudeFolderTrust(80, []string{claudeTrustNoLabel, claudeTrustYesLabel}, 1, "")
	synthetic := strings.Split(frame.text, "\n")
	for _, tt := range []struct {
		name string
		row  func([]string) int
		want string
	}{
		{"the rule's close opens the title row", func([]string) int { return 2 }, "\x1b[39m"},
		{"the title's close opens the next painted row", func([]string) int { return 4 }, "\x1b[0m"},
		{"the ❯ row's close opens the footer", footerOf, "\x1b[39m"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, opening.FindString(measured[tt.row(measured)]), "precondition: the measured capture")
			require.Equal(t, tt.want, opening.FindString(synthetic[tt.row(synthetic)]), "the synthetic model")
		})
	}
}

// Frames from #7280 that read clear where master held, or held where the
// agent's composer shows a working agent. Every one sends no key: the ones in
// the way hold, and only a composer below the dialog's options reads clear.
func TestCheckAndHandleTrustPrompt_FolderTrustOddFrames(t *testing.T) {
	frame := renderSyntheticClaudeFolderTrust(80, []string{claudeTrustNoLabel, claudeTrustYesLabel}, 0, "")
	lines := strings.Split(frame.text, "\n")
	through := func(row int) string { return strings.Join(lines[:row], "\n") + "\n" }
	from := func(row int) string { return strings.Join(lines[row:], "\n") }
	dialog := strings.TrimRight(frame.text, "\n") + "\n"
	const composer = "\x1b[38;5;244m────────────────────────────────────────\n\x1b[39m❯\u00a0\x1b[7m \n\x1b[0m\x1b[38;5;244m────────────────────────────────────────\n"
	for _, tt := range []struct {
		name, content string
		inTheWay      bool
	}{
		// Item 1: af's Down caught mid-repaint, the footer still painted.
		{"the affirmative row erased", through(frame.cursorRow+1) + "\n" + from(frame.cursorRow+2), true},
		{"the ❯ row half-written", through(frame.cursorRow) + "    No, exit\n  \x1b[38;5;153m❯\x1b[39m \x1b[38;5;153mYes,\x1b[39m \x1b[38;5;153mI\x1b[39m \x1b[38;5;153mtr\n" + from(frame.cursorRow+2), true},
		// Item A: a ❯ above the question is no composer below the dialog.
		{"a composer above the dialog", composer + "  ? for shortcuts\n" + dialog, true},
		{"a composer above and below the dialog", composer + "  ? for shortcuts\n" + dialog + composer + "  ? for shortcuts\n", false},
		// Item B: a ❯ row cut short of an option label is the dialog's.
		{"a ❯ row cut short of an option label", through(frame.footerRow) + "  ❯ No, ex\n\n" + from(frame.footerRow), true},
		{"an empty ❯ row is still the composer", through(frame.footerRow) + "  ❯\n\n" + from(frame.footerRow), false},
		// Item C: a ❯ between two rules is the composer, last row or not.
		{"a quote ending in the composer between its rules", dialog + composer, false},
		// Item D: a ❯ row is no footer, so the rule above it is no closing frame.
		{"a ❯ draft reading like the footer, between rules", through(frame.footerRow) + "────────\n❯ Enter to confirm · Esc to cancel\n────────\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.Equal(t, tt.inTheWay, handled, "\n%s", syntheticTerminalEscape.ReplaceAllString(tt.content, ""))
			require.Empty(t, sentKeystrokes(cmds), "no key may be sent; got %v", cmds)
		})
	}
}

// Every width af can be given, from a phone client's narrow pane to a wide
// terminal, in a pane 24 rows tall: a dialog whose rows fit is answered by
// label, and one whose rows wrap — or whose question a narrow pane pushes off
// the top (#7280) — is held: reported in the way, sent nothing, and its refusal
// logged once. Before #7271, every width that wraps the question failed open:
// the check reported the pane clear and the create typed the prompt into the
// modal.
func TestCheckAndHandleTrustPrompt_FolderTrustAtEveryPaneWidthIsAnsweredOrHeld(t *testing.T) {
	requireFolderTrustAnsweredOrHeld(t, func(yield func(width, height int) bool) {
		for width := 24; width <= 120; width++ {
			if !yield(width, 24) {
				return
			}
		}
	})
}

// A pane too short for the dialog scrolls its question off the top — a phone
// in landscape with its keyboard up is about 11 rows (#7280). The dialog still
// owns the pane: its own option block and footer are the last content, which a
// quote never has, since its composer is below. So it holds, and its refusal
// is logged once; a pane tall enough to show the question is answered.
func TestCheckAndHandleTrustPrompt_FolderTrustInAShortPaneIsAnsweredOrHeld(t *testing.T) {
	requireFolderTrustAnsweredOrHeld(t, func(yield func(width, height int) bool) {
		for _, width := range []int{80, 120} {
			for height := 5; height <= 24; height++ {
				if !yield(width, height) {
					return
				}
			}
		}
	})
}

// requireFolderTrustAnsweredOrHeld drives the synthetic dialog at each pane
// size: answered by label when every row af reads by is on screen whole, held
// otherwise, and never reported clear.
func requireFolderTrustAnsweredOrHeld(t *testing.T, sizes func(yield func(width, height int) bool)) {
	t.Helper()
	for width, height := range sizes {
		for _, tt := range []struct {
			name     string
			selected int
			wantKeys []string
		}{
			{"No preselected", 0, []string{"Down", "Enter"}},
			{"Yes selected", 1, []string{"Enter"}},
		} {
			options := []string{claudeTrustNoLabel, claudeTrustYesLabel}
			frame := syntheticPane(renderSyntheticClaudeFolderTrust(width, options, tt.selected, ""), height)
			t.Run(fmt.Sprintf("%dx%d, %s", width, height, tt.name), func(t *testing.T) {
				pane := &claudeFolderTrustPane{options: options, selected: tt.selected, width: width, height: height}
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

// The dialog's text with the agent's composer painted below it is an observed
// dialog-free pane: the agent is showing the dialog in its own transcript — a
// diff, a grep of this repo, a review — above its composer, or above a
// permission prompt, which takes the composer's place. The check reports no
// dialog, so the create delivers the prompt to the agent, and it sends no
// trust-handler key. #3587 held such a pane instead (refusing to answer, 20
// times over, until the create failed); the legacy, MCP and 2.1.322 clauses
// already report it clear. The composer below is the measured one
// (testdata/claude_2_1_322_composer_after_mcp_decline.ansi).
func TestCheckAndHandleTrustPrompt_FolderTrustQuotedAboveTheComposerIsNoDialog(t *testing.T) {
	composer := readTrustFixture(t, mcpServerComposerFixture)
	for _, width := range []int{46, 80, 120} {
		frame := strings.TrimRight(renderSyntheticClaudeFolderTrust(width, []string{claudeTrustNoLabel, claudeTrustYesLabel}, 0, "").text, "\n")
		for name, content := range map[string]string{
			"the agent's composer below": frame + "\n" + composer,
			"the boxed composer below": frame + "\n\n╭────────────────────────────────────╮\n" +
				"│ > Type your message here           │\n╰────────────────────────────────────╯\n  ? for shortcuts\n",
			"a permission prompt below":                       frame + "\n" + syntheticPermissionPrompt,
			"a coloured grep of this repo above the composer": syntheticColouredGrep + composer,
		} {
			t.Run(fmt.Sprintf("%d columns, %s", width, name), func(t *testing.T) {
				handled, cmds := pollStaticPane(t, content, 4)
				require.False(t, handled, "a quoted dialog with the agent painted below it is not in the way")
				require.Empty(t, sentKeystrokes(cmds), "no trust-handler key may reach a pane that only quotes the dialog; got %v", cmds)
			})
		}
	}
}

// syntheticPermissionPrompt is Claude Code's tool-permission prompt, which a
// working agent paints in its composer's place. Synthetic: its ❯ is the mark
// of a working agent the folder-trust verdict needs.
const syntheticPermissionPrompt = "\x1b[38;5;174m" + "────────────────────────────────────────" + "\n" +
	"\x1b[39m Bash command\n\n   rg -n \"trust this folder\" session/tmux\n\n" +
	" Do you want to proceed?\n" +
	" \x1b[38;5;153m❯\x1b[39m \x1b[38;5;153m1.\x1b[39m \x1b[38;5;153mYes\n" +
	"\x1b[39m   2. Yes, and don't ask again for rg commands in /home/dev/af-trees/wt\n" +
	"   3. No, and tell Claude what to do differently (esc)\n"

// syntheticColouredGrep is `rg --color=always` over this package, quoting the
// dialog's strings with each match highlighted (synthetic, in rg's escapes).
const syntheticColouredGrep = "\x1b[35msession/tmux/claude_folder_trust.go\x1b[0m:\x1b[32m12\x1b[0m:const claudeFolderTrustQuestion = \"" +
	"\x1b[1m\x1b[31mIs this a project you created or one you trust\x1b[0m\"\n" +
	"\x1b[35msession/tmux/claude_trust.go\x1b[0m:\x1b[32m17\x1b[0m:const claudeTrustAffirmativeLabel = \"\x1b[1m\x1b[31mYes, I trust this folder\x1b[0m\"\n" +
	"\x1b[35msession/tmux/claude_folder_trust.go\x1b[0m:\x1b[32m21\x1b[0m:const claudeFolderTrustFooter = \"\x1b[1m\x1b[31mEnter to confirm · Esc to cancel\x1b[0m\"\n"

// A row the dialog does not paint, in or below its footer slot, is not by
// itself a working agent: the live dialog can show one (#7275 review, R1).
// Claude Code's footer element reads "Press Ctrl-C again to exit" for a moment
// after one Ctrl-C from an attached client. A notice, a tip, a status row or a
// stray stderr line can land there too, and a resize can leave a fragment.
// Each one holds without a key; only the composer below reads as no dialog.
// Before the review's fix every one read clear, and the create typed its prompt
// into the live modal.
func TestCheckAndHandleTrustPrompt_FolderTrustWithAnUnknownRowInItsFooterSlotHolds(t *testing.T) {
	for _, width := range []int{46, 80} {
		frame := renderSyntheticClaudeFolderTrust(width, []string{claudeTrustNoLabel, claudeTrustYesLabel}, 0, "")
		lines := strings.Split(frame.text, "\n")
		through := func(row int) string { return strings.Join(lines[:row], "\n") + "\n" }
		dialog := strings.TrimRight(frame.text, "\n") + "\n"
		for name, content := range map[string]string{
			"the Ctrl-C footer swap": through(frame.footerRow) +
				"  \x1b[3m\x1b[38;5;246mPress\x1b[0m \x1b[3m\x1b[38;5;246mCtrl-C\x1b[0m \x1b[3m\x1b[38;5;246magain\x1b[0m \x1b[3m\x1b[38;5;246mto\x1b[0m \x1b[3m\x1b[38;5;246mexit\n",
			"a tip row after a blank below the footer": dialog + "\n  ※ Tip: run /terminal-setup to enable Shift+Enter\n",
			"a tip row after a blank above the footer": through(frame.footerRow) + "  ※ Tip: press Esc twice to rewind\n\n" +
				strings.Join(lines[frame.footerRow:], "\n"),
			"a notice straight below the footer": dialog + "  Update available: run claude update\n",
			"a right-aligned status row":         dialog + "\n" + strings.Repeat(" ", width-20) + "◐ medium · /effort\n",
			"a stray stderr line":                dialog + "(node:4242) Warning: an unhandled stderr line\n",
			"a leftover footer fragment":         dialog + "  cancel\n",
			"more output below":                  dialog + "\n  That is the dialog af answers at startup.\n",
		} {
			t.Run(fmt.Sprintf("%d columns, %s", width, name), func(t *testing.T) {
				handled, cmds := pollStaticPane(t, content, 4)
				require.True(t, handled, "a row the dialog does not paint is not the agent's composer; the dialog is still in the way:\n%s", content)
				require.Empty(t, sentKeystrokes(cmds), "nothing may be sent to a dialog af cannot read; got %v", cmds)
			})
		}
	}
}

// The motivating frame, as a sequence: the dialog is up, a Ctrl-C from an
// attached client swaps its footer for about 0.8 s, and the footer comes back.
// Every poll holds, no key reaches the swapped frame, and once the footer is
// back af answers by label.
func TestCheckAndHandleTrustPrompt_FolderTrustHoldsThroughTheCtrlCFooterSwap(t *testing.T) {
	advance := setClaudeTrustClock(t)
	pane := &claudeFolderTrustPane{options: []string{claudeTrustNoLabel, claudeTrustYesLabel}}
	session, keys := claudeTrustSession(t, pane)

	require.True(t, session.CheckAndHandleTrustPrompt(), "the dialog is in the way")
	pane.mu.Lock()
	pane.footer = "Press Ctrl-C again to exit"
	pane.mu.Unlock()
	for i := 0; i < 3; i++ {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt(), "poll %d: the swapped footer is still the live dialog", i+1)
	}
	require.Empty(t, *keys, "no key may reach the dialog while its footer is swapped")

	pane.mu.Lock()
	pane.footer = ""
	pane.mu.Unlock()
	for i := 0; i < 6 && len(pane.committedLabels()) == 0; i++ {
		advance(time.Second)
		require.True(t, session.CheckAndHandleTrustPrompt())
	}
	require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels(), "keystrokes were %v", *keys)
	require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(*keys))
}

// A frame rule inside the region (#7275 review, R2). The dialog's own closing
// frame, directly above its footer, is the dialog: the boxed layout of the
// repo's legacy fixtures, with the footer under the box, is answered by label.
// A rule anywhere else holds, since a rule alone does not show a working agent.
func TestCheckAndHandleTrustPrompt_FolderTrustFrameRules(t *testing.T) {
	t.Run("a closing frame directly above the footer is answered", func(t *testing.T) {
		pane := &claudeFolderTrustPane{
			options:          []string{claudeTrustNoLabel, claudeTrustYesLabel},
			boxed:            true,
			footerBelowFrame: true,
			ordinals:         true,
		}
		require.Contains(t, pane.render(), "╯\n   Enter to confirm", "precondition: the box closes above the footer")
		keys := driveClaudeTrustPane(t, pane, 6)
		require.Equal(t, []string{claudeTrustYesLabel}, pane.committedLabels(), "keystrokes were %v", keys)
		require.Equal(t, []string{"Down", "Enter"}, injectedKeyNames(keys))
	})
	t.Run("a separator between the body and the options holds", func(t *testing.T) {
		content := "Quick safety check:\nIs this a project you created or one you trust?\n" +
			"────────────────────────────────────────\n" +
			"❯ No, exit\n  Yes, I trust this folder\n\nEnter to confirm · Esc to cancel\n"
		handled, cmds := pollStaticPane(t, content, 4)
		require.True(t, handled, "a frame rule is not the agent's composer; the dialog is still in the way")
		require.Empty(t, sentKeystrokes(cmds))
	})
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
