package tmux

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/log"
)

// The re-wrap matcher (#7346). The folder-trust dialogs kept reading clear at
// one more width or wrap point, because each hold before this read a shape:
// rows, then words. This one reads the dialog itself.
//
// For a launch dialog whose text af knows, the rows a pane paints are that
// text broken into rows at the pane's width, with blank rows and frame chrome
// between them. Remove every space and every break and the two are the same
// string, whatever the width and wherever a row broke, between words or inside
// one. So the matcher compares text with its whitespace removed:
//
//   - a pane whose content ENDS with the dialog's option block and whole footer
//     is the dialog scrolled off the top, at any width (claudeLaunchDialogTail);
//   - a pane whose content ENDS with the start of the legacy dialog's question,
//     or with the whole question and the rows it paints below it, is that
//     dialog being painted (claudeLaunchDialogPainting).
//
// It is asked last, after every clause that answers or holds, so it only turns
// a clear pane into a hold. It sends no key.

// claudeLaunchDialogKind names which dialog a re-wrap match saw.
type claudeLaunchDialogKind int

const (
	claudeLaunchDialogNone claudeLaunchDialogKind = iota
	claudeLaunchDialogReworded
	claudeLaunchDialogLegacy
	// claudeLaunchDialogEither: the pane shows only words the legacy
	// folder-trust question and the old MCP modal's question share ("Do you
	// trust th…"), so af cannot say which of the two is being painted.
	claudeLaunchDialogEither
)

// claudeLaunchDialogMatch is what the re-wrap matcher saw: which dialog, and
// which part of it the pane shows.
type claudeLaunchDialogMatch struct {
	kind claudeLaunchDialogKind
	saw  string
}

func (m claudeLaunchDialogMatch) name() string {
	switch m.kind {
	case claudeLaunchDialogLegacy:
		return claudeLegacyFolderTrustDialogName
	case claudeLaunchDialogEither:
		return claudeLaunchGateDialogName
	}
	return claudeFolderTrustDialogName
}

// aim is what af would have to aim a key at, for the refusal line.
func (m claudeLaunchDialogMatch) aim() string {
	switch m.kind {
	case claudeLaunchDialogLegacy:
		return fmt.Sprintf("%q", claudeLegacyTrustProceedLabel)
	case claudeLaunchDialogEither:
		return "an option it has not seen painted"
	}
	return fmt.Sprintf("%q", claudeTrustAffirmativeLabel)
}

// claudeLegacyFolderTrustDialogName is the legacy folder-trust dialog as a
// user would recognize it, for a refusal line.
const claudeLegacyFolderTrustDialogName = "Claude legacy folder-trust"

// claudeSquash removes every space from s: the text a row paints, without the
// breaks a pane's width put into it.
func claudeSquash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// The folder-trust dialogs' option labels and footers, squashed.
var (
	claudeLaunchDialogLabels = []string{
		claudeSquash(claudeTrustAffirmativeLabel),
		claudeSquash(claudeLegacyTrustProceedLabel),
		claudeSquash(claudeFolderTrustExitLabel),
	}
	claudeLaunchDialogFooters = []string{
		claudeSquash(claudeFolderTrustFooter),
		claudeSquash(claudeLegacyTrustFooter),
	}
	claudeLegacyTrustQuestionSquashed = claudeSquash(claudeLegacyTrustQuestion)
	// The old MCP modal's question as it paints it: "Do you trust this new
	// MCP server?". Its first words are the legacy question's too.
	claudeMCPTrustQuestionSquashed = "Doyoutrustthisnew" + "MCPserver?"
)

// claudeLaunchDialogPane is a pane's content rows as the matcher reads them:
// each row's text squashed, in order, with where each row begins in the whole.
type claudeLaunchDialogPane struct {
	lines  []string
	rows   []claudeTrustRow
	text   string
	starts []int // offset in text where content row i begins
	index  []int // the pane row each content row is
}

