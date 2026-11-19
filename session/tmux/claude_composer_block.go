package tmux

import (
	"strings"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/log"
)

// claudeScreenOf is one capture as every Claude launch-gate clause reads it:
// its visible text (visibleTextOf), with a whole composer drawn last on it read
// as the empty composer of its kind (claudeComposersCanonical, #7464).
func claudeScreenOf(capture string) visibleText {
	return claudeComposersCanonical(visibleTextOf(capture))
}

// claudeComposersCanonical returns screen with the composer drawn last on it,
// when it is whole, replaced by the empty composer of its kind: its opening and
// closing lines as painted, and between them one prompt row, the ❯ prompt
// between plain rules or the "> " prompt inside a box. Every other line is
// unchanged.
//
// Claude Code paints its launch dialogs in place of the composer, never above a
// live one, so the rows of the draft of a composer drawn last are never a
// dialog's. But each clause read a composer its own way: some knew only the ❯
// prompt, none read a "!" (bash) or "#" (memory) prompt as one, and only the old
// MCP modal's readers read a draft's rows as the composer's. So the mode a
// composer was in, and what its draft said, changed what it explained. A "!"
// composer under a quoted modal held where its ❯ twin cleared, and a draft that
// held a whole dialog was answered, an Enter typed into the draft (#7464). Read
// once here, a composer drawn last in any mode, with any draft, is to every
// clause what the empty composer of its kind is. Each clause keeps its own
// reading of the rows above it and of its status rows below.
//
// Drawn last means that below the composer's closing line there is nothing but
// the one status row a composer paints straight under itself, and blank rows
// (claudeComposerDrawnLast). Anything more below it, such as a dialog's rows,
// was painted after it, and then the composer is read as painted: a composer
// above a live dialog is untouched, and what its draft holds stays evidence
// about the rows below (#7412 review, R3 and R6). A block is whole only as Claude Code draws
// one (claudeComposerBlockAt). A prompt row alone, or an opening rule and a
// prompt row with nothing closing them, is no block and stays as painted,
// because that is how an earlier agent's composer looks with a live dialog
// painted under it.
func claudeComposersCanonical(screen visibleText) visibleText {
	if !strings.ContainsAny(string(screen), "─") {
		return screen
	}
	lines := screen.lines()
	open, closing, boxed := -1, -1, false
	for i := 0; i < len(lines); i++ {
		if c, b, ok := claudeComposerBlockAt(lines, i); ok {
			open, closing, boxed = i, c, b
			i = c
		}
	}
	if open < 0 || !claudeComposerDrawnLast(lines[closing+1:]) {
		return screen
	}
	prompt := claudeComposerCanonicalPrompt
	if boxed {
		prompt = claudeComposerCanonicalBoxedPrompt
	}
	if closing == open+2 && lines[open+1] == prompt {
		return screen
	}
	out := make([]string, 0, len(lines)-(closing-open-2))
	out = append(append(append(out, lines[:open+1]...), prompt), lines[closing:]...)
	return visibleText(strings.Join(out, "\n"))
}

// claudeComposerDrawnLast reports whether below, the lines under a composer's
// closing line, are only what a composer drawn last paints under itself: its
// one status row, straight under the closing line and indented from the first
// column ("  ? for shortcuts", "  ⏵⏵ auto mode on …"), and blank rows. Two rows
// or more below a composer are something painted after it, such as a live
// dialog's 2.1.322 rows, which are indented too, under a composer an earlier
// Claude left on the pane; and every clause that answers a dialog needs more
// than one of its rows on screen.
func claudeComposerDrawnLast(below []string) bool {
	for i, line := range below {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if i > 0 || !strings.HasPrefix(line, " ") || claudeTrustFrameRule(line) {
			return false
		}
	}
	return true
}

