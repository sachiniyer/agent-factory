package tmux

import "strings"

// claudeFolderTrustSentence is the reworded folder-trust dialog's question as
// Claude Code paints it, from the start of the sentence it sits in to the
// question's end. It begins a row of its own.
const claudeFolderTrustSentence = "Quick safety check: " + claudeFolderTrustQuestion

var claudeFolderTrustSentenceSquashed = claudeSquash(claudeFolderTrustSentence)

// claudeFolderTrustQuestionPaintingOf reports the reworded folder-trust dialog
// while Claude Code paints its question with no title above it (#7585).
//
// Claude Code paints the dialog top-down. Once the question is whole,
// claudeFolderTrustDialogOf holds every frame, and while the title is on
// screen, claudeFolderTrustPaintingOf does. A pane whose title is gone, which
// a pane too short scrolls off, read clear from the sentence's first letter
// until the question was whole. On a clear pane, the create typed its prompt
// and pressed Enter into the dialog.
//
// It is asked after every other clause CheckAndHandleTrustPrompt asks before
// it reports a Claude pane clear. It only holds, and sends no key. Every clause
// that answers a dialog has declined the frame before it runs, so it turns a
// clear pane into a hold and changes nothing else.
//
// It holds when all of these are true. The pane's rows are read squashed, as
// claudeLaunchDialogPaneOf reads them, so that no wrap point matters:
//
//   - a row begins the sentence, and from it to the end of the pane is the
//     sentence begun, but not as far as the question's end. A whole question
//     is for the readers before this one;
//   - from that row down, there is no frame rule and no ❯. A working agent
//     frames its composer with rules, plain or boxed, so a sentence it quotes
//     has one below it;
//   - that row is no line of a composer's draft. The dialog paints a blank
//     row, or its own frame, above the sentence. A draft's lines hang under
//     its prompt row with nothing between, so a ❯ prompt row holding text
//     (#7575's claudeComposerPromptText) above the row, with only rows of
//     text between, makes it a line of that draft.
//
// Nothing above that row is asked: a composer that a relaunch in the same
// pane left there explains only its own rows (#7388).
func claudeFolderTrustQuestionPaintingOf(screen visibleText) bool {
	pane := claudeLaunchDialogPaneOf(screen)
	head := -1
	for content := len(pane.starts) - 1; content >= 0; content-- {
		// A row that paints text of its own: a bare ❯ row above the sentence,
		// such as an empty composer prompt, starts at the same offset.
		if pane.rows[pane.index[content]].selected ||
			(content+1 < len(pane.starts) && pane.starts[content+1] == pane.starts[content]) {
			continue
		}
		painted := pane.text[pane.starts[content]:]
		if painted != "" && len(painted) < len(claudeFolderTrustSentenceSquashed) &&
			strings.HasPrefix(claudeFolderTrustSentenceSquashed, painted) {
			head = content
		}
	}
	if head < 0 {
		return false
	}
	for content := head; content < len(pane.starts); content++ {
		if pane.rows[pane.index[content]].selected {
			return false
		}
	}
	for i := pane.index[head]; i < len(pane.lines); i++ {
		if claudeTrustFrameRule(pane.lines[i]) {
			return false
		}
	}
	for i := pane.index[head] - 1; i >= 0 && !pane.rows[i].blank && !claudeTrustFrameRule(pane.lines[i]); i-- {
		if draft, prompt := claudeComposerPromptText(pane.lines[i], claudeComposerBordered(pane.lines[i])); prompt && draft != "" {
			return false
		}
	}
	return true
}