func claudeLaunchDialogPaneOf(screen visibleText) claudeLaunchDialogPane {
	p := claudeLaunchDialogPane{lines: screen.lines()}
	p.rows = make([]claudeTrustRow, len(p.lines))
	var text strings.Builder
	for i, line := range p.lines {
		p.rows[i] = claudeTrustRowOf(line)
		if p.rows[i].blank {
			continue
		}
		p.starts = append(p.starts, text.Len())
		p.index = append(p.index, i)
		text.WriteString(claudeSquash(p.rows[i].label))
	}
	p.text = text.String()
	return p
}

// rowStart reports whether offset at is where a content row begins, and which.
func (p claudeLaunchDialogPane) rowStart(at int) (content int, ok bool) {
	for i, start := range p.starts {
		if start == at {
			return i, true
		}
	}
	return 0, false
}

// claudeLaunchDialogRewrapOf reports a folder-trust dialog the pane ends with a
// part of, as wrapped at any width.
func claudeLaunchDialogRewrapOf(screen visibleText, paneWidth claudePaneWidth) (claudeLaunchDialogMatch, bool) {
	pane := claudeLaunchDialogPaneOf(screen)
	if len(pane.starts) == 0 {
		return claudeLaunchDialogMatch{}, false
	}
	if match, ok := claudeLaunchDialogTail(pane); ok {
		return match, true
	}
	return claudeLaunchDialogPainting(pane, paneWidth)
}

// claudePaneWidth reads the pane's width from tmux, bounded like every other
// read on this path. A failed or implausible read is not a width.
func (t *TmuxSession) claudePaneWidth() (int, bool) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", ExactTarget(t.sanitizedName), "#{pane_width}")
	if err != nil {
		return 0, false
	}
	var width int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(output)), "%d", &width); err != nil || width <= 0 {
		return 0, false
	}
	return width, true
}

// claudeLaunchDialogTail reports a pane whose content ends with a folder-trust
// dialog's option block and its whole footer. Reading up from the footer, the
// text is whole option labels, each with or without its ordinal. What is above
// them decides the rest:
//
//   - nothing: the pane shows only the dialog's bottom;
//   - the tail of one more label, cut by the top of the pane: the same;
//   - other rows, with the block beginning on a row of its own and holding the
//     dialog's affirmative label: the dialog's body, as far as the pane shows.
//     None of those rows may be a ❯ row, a rule or a footer, which is what
//     claudeFolderTrustHeadless asks of them: a launch dialog owns the pane
//     above its options.
//
// A ❯ row must begin an option. No row may be a plain rule, which is what a
// composer draws. And the footer must be the last content, so a composer or
// any other row below the dialog keeps the pane clear.
func claudeLaunchDialogTail(pane claudeLaunchDialogPane) (claudeLaunchDialogMatch, bool) {
	for _, line := range pane.lines {
		if claudeComposerRule(line) {
			return claudeLaunchDialogMatch{}, false
		}
	}
	end, legacyFooter := -1, false
	for i, footer := range claudeLaunchDialogFooters {
		if strings.HasSuffix(pane.text, footer) {
			end, legacyFooter = len(pane.text)-len(footer), i == 1
		}
	}
	if end <= 0 {
		return claudeLaunchDialogMatch{}, false
	}
	// The footer begins a row: it is the dialog's own row, not the end of a
	// sentence that happens to close with its words.
	footerRow, ok := pane.rowStart(end)
	if !ok {
		return claudeLaunchDialogMatch{}, false
	}

	at := end
	begins := map[int]bool{} // offsets where an option, or its ordinal, begins
	labels := 0
	var affirmative, proceed bool
	for at > 0 {
		matched := false
		for i, label := range claudeLaunchDialogLabels {
			if !strings.HasSuffix(pane.text[:at], label) {
				continue
			}
			at -= len(label)
			begins[at] = true
			// The label's ordinal, when a narrow pane left it on the row
			// above: "❯ 1." over "Yes, proceed".
			if n := claudeLaunchDialogOrdinalBefore(pane, at); n > 0 {
				at -= n
				begins[at] = true
			}
			affirmative = affirmative || i == 0 || i == 1
			proceed = proceed || i == 1
			labels++
			matched = true
			break
		}
		if !matched {
			break
		}
	}
	for content := 0; content < footerRow; content++ {
		if pane.rows[pane.index[content]].selected && !begins[pane.starts[content]] && pane.starts[content] >= at {
			return claudeLaunchDialogMatch{}, false
		}
	}
	for content := footerRow; content < len(pane.starts); content++ {
		if pane.rows[pane.index[content]].selected {
			return claudeLaunchDialogMatch{}, false
		}
	}

	match := claudeLaunchDialogMatch{kind: claudeLaunchDialogReworded}
	if proceed || (legacyFooter && !affirmative) {
		match.kind = claudeLaunchDialogLegacy
	}
	switch {
	case at == 0 && labels > 0:
		match.saw = "only its option rows and its footer"
		return match, true
	case at > 0 && claudeLaunchDialogLabelTail(pane.text[:at]):
		match.saw = "only the end of its option rows and its footer"
		return match, true
	case at > 0 && labels > 0 && affirmative:
		block, ok := pane.rowStart(at)
		if !ok {
			return claudeLaunchDialogMatch{}, false
		}
		for content := 0; content < block; content++ {
			i := pane.index[content]
			if pane.rows[i].selected || claudeTrustIsFooter(pane.rows[i].label) {
				return claudeLaunchDialogMatch{}, false
			}
		}
		for i := 0; i < pane.index[block]; i++ {
			if claudeTrustFrameRule(pane.lines[i]) {
				return claudeLaunchDialogMatch{}, false
			}
		}
		match.saw = "its option rows and its footer under rows af cannot read as the dialog's question"
		return match, true
	}
	return claudeLaunchDialogMatch{}, false
}

