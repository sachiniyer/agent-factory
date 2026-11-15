package tmux

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// claudeMCPTrustTail is what af saw of the old single-server MCP trust modal
// when it could not read the whole of it: the option row its footer sits under,
// the footer as painted, and how many rows are painted below the footer.
type claudeMCPTrustTail struct {
	option string
	footer string
	below  int
}

// claudeMCPTrustTailOf reports the old single-server MCP trust modal ("New MCP
// server found. Do you trust this new MCP server? ❯ 1. Yes / 2. No / Enter to
// confirm") in every shape claudeMCPTrustFooterIsLast declines: the pane shows
// the modal's footer under one of its own option rows, and no composer below.
//
// It is the last clause CheckAndHandleTrustPrompt asks before it reports a
// Claude pane clear, and a clear pane is the create path's permission to type
// the user's prompt and press Enter — which this modal takes on its preselected
// "1. Yes", trusting an MCP server that can execute code (#3579 class, #7306).
// It can only hold: it sends no key, and it runs after every clause that
// answers a dialog has declined the frame.
//
// It holds when all of these are true:
//
//   - a row is the modal's footer, "Enter to confirm…", on one row or wrapped
//     by a pane under 16 columns ("Enter to" / "confirm");
//   - the content row straight above it, past blank rows and frame chrome, is
//     exactly one of the modal's option labels, "Yes" or "No", once
//     claudeTrustRowOf has taken off the cursor and the ordinal;
//   - no row below the footer is a composer's prompt row.
//
// Nothing else on screen is asked. What is above the option row does not
// matter: a pane two rows tall shows one option row, a taller one shows both,
// and a pane narrow enough to wrap the question shows the preamble too, which
// claudeMCPTrustFooterIsLast cannot read as the question and so declines.
// Seeing the preamble is evidence for the dialog, never against it (#7315
// review, R1 and R2). What is below the footer matters in one way only: a hint
// row, or the rest of a footer too long for the pane, is no evidence the
// dialog is gone (R3), and a composer is. A working agent paints its composer
// below its output, so a modal it merely quotes has one under it and reads
// clear.
//
// An unrelated picker that ends in a Yes or No row over the same footer holds
// too. af cannot tell it from the modal, and a prompt typed into either is
// lost. The cost of the hold is the create's trust budget running out, which
// is loud and can be retried.
func claudeMCPTrustTailOf(screen visibleText) (claudeMCPTrustTail, bool) {
	lines := screen.lines()
	rows := make([]claudeTrustRow, len(lines))
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
	}
	// The lowest composer prompt row. A footer above it is quoted output.
	composer := -1
	for i := len(rows) - 1; i >= 0; i-- {
		if claudeMCPTrustComposerRow(lines, rows, i) {
			composer = i
			break
		}
	}
	for footer := len(rows) - 1; footer > composer; footer-- {
		text, end, ok := claudeMCPTrustFooterAt(rows, footer)
		if !ok {
			continue
		}
		option := footer - 1
		for option >= 0 && rows[option].blank {
			option--
		}
		tail := claudeMCPTrustTail{footer: text}
		switch {
		case option < 0:
			// The footer is the first content the pane shows: a pane too short
			// for even one option row.
		case claudeMCPTrustOptionLabel(rows[option].label):
			tail.option = rows[option].label
		default:
			continue
		}
		for i := end + 1; i < len(rows); i++ {
			if !rows[i].blank {
				tail.below++
			}
		}
		return tail, true
	}
	return claudeMCPTrustTail{}, false
}

// claudeMCPTrustFooterMaxRows is how many rows a wrapped "Enter to confirm" can
// take: one per word.
const claudeMCPTrustFooterMaxRows = 3

// claudeMCPTrustFooterAt reads the modal's footer starting at row i: the row
// itself when it starts with "Enter to confirm", or that row and the one or two
// straight below it when a narrow pane wrapped the footer at its spaces and the
// rows read back as it once joined. end is the footer's last row.
func claudeMCPTrustFooterAt(rows []claudeTrustRow, i int) (text string, end int, ok bool) {
	if rows[i].blank {
		return "", 0, false
	}
	affordance := strings.ToLower(claudeTrustAffordancePrefix)
	joined := rows[i].label
	if strings.HasPrefix(strings.ToLower(joined), affordance) {
		return joined, i, true
	}
	pieces := 1
	for end = i + 1; end < len(rows) && pieces < claudeMCPTrustFooterMaxRows; end++ {
		if rows[end].blank {
			// Blank rows and frame chrome: a boxed dialog's closing rule can
			// fall between two rows of a footer the pane wrapped.
			continue
		}
		// Only a row that is the footer's own first word or words can open a
		// wrapped footer: "Enter" or "Enter to".
		if !strings.HasPrefix(affordance, strings.ToLower(joined)+" ") {
			return "", 0, false
		}
		joined += " " + rows[end].label
		pieces++
		if strings.HasPrefix(strings.ToLower(joined), affordance) {
			return joined, end, true
		}
	}
	return "", 0, false
}

// claudeMCPTrustOptionLabel reports whether label is exactly one of the modal's
// option labels. Exact, not a suffix: "Always yes" is another picker's row.
func claudeMCPTrustOptionLabel(label string) bool {
	label = strings.ToLower(label)
	for _, option := range claudeMCPOptionLabels {
		if label == option {
			return true
		}
	}
	return false
}

