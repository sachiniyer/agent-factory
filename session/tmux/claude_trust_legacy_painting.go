package tmux

import "strings"

// claudeLegacyTrustOptionBlocks are the legacy dialog's option blocks as the
// pane reads them from the cursor row down: squashed, lower-cased, and with or
// without the ordinal before each label, since claudeTrustRowOf drops "N. "
// once the label follows it. Each pairs a Yes label with the No label the
// dialog paints with it: "Yes" with "No", "Yes, proceed" with "No, exit". A
// whole short pair is then never read as a long one begun.
var claudeLegacyTrustOptionBlocks = func() []string {
	var blocks []string
	for _, pair := range [][2]string{{"yes", "no"}, {"yes, proceed", "no, exit"}} {
		for _, first := range []string{"", "1. "} {
			for _, second := range []string{"", "2. "} {
				blocks = append(blocks, claudeSquash(first+pair[0]+" "+second+pair[1]))
			}
		}
	}
	return blocks
}()

// claudeLegacyTrustPaintingOf reports the legacy folder-trust dialog while
// Claude Code paints its option rows (#7584).
//
// Claude Code paints the dialog top-down: its question, its body, then
// "❯ 1. Yes, proceed" and "2. No, exit". claudeLegacyTrustDialogOf holds the
// frames before the options and the frames with the Yes row whole, but a row
// the paint has only begun reads clear: "❯", "❯ 1." or "❯ 1. Yes," as the
// cursor row, or "2", "2." or "2. No," under it. The grammar takes a ❯ row
// that is no option for a composer's draft, and any other row below the
// options for content the dialog never paints. On a clear pane the create
// typed its prompt and pressed Enter into the dialog.
//
// It is asked after every other clause CheckAndHandleTrustPrompt asks before
// it reports a Claude pane clear. It only holds, and sends no key. Every clause
// that answers a dialog has declined the frame before it runs, so it turns a
// clear pane into a hold and changes nothing else.
//
// It holds when all of these are true. The pane's rows are read squashed, as
// claudeLaunchDialogPaneOf reads them, so that no wrap point matters:
//
//   - the dialog's question is on screen as rows of its own, with no ❯ on
//     them. The last place it appears is the head;
//   - from the head's first row down, there is no frame rule. A working agent
//     frames its composer with rules, plain or boxed, so a question it quotes
//     has one below it;
//   - below the head there is exactly one ❯ row, the dialog's cursor, and it
//     is no composer's draft line. #7575's composer reading
//     (claudeComposerPromptText) reads a ❯ prompt row whose text begins no
//     option as a composer's prompt, and its text as a draft, such as both
//     options on the glyph's row ("❯ Yes No"). The dialog never paints that,
//     so a pane with one is a user's draft under a quoted question;
//   - everything from the cursor row to the end of the pane is an option
//     block begun but not yet whole. A whole block is for the grammar, which answers it or
//     reads it as a draft (claudeLegacyProseAboveQuestion), and a block
//     followed by more rows is for the readers before this one.
//
// Nothing above the head is asked: a composer that a relaunch in the same pane
// left there explains only its own rows (#7388).
func claudeLegacyTrustPaintingOf(screen visibleText) bool {
	pane := claudeLaunchDialogPaneOf(screen)
	head, end, ok := claudeLegacyTrustHeadOf(pane)
	if !ok {
		return false
	}
	for i := pane.index[head]; i < len(pane.lines); i++ {
		if claudeTrustFrameRule(pane.lines[i]) {
			return false
		}
	}
	cursor := -1
	for content := head; content < len(pane.starts); content++ {
		if !pane.rows[pane.index[content]].selected {
			continue
		}
		if cursor >= 0 || pane.starts[content] < end {
			return false
		}
		cursor = content
	}
	if cursor < 0 {
		return false
	}
	line := pane.lines[pane.index[cursor]]
	if draft, prompt := claudeComposerPromptText(line, claudeComposerBordered(line)); prompt && draft != "" {
		return false
	}
	painted := strings.ToLower(pane.text[pane.starts[cursor]:])
	for _, block := range claudeLegacyTrustOptionBlocks {
		if len(painted) < len(block) && strings.HasPrefix(block, painted) {
			return true
		}
	}
	return false
}

// claudeLegacyTrustHeadOf finds the legacy dialog's question in the pane: its
// last appearance as rows of their own. head is its first content row; end is
// where it ends in the pane's squashed text.
func claudeLegacyTrustHeadOf(pane claudeLaunchDialogPane) (head, end int, ok bool) {
	at := strings.LastIndex(pane.text, claudeLegacyTrustQuestionSquashed)
	if at < 0 {
		return 0, 0, false
	}
	end = at + len(claudeLegacyTrustQuestionSquashed)
	// The last content row that starts at or before it: a bare ❯ row above
	// the question, such as an empty composer prompt, has no text and starts
	// at the same offset, and is not the question's row.
	head = -1
	for content, start := range pane.starts {
		if start <= at {
			head = content
		}
	}
	if head < 0 || pane.starts[head] != at {
		return 0, 0, false
	}
	if _, ends := pane.rowStart(end); !ends && end != len(pane.text) {
		return 0, 0, false
	}
	for content := head; content < len(pane.starts) && pane.starts[content] < end; content++ {
		if pane.rows[pane.index[content]].selected {
			return 0, 0, false
		}
	}
	return head, end, true
}