// claudeLaunchDialogOrdinalBefore returns the length of a picker ordinal ("1.")
// that ends at offset at and is a whole row, or 0.
func claudeLaunchDialogOrdinalBefore(pane claudeLaunchDialogPane, at int) int {
	content, ok := pane.rowStart(at)
	if !ok || content == 0 {
		return 0
	}
	above := pane.text[pane.starts[content-1]:at]
	if claudeMCPTrustOrdinalWord(above) {
		return len(above)
	}
	return 0
}

// claudeLaunchDialogLabelTail reports whether text is the end of one option
// label and not the whole of it: what the top of a pane leaves of a row it cut.
func claudeLaunchDialogLabelTail(text string) bool {
	for _, label := range claudeLaunchDialogLabels {
		if len(text) < len(label) && strings.HasSuffix(label, text) {
			return true
		}
	}
	return false
}

// claudeLegacyTrustPaintedMin is the least of the legacy question af reads as
// that dialog being painted: "Do you", its first row on the narrowest pane
// that shows two words. claudeLegacyTrustPaintedAlone is the least it reads
// that way on the words alone, "Do you trust": text a trust question begins
// with and little else does. Anything shorter is two common words, and holds
// only where the pane's width broke it (#7350).
const (
	claudeLegacyTrustPaintedMin   = len("Doyou")
	claudeLegacyTrustPaintedAlone = len("Doyoutrust")
)

// claudeLaunchDialogWrapsAt reports whether content rows first to the last
// could be the legacy question broken by a pane width columns wide. A renderer
// breaks a row only when the next word does not fit. So a row of the question
// that stops with room left for the next word is not a wrap at that width:
// "Do you" as the last row of an 80-column pane is the end of a sentence, and
// "Do you" on a 6-column pane is the question's first row.
//
// The width is the pane's own, read from tmux. Rows already on the pane are
// no evidence of it: after a resize, rows painted at the old width are still
// there, wider than anything the new width paints.
func claudeLaunchDialogWrapsAt(pane claudeLaunchDialogPane, first, width int) bool {
	at := 0 // offset into the question, spaces included
	question := claudeLegacyTrustQuestion
	for content := first; content < len(pane.starts); content++ {
		for range claudeLaunchDialogRowText(pane, content) {
			for at < len(question) && question[at] == ' ' {
				at++
			}
			at++
		}
		if at >= len(question) {
			return true
		}
		// What the renderer would have put next on this row: the rest of a
		// word it cut, or a space and the next word.
		rest := question[at:]
		need := 0
		if rest[0] == ' ' {
			rest, need = strings.TrimLeft(rest, " "), 1
		}
		if cut := strings.IndexByte(rest, ' '); cut >= 0 {
			rest = rest[:cut]
		}
		need += len(rest)
		painted := utf8.RuneCountInString(strings.TrimRightFunc(pane.lines[pane.index[content]], unicode.IsSpace))
		if painted+need <= width {
			return false
		}
	}
	return true
}