// claudeMCPTrustComposerRow reports whether row i is a working agent's prompt
// row: a ❯ row, which the modal never paints below its footer, or the "> " row
// of the boxed composer older builds draw, straight below its frame rule. The
// same two shapes claudeFolderTrustComposerRow reads.
func claudeMCPTrustComposerRow(lines []string, rows []claudeTrustRow, i int) bool {
	if rows[i].selected {
		return true
	}
	label := rows[i].label
	return i > 0 && claudeTrustFrameRule(lines[i-1]) && (label == ">" || strings.HasPrefix(label, "> "))
}

// refuseClaudeMCPTrustTail reports, once per dialog rather than once per poll,
// that af is holding on an old MCP trust modal it cannot read whole, and what
// it saw. The hold surfaces as the create's trust-prompt budget expiring;
// without this line that failure names no reason (#7315 review, R4). Mirrors
// refuseTrustPicker.
func (t *TmuxSession) refuseClaudeMCPTrustTail(tail claudeMCPTrustTail) {
	if t.claudeTrust.refusalLogged {
		return
	}
	t.claudeTrust.refusalLogged = true
	below := "the last content in the pane"
	if tail.below > 0 {
		below = fmt.Sprintf("with %d more row(s) below it and no composer", tail.below)
	}
	log.ErrorLog.Printf(
		"refusing to answer the %s dialog for session %q: the pane shows its footer %q under the option row %q, %s, "+
			"but not a dialog af can read whole (a pane too short or too narrow cuts or wraps it); "+
			"af will not press Enter on a preselection it cannot read, because that could trust an MCP server; answer it in the session",
		claudeLaunchGateDialogName, t.sanitizedName, tail.footer, tail.option, below)
}

// claudeMCPTrustModalWords is the old MCP trust modal, top to bottom, as the
// words a pane of any width paints in this order: the preamble, the question,
// the two option labels and the footer. Claude Code's renderer breaks a row
// only at its spaces, so a pane too narrow for a row changes which words share
// a row and never the words or their order.
var claudeMCPTrustModalWords = strings.Fields(
	claudeMCPDialogPreamble + ". " + claudeMCPDialogQuestion + "? " +
		strings.Join(claudeMCPOptionLabels, " ") + " " + strings.ToLower(claudeTrustAffordancePrefix))

// claudeMCPTrustOptionWordsAt is where the option labels begin in
// claudeMCPTrustModalWords, and claudeMCPTrustFooterWordsAt where they end.
var (
	claudeMCPTrustOptionWordsAt = len(strings.Fields(claudeMCPDialogPreamble + ". " + claudeMCPDialogQuestion + "?"))
	claudeMCPTrustFooterWordsAt = claudeMCPTrustOptionWordsAt + len(claudeMCPOptionLabels)
)

// claudeMCPTrustPaintingOf reports the old MCP trust modal caught while Claude
// Code is still painting it, at any pane width (#7330): from some row down to
// the last content in the pane, the rows spell the modal's words from its
// first, in order, and stop short of the end or at it.
//
// claudeMCPDialogPartiallyRendered holds the same moment only while the
// preamble sits whole on one row. Under 21 columns the preamble wraps, and
// under 33 a question on its own row does, so a capture taken between two rows
// of that paint read clear: the create typed its prompt, and its Enter landed
// on the preselected Yes once the modal finished painting.
//
// Rows are read as claudeTrustRowOf reads them, so frame chrome and blank rows
// are skipped and an option row's cursor and ordinal are not words. A ❯ row is
// the modal's only on an option: one anywhere earlier is a composer holding
// the words as a draft, and no modal. It holds without a key and says nothing:
// the frame lasts as long as one paint.
func claudeMCPTrustPaintingOf(screen visibleText) bool {
	lines := screen.lines()
	rows := make([]claudeTrustRow, len(lines))
	last := -1
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
		if !rows[i].blank {
			last = i
		}
	}
	for start := last; start >= 0; start-- {
		if rows[start].blank || !strings.HasPrefix(strings.ToLower(rows[start].label), claudeMCPTrustModalWords[0]) {
			continue
		}
		if claudeMCPTrustPaintedFrom(rows, start, last) {
			return true
		}
	}
	return false
}

// claudeMCPTrustPaintedFrom reports whether rows start to last spell the
// modal's words from the first, in order.
func claudeMCPTrustPaintedFrom(rows []claudeTrustRow, start, last int) bool {
	at := 0
	for i := start; i <= last; i++ {
		if rows[i].blank {
			continue
		}
		words := strings.Fields(strings.ToLower(rows[i].label))
		ordinal := false
		if n := len(words); n > 0 && claudeMCPTrustOrdinalWord(words[n-1]) {
			// "❯ 1." on a row of its own: a pane under 8 columns wrapped the
			// label off its ordinal.
			words, ordinal = words[:n-1], true
		}
		if rows[i].selected {
			// The cursor sits on an option row, or on the ordinal a narrow
			// pane left of one. A bare ❯ is a composer.
			if at < claudeMCPTrustOptionWordsAt || at >= claudeMCPTrustFooterWordsAt || (len(words) == 0 && !ordinal) {
				return false
			}
		}
		if ordinal && (at < claudeMCPTrustOptionWordsAt || at >= claudeMCPTrustFooterWordsAt) {
			return false
		}
		for _, word := range words {
			if at >= len(claudeMCPTrustModalWords) || word != claudeMCPTrustModalWords[at] {
				return false
			}
			at++
		}
	}
	return at > 0
}

// claudeMCPTrustOrdinalWord reports whether word is a picker ordinal with no
// label after it: "1.", "2.".
func claudeMCPTrustOrdinalWord(word string) bool {
	digits, ok := strings.CutSuffix(word, ".")
	if !ok || digits == "" || len(digits) > 3 {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