// claudeComposerCanonicalPrompt is the empty prompt row 2.1.322 paints between
// its composer's rules: ❯ and U+00A0
// (testdata/claude_2_1_322_composer_after_mcp_decline.ansi).
// claudeComposerCanonicalBoxedPrompt is the empty prompt row of the boxed
// composer older builds draw.
const (
	claudeComposerCanonicalPrompt      = claudeTrustSelectionGlyph + "\u00a0"
	claudeComposerCanonicalBoxedPrompt = "│ > │"
)

// claudeComposerBlockAt reports a whole composer block whose opening line is
// lines[open], and the line that closes it. Claude Code draws two:
//
//	────────────────          ╭────────────────╮
//	❯ Explain this:           │ > Explain this: │
//	  New MCP server          │   New MCP server │
//	────────────────          ╰────────────────╯
//
// Every part of it is the composer's own:
//
//   - The opening and closing lines start at the pane's first column: plain
//     rules ("─" and nothing else) around a ruled composer, a box's top and
//     bottom edges around a boxed one. The closing line is the first such line
//     after the prompt, so no rule of the composer's own kind is ever a draft
//     row.
//   - The prompt row is the line straight under the opening one. A ruled
//     composer paints it from the first column, a boxed one inside its side
//     borders: the glyph of one of its modes (❯, or ">", "!" or "#",
//     claudeComposerPromptLabel), then nothing, or a space and text. The text
//     is not the start of a row a dialog paints on its options: an ordinal, a
//     checkbox, or an option label, whole or cut short
//     (claudeComposerPromptIsAnOption). So a dialog's cursor row never opens a
//     composer.
//   - Every line between the prompt and the closing line is a draft row: under
//     a ruled composer, blank or indented under the prompt's text; inside a box,
//     between its side borders. What a draft row says does not matter. A draft
//     can quote a whole dialog, its cursor and its frame included.
//
// An empty prompt row opens no draft: the composer is the three lines alone.
func claudeComposerBlockAt(lines []string, open int) (closing int, boxed, ok bool) {
	if open+2 >= len(lines) {
		return 0, false, false
	}
	switch {
	case claudeComposerOwnRule(lines[open]):
	case claudeComposerOwnEdge(lines[open], "╭┌"):
		boxed = true
	default:
		return 0, false, false
	}
	text, ok := claudeComposerPromptText(lines[open+1], boxed)
	if !ok {
		return 0, false, false
	}
	for i := open + 2; i < len(lines); i++ {
		line := lines[i]
		switch {
		case !boxed && claudeComposerOwnRule(line), boxed && claudeComposerOwnEdge(line, "╰└"):
			if i > open+2 && text == "" {
				return 0, false, false
			}
			return i, boxed, true
		case boxed && claudeComposerBordered(line):
		case !boxed && (strings.TrimSpace(line) == "" || strings.HasPrefix(line, "  ")):
		default:
			return 0, false, false
		}
	}
	return 0, false, false
}

// claudeComposerOwnRule reports a plain rule painted from the pane's first
// column: the line above and below a ruled composer. A rule indented under a
// prompt is a row of the draft.
func claudeComposerOwnRule(line string) bool {
	return strings.HasPrefix(line, "─") && claudeComposerRule(line)
}

// claudeComposerOwnEdge reports a box's top or bottom edge painted from the
// pane's first column, its first rune one of corners.
func claudeComposerOwnEdge(line, corners string) bool {
	first, _ := utf8.DecodeRuneInString(line)
	return strings.ContainsRune(corners, first) && claudeTrustFrameRule(line)
}

// claudeComposerBordered reports a line between a box's side borders, painted
// from the pane's first column.
func claudeComposerBordered(line string) bool {
	line = strings.TrimRight(line, " ")
	first, _ := utf8.DecodeRuneInString(line)
	last, _ := utf8.DecodeLastRuneInString(line)
	return len(line) > 1 && strings.ContainsRune(claudeTrustBoxSides, first) && strings.ContainsRune(claudeTrustBoxSides, last)
}