// claudePaneWidth is how the matcher asks for the pane's width, and only when
// it needs it. ok is false when the width could not be read; the matcher then
// holds, because an unread width is not evidence of a wide pane.
type claudePaneWidth func() (width int, ok bool)

// claudeLaunchDialogPainting reports the legacy folder-trust dialog while
// Claude Code is still painting it, on a pane too narrow for its question to
// fit a row. claudeLegacyTrustDialogOf reads that question only as one whole
// row, so under 38 columns every frame of the paint read clear.
//
// From some row down to the last content, the pane's text is the start of the
// question, or the whole question, wrapped onto two rows or more, and then
// only rows the dialog paints below it: body text, option rows, its closing
// frame, its footer. A ❯ row there must begin one of the dialog's options; any
// other, or a plain rule, is a composer, and the question is output being
// quoted. A question on one row is left to claudeLegacyTrustDialogOf, which
// reads it whole.
//
// Rows that are the later rows of a composer's draft are the composer's, not
// the dialog's (claudeMCPTrustDraftRow). And fewer words than "Do you trust"
// hold only where the pane's width broke them (claudeLaunchDialogWrapsAt).
func claudeLaunchDialogPainting(pane claudeLaunchDialogPane, paneWidth claudePaneWidth) (claudeLaunchDialogMatch, bool) {
	question := claudeLegacyTrustQuestionSquashed
	match := claudeLaunchDialogMatch{kind: claudeLaunchDialogLegacy}
	for first := len(pane.starts) - 1; first >= 0; first-- {
		if !strings.HasPrefix(question, claudeLaunchDialogRowText(pane, first)) ||
			pane.rows[pane.index[first]].selected {
			continue
		}
		text := pane.text[pane.starts[first]:]
		if len(text) < len(question) {
			// The question is still being painted.
			if len(text) >= claudeLegacyTrustPaintedMin && strings.HasPrefix(question, text) &&
				!claudeLaunchDialogAnySelected(pane, first, len(pane.starts)) {
				if len(text) < claudeLegacyTrustPaintedAlone {
					// Two common words: the question only where this pane's
					// width broke them.
					if width, ok := paneWidth(); ok && !claudeLaunchDialogWrapsAt(pane, first, width) {
						continue
					}
				}
				if claudeMCPTrustDraftRow(pane.lines, pane.rows, pane.index[first], pane.index[len(pane.index)-1]) {
					// The later rows of a composer's draft: the composer
					// explains them (#7344, #7350).
					continue
				}
				match.saw = "the start of its question, with nothing painted below it yet"
				if strings.HasPrefix(claudeMCPTrustQuestionSquashed, text) {
					match.kind = claudeLaunchDialogEither
					match.saw = "the first words of a trust question, which the legacy folder-trust dialog and the MCP-server dialog share"
				}
				return match, true
			}
			continue
		}
		if !strings.HasPrefix(text, question) {
			continue
		}
		below, ok := pane.rowStart(pane.starts[first] + len(question))
		if len(text) == len(question) {
			below, ok = len(pane.starts), true
		}
		if !ok || below-first < 2 || claudeLaunchDialogAnySelected(pane, first, below) {
			// The question ends inside a row, or sits on one row.
			continue
		}
		if claudeLaunchDialogComposerBelow(pane, below) ||
			claudeMCPTrustDraftRow(pane.lines, pane.rows, pane.index[first], pane.index[len(pane.index)-1]) {
			continue
		}
		match.saw = "its question wrapped onto several rows"
		return match, true
	}
	return claudeLaunchDialogMatch{}, false
}

