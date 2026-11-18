package tmux

import "strings"

// claudeFolderTrustTitle is the reworded folder-trust dialog's title, the first
// row it paints under its top rule.
const claudeFolderTrustTitle = "Accessing workspace:"

var (
	claudeFolderTrustTitleSquashed    = claudeSquash(claudeFolderTrustTitle)
	claudeFolderTrustQuestionSquashed = claudeSquash(claudeFolderTrustQuestion)
	claudeFolderTrustFooterSquashed   = claudeSquash(claudeFolderTrustFooter)
	// claudeFolderTrustOptionTexts are the option rows' text as the ❯ row can
	// begin it: with the ordinal Claude Code paints before each label, and
	// without it, since claudeTrustRowOf drops "N. " once the label follows.
	claudeFolderTrustOptionTexts = []string{
		"1." + claudeSquash(claudeTrustAffirmativeLabel), claudeSquash(claudeTrustAffirmativeLabel),
		"2." + claudeSquash(claudeFolderTrustExitLabel), claudeSquash(claudeFolderTrustExitLabel),
	}
)

// claudeFolderTrustPaintingOf reports the reworded folder-trust dialog while
// Claude Code paints it, read from its head down whatever is above the head
// (#7505).
//
// Claude Code paints the dialog top-down. claudeFolderTrustDialogOf holds a
// frame of that paint once it finds the question, but some frames read clear:
//
//   - The title and path, and the paragraph before its question is whole.
//     No reader anchors on them.
//   - A ❯ row the paint has only begun, or a narrow pane has cut short:
//     "❯ 1." before the label, or "❯ 1. Yes," over "I" at 10 columns or
//     fewer, the first row of a label wrapped at its spaces. The grammar takes
//     a ❯ row that is no option it knows for a composer, and a composer below
//     the question for a quote.
//
// In each, the create typed its prompt and pressed Enter into the dialog.
//
// It is the last clause CheckAndHandleTrustPrompt asks before it reports a
// Claude pane clear. It only holds, and sends no key. Every clause that
// answers a dialog has declined the frame before it runs, so it turns a clear
// pane into a hold and changes nothing else.
//
// It holds when all of these are true. The pane's rows are read squashed, as
// claudeLaunchDialogPaneOf reads them, so that no wrap point matters:
//
//   - the dialog's head is on screen: its title as rows of their own, or its
//     question, with no ❯ on any row of it. The last place either appears
//     is the head;
//   - from the head's first row down, there is no frame rule. A working
//     agent frames its composer with rules, plain or boxed, so a dialog it
//     quotes has one below the head;
//   - below the head there is at most one ❯ row, and it begins one of the
//     dialog's options, its ordinal and label as far as they are painted;
//   - the paint has not reached the whole footer. A whole dialog is for the
//     readers before this one.
//
// Nothing above the head is asked: a composer that a relaunch in the same pane
// left there explains only its own rows (#7388).
func claudeFolderTrustPaintingOf(screen visibleText) bool {
	pane := claudeLaunchDialogPaneOf(screen)
	head, end, ok := claudeFolderTrustHeadOf(pane)
	if !ok || strings.Contains(pane.text[end:], claudeFolderTrustFooterSquashed) {
		return false
	}
	for i := pane.index[head]; i < len(pane.lines); i++ {
		if claudeTrustFrameRule(pane.lines[i]) {
			return false
		}
	}
	cursors := 0
	for content := head; content < len(pane.starts); content++ {
		row := pane.rows[pane.index[content]]
		if !row.selected {
			continue
		}
		cursors++
		if cursors > 1 || !claudeFolderTrustOptionBegun(row.label) {
			return false
		}
	}
	return true
}

// claudeFolderTrustHeadOf finds the dialog's head in the pane: the last place
// its title, as rows of their own, or its question appears, with no ❯ on the
// rows it spans. head is its first content row; end is where it ends in the
// pane's squashed text.
func claudeFolderTrustHeadOf(pane claudeLaunchDialogPane) (head, end int, ok bool) {
	at := -1
	if title := strings.LastIndex(pane.text, claudeFolderTrustTitleSquashed); title >= 0 {
		titleEnd := title + len(claudeFolderTrustTitleSquashed)
		_, starts := pane.rowStart(title)
		_, ends := pane.rowStart(titleEnd)
		if starts && (ends || titleEnd == len(pane.text)) {
			at, end = title, titleEnd
		}
	}
	if question := strings.LastIndex(pane.text, claudeFolderTrustQuestionSquashed); question > at {
		at, end = question, question+len(claudeFolderTrustQuestionSquashed)
	}
	if at < 0 {
		return 0, 0, false
	}
	for content := range pane.starts {
		if pane.starts[content] <= at {
			head = content
		}
	}
	for content := head; content < len(pane.starts) && pane.starts[content] < end; content++ {
		if pane.rows[pane.index[content]].selected {
			return 0, 0, false
		}
	}
	return head, end, true
}

// claudeFolderTrustOptionBegun reports whether a ❯ row's label begins one of
// the dialog's options as far as it is painted: nothing yet, its ordinal, or
// its ordinal and label up to where the paint or the pane's width cut it.
func claudeFolderTrustOptionBegun(label string) bool {
	text := claudeSquash(label)
	for _, option := range claudeFolderTrustOptionTexts {
		if strings.HasPrefix(option, text) {
			return true
		}
	}
	return false
}