// claudeComposerPromptText reads line as a composer's prompt row and returns
// the text after its glyph. ok is false when it is no prompt row.
func claudeComposerPromptText(line string, boxed bool) (text string, ok bool) {
	if boxed {
		if !claudeComposerBordered(line) {
			return "", false
		}
		line = strings.TrimSpace(strings.Trim(strings.TrimSpace(line), claudeTrustBoxSides))
	}
	if !strings.HasPrefix(line, claudeTrustSelectionGlyph) && !claudeComposerPromptLabel(line) {
		return "", false
	}
	_, size := utf8.DecodeRuneInString(line)
	rest := line[size:]
	if rest != "" {
		after, _ := utf8.DecodeRuneInString(rest)
		if after != ' ' && after != '\u00a0' {
			return "", false
		}
	}
	text = strings.TrimSpace(rest)
	if claudeComposerPromptIsAnOption(text) {
		return "", false
	}
	return text, true
}

// claudeComposerOptionLabels are the option rows of every dialog the gate
// reads: the folder-trust dialogs, reworded and legacy, the 2.1.322 MCP-server
// dialog, the multi-server picker's submit row, and the old MCP modal.
var claudeComposerOptionLabels = []string{
	claudeTrustAffirmativeLabel, claudeFolderTrustExitLabel, claudeLegacyTrustProceedLabel,
	claudeMCPServerUseLabel, claudeMCPServerUseAllLabel, claudeMCPServerDeclineLabel,
	claudeMCPServersSubmitLabel, "Yes", "No",
}

// claudeComposerPromptIsAnOption reports prompt text that begins a row a
// dialog paints on its options, so a prompt row that holds it is the dialog's
// cursor row and opens no composer: an ordinal ("1." or "1. Yes"), a checkbox
// ("[✔] alpha"), or one of claudeComposerOptionLabels, whole or cut short by a
// narrow pane or a paint caught mid-row.
func claudeComposerPromptIsAnOption(text string) bool {
	if text == "" {
		return false
	}
	if word, _, _ := strings.Cut(text, " "); claudeMCPTrustOrdinalWord(word) || strings.HasPrefix(text, "[") {
		return true
	}
	lower := strings.ToLower(text)
	for _, label := range claudeComposerOptionLabels {
		if strings.HasPrefix(strings.ToLower(label), lower) {
			return true
		}
	}
	return false
}

// claudeComposerRuleUnderDialog reports a pane whose trailing frame chrome, the
// lines below its last content past blank rows, holds a plain rule from the
// first column. Every answering clause requires its dialog's footer to be the
// last content, with only blank rows and frame chrome below it, so on a pane
// any of them answers, that rule is below the dialog's footer.
//
// No dialog the gate reads paints one there. The 2.1.322 dialogs end with their
// footer, and a boxed dialog closes with its bottom edge (╰…╯), not a plain
// rule. A ruled composer closes with one. So a dialog over such a rule is a
// composer's draft, on a pane too short to show the composer's prompt and
// opening rule, and an Enter there would submit the draft (#7464). The gate
// holds it instead, and sends nothing.
func claudeComposerRuleUnderDialog(screen visibleText) bool {
	lines := screen.lines()
	for i := len(lines) - 1; i >= 0; i-- {
		switch {
		case strings.TrimSpace(lines[i]) == "":
		case claudeComposerOwnRule(lines[i]):
			return true
		case claudeTrustFrameRule(lines[i]):
		default:
			return false
		}
	}
	return false
}

// refuseClaudeDialogInADraft reports, once per dialog rather than once per
// poll, that af is holding on a dialog with a composer's closing rule under its
// footer (claudeComposerRuleUnderDialog).
func (t *TmuxSession) refuseClaudeDialogInADraft() {
	if t.claudeTrust.refusalLogged {
		return
	}
	t.claudeTrust.refusalLogged = true
	log.ErrorLog.Printf(
		"refusing to answer the %s dialog for session %q: a plain rule is painted under its footer, where only the agent's composer draws one, "+
			"so the dialog is likely text in the composer's draft, on a pane too short to show the composer's prompt; "+
			"af will not press Enter, because that would submit the draft; make the pane taller, or answer the dialog in the session",
		claudeLaunchGateDialogName, t.sanitizedName)
}