func claudeLaunchDialogRowText(pane claudeLaunchDialogPane, content int) string {
	end := len(pane.text)
	if content+1 < len(pane.starts) {
		end = pane.starts[content+1]
	}
	return pane.text[pane.starts[content]:end]
}

func claudeLaunchDialogAnySelected(pane claudeLaunchDialogPane, from, to int) bool {
	for content := from; content < to; content++ {
		if pane.rows[pane.index[content]].selected {
			return true
		}
	}
	return false
}

// claudeLaunchDialogComposerBelow reports whether a row from content row from
// down shows a working agent rather than the legacy dialog: a plain rule, a ❯
// row that does not begin one of the dialog's options, or the boxed composer's
// "> " row.
func claudeLaunchDialogComposerBelow(pane claudeLaunchDialogPane, from int) bool {
	firstLine := len(pane.lines)
	if from < len(pane.index) {
		firstLine = pane.index[from]
	}
	for i := firstLine; i < len(pane.lines); i++ {
		if claudeComposerRule(pane.lines[i]) {
			return true
		}
	}
	for content := from; content < len(pane.starts); content++ {
		i := pane.index[content]
		row := pane.rows[i]
		if !row.selected {
			if i > 0 && claudeTrustFrameRule(pane.lines[i-1]) && (row.label == ">" || strings.HasPrefix(row.label, "> ")) {
				return true
			}
			continue
		}
		text := claudeLaunchDialogRowText(pane, content)
		option := claudeMCPTrustOrdinalWord(text)
		for _, label := range []string{claudeSquash(claudeLegacyTrustProceedLabel), claudeSquash(claudeFolderTrustExitLabel), "Yes", "No"} {
			option = option || (text != "" && strings.HasPrefix(label, text))
		}
		if !option {
			return true
		}
	}
	return false
}

// refuseClaudeLaunchDialog reports, once per dialog rather than once per poll,
// a folder-trust dialog the re-wrap matcher holds on, naming the dialog the
// pane shows and the part of it af saw.
func (t *TmuxSession) refuseClaudeLaunchDialog(match claudeLaunchDialogMatch) {
	if t.claudeTrust.refusalLogged {
		return
	}
	t.claudeTrust.refusalLogged = true
	log.ErrorLog.Printf(
		"refusing to answer the %s dialog for session %q: the pane shows %s (a pane too short scrolls the dialog's top off, and one too narrow wraps its rows); "+
			"af will not press a key it cannot aim at %s; make the pane larger, or answer the dialog in the session",
		match.name(), t.sanitizedName, match.saw, match.aim())
}

// claudeFolderTrustUnreadablePicker says which folder-trust dialog a pane
// claudeFolderTrustDialogOf reads as unreadable is showing, and why af refuses
// it, so the refusal line describes the pane and not the other dialog (#7346,
// R3). The legacy dialog reaches that verdict through
// claudeFolderTrustHeadless since #7343.
func claudeFolderTrustUnreadablePicker(screen visibleText) (trustPicker, error) {
	pane := claudeLaunchDialogPaneOf(screen)
	legacy := strings.Contains(pane.text, claudeLegacyTrustQuestionSquashed) ||
		strings.Contains(pane.text, claudeSquash(claudeLegacyTrustProceedLabel)) ||
		strings.HasSuffix(pane.text, claudeSquash(claudeLegacyTrustFooter))
	if !legacy || strings.Contains(pane.text, claudeSquash(claudeTrustAffirmativeLabel)) {
		return claudeFolderTrustPicker, claudeFolderTrustUnreadableWhy(screen)
	}
	picker := trustPicker{name: claudeLegacyFolderTrustDialogName, affirmative: claudeLegacyTrustProceedLabel}
	if strings.Contains(pane.text, claudeLegacyTrustQuestionSquashed) {
		return picker, fmt.Errorf("its question is on screen, but its rows do not read as a dialog af may answer: "+
			"the cursor is not on %q, or the pane is too narrow for its rows", claudeLegacyTrustProceedLabel)
	}
	return picker, errClaudeFolderTrustQuestionOffScreen
}
