package tmux

import (
	"fmt"
	"strings"
	"unicode"

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
// Claude Code 2.1.322's single-server MCP dialog is read the same two ways
// (#7364): its option block over its footer (claudeLaunchDialogTail, with its
// own labels), and its preamble opening the rows it paints, whole, framed or
// still being painted (claudeLaunchDialogMCPServerFromPreamble).
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
	// claudeLaunchDialogMCPServer: Claude Code 2.1.322's single-server MCP
	// dialog (claudeMCPServerDialogOf).
	claudeLaunchDialogMCPServer
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
	case claudeLaunchDialogMCPServer:
		return claudeMCPServerDialogName
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
	case claudeLaunchDialogMCPServer:
		// The only row af ever confirms on this dialog (declineClaudeMCPServer).
		return fmt.Sprintf("%q", claudeMCPServerDeclineLabel)
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

// claudeLaunchDialogOptions is one dialog's option labels and footers,
// squashed, as claudeLaunchDialogTail reads them up from the footer.
type claudeLaunchDialogOptions struct {
	labels  []string
	footers []string
	// read names the dialog that the labels read show, given which of them were
	// read (indices into labels) and which footer closed them. It also says
	// whether they are enough of the dialog's option block for rows above them
	// to be the dialog's body.
	read func(read []int, footer int) (kind claudeLaunchDialogKind, block bool)
}

// claudeLaunchDialogOptionSets are the dialogs claudeLaunchDialogTail reads, in
// order. The folder-trust dialogs come first, read exactly as before the MCP
// dialog joined them (#7364), so a pane they hold is held the same way.
var claudeLaunchDialogOptionSets = []claudeLaunchDialogOptions{
	{labels: claudeLaunchDialogLabels, footers: claudeLaunchDialogFooters, read: claudeFolderTrustOptionsRead},
	{labels: claudeMCPServerLabelsSquashed, footers: []string{claudeMCPServerFooterSquashed}, read: claudeMCPServerOptionsRead},
}

// claudeFolderTrustOptionsRead: the block counts once it holds an affirmative
// label, and "Yes, proceed" or the legacy footer alone names the legacy dialog.
func claudeFolderTrustOptionsRead(read []int, footer int) (claudeLaunchDialogKind, bool) {
	var affirmative, proceed bool
	for _, i := range read {
		affirmative = affirmative || i == 0 || i == 1
		proceed = proceed || i == 1
	}
	if proceed || (footer == 1 && !affirmative) {
		return claudeLaunchDialogLegacy, affirmative
	}
	return claudeLaunchDialogReworded, affirmative
}

// claudeMCPServerOptionsRead: the block counts only whole, with every one of
// the dialog's options. A pane that shows a row above the block shows all of
// the block, and the dialog always paints all three.
func claudeMCPServerOptionsRead(read []int, _ int) (claudeLaunchDialogKind, bool) {
	seen := map[int]bool{}
	for _, i := range read {
		seen[i] = true
	}
	return claudeLaunchDialogMCPServer, len(seen) == len(claudeMCPServerLabelsSquashed)
}

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

// claudeLaunchDialogRewrapOf reports a launch dialog the pane ends with a part
// of, as wrapped at any width: a folder-trust dialog, or the 2.1.322 MCP
// server dialog.
func claudeLaunchDialogRewrapOf(screen visibleText) (claudeLaunchDialogMatch, bool) {
	pane := claudeLaunchDialogPaneOf(screen)
	if len(pane.starts) == 0 {
		return claudeLaunchDialogMatch{}, false
	}
	for _, options := range claudeLaunchDialogOptionSets {
		if match, ok := claudeLaunchDialogTail(pane, options); ok {
			return match, true
		}
	}
	if match, ok := claudeLaunchDialogPainting(pane); ok {
		return match, true
	}
	return claudeLaunchDialogMCPServerFromPreamble(pane)
}

// claudeLaunchDialogTail reports a pane whose content ends with a launch
// dialog's option block and its whole footer. Reading up from the footer, the
// text is whole option labels, each with or without its ordinal. What is above
// them decides the rest:
//
//   - nothing: the pane shows only the dialog's bottom;
//   - the tail of one more label, cut by the top of the pane: the same;
//   - other rows, with the block beginning on a row of its own and being enough
//     of the dialog's options (claudeLaunchDialogOptions.read): the folder
//     dialog's affirmative label, or every option of the MCP server dialog.
//     Those rows are the dialog's body, as far as the pane shows. None of them
//     may be a ❯ row, a rule or a footer, which is what
//     claudeFolderTrustHeadless asks of them: a launch dialog owns the pane
//     above its options.
//
// A ❯ row must begin an option. No row may be a plain rule, which is what a
// composer draws. And the footer must be the last content, so a composer or
// any other row below the dialog keeps the pane clear.
func claudeLaunchDialogTail(pane claudeLaunchDialogPane, options claudeLaunchDialogOptions) (claudeLaunchDialogMatch, bool) {
	for _, line := range pane.lines {
		if claudeComposerRule(line) {
			return claudeLaunchDialogMatch{}, false
		}
	}
	end, footerIndex := -1, -1
	for i, footer := range options.footers {
		if strings.HasSuffix(pane.text, footer) {
			end, footerIndex = len(pane.text)-len(footer), i
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
	var read []int
	for at > 0 {
		matched := false
		for i, label := range options.labels {
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
			read = append(read, i)
			matched = true
			break
		}
		if !matched {
			break
		}
	}
	labels := len(read)
	kind, block := options.read(read, footerIndex)
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

	match := claudeLaunchDialogMatch{kind: kind}
	switch {
	case at == 0 && labels > 0:
		match.saw = "only its option rows and its footer"
		return match, true
	case at > 0 && claudeLaunchDialogLabelTail(pane.text[:at], options.labels):
		match.saw = "only the end of its option rows and its footer"
		return match, true
	case at > 0 && labels > 0 && block:
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
			if !claudeTrustFrameRule(pane.lines[i]) {
				continue
			}
			// Box corners and tees are a framed dialog's own edges (#7284
			// review R5), not the composer's, so a frame rule above the
			// options is the dialog's top border and must NOT reject: the
			// framed form must parse identically to the unframed one
			// (claudeTrustBoxDrawing). The one frame that grounds a
			// rejection here is a boxed COMPOSER's, recognised the same way
			// claudeLaunchDialogComposerBelow recognises it: a frame-rule
			// line adjacent to a row reading ">" or "> ".
			if claudeLaunchDialogPromptRow(pane, i+1) || claudeLaunchDialogPromptRow(pane, i-1) {
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
func claudeLaunchDialogLabelTail(text string, labels []string) bool {
	for _, label := range labels {
		if len(text) < len(label) && strings.HasSuffix(label, text) {
			return true
		}
	}
	return false
}

// claudeLegacyTrustPaintedMin is the least of the legacy question af reads as
// that dialog being painted: "Do you", its first row on the narrowest pane
// that shows two words.
const claudeLegacyTrustPaintedMin = len("Doyou")

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
// the dialog's (claudeMCPTrustDraftRow, #7350).
//
// "Do you" alone holds, two common words though they are: on a narrow pane
// they are the whole of the question's first row, and af does not know the
// width Claude Code wraps this dialog at. It is narrower than the pane by a
// margin no capture of this dialog records (#7351 review).
func claudeLaunchDialogPainting(pane claudeLaunchDialogPane) (claudeLaunchDialogMatch, bool) {
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
			if i > 0 && claudeTrustFrameRule(pane.lines[i-1]) && claudeLaunchDialogPromptRow(pane, i) {
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

// claudeLaunchDialogPromptRow reports whether pane line i is a boxed composer's
// prompt row: the ">" or "> " row Claude Code paints inside its framed composer.
// claudeTrustRowOf reduces "│ > … │" to "> …", and a selected option row is
// never the composer's prompt. Used to tell a boxed composer's frame edge from
// a framed dialog's own top border (#7284 review R5): the composer's edge is
// adjacent to such a row; the dialog's is adjacent to its question.
func claudeLaunchDialogPromptRow(pane claudeLaunchDialogPane, i int) bool {
	return i >= 0 && i < len(pane.lines) && !pane.rows[i].selected &&
		(pane.rows[i].label == ">" || strings.HasPrefix(pane.rows[i].label, "> "))
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
