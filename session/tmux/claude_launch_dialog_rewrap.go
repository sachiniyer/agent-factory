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
//     is the dialog scrolled off the top, at any width (claudeLaunchDialogTail),
//     and one that ends with the block and the start of the footer is the
//     same, with the footer still being painted
//     (claudeLaunchDialogFooterPainting, #7390);
//   - the same block read again, with a bare ❯ at the block's offset as a row
//     above the block, not its first row: an empty composer straight above
//     the options, under the dialog's head (claudeLaunchDialogPromptOverOptions,
//     #7453);
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
	// pair: the option block's affirmative was read through the legacy
	// dialog's single-row inline picker, so the row the pane shows is "Yes"
	// rather than the stacked form's "Yes, proceed" (#7661 review, R4).
	pair bool
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
		if m.pair {
			// The inline pair's affirmative is the row's "Yes", not the
			// stacked form's "Yes, proceed": name the row the pane shows
			// (#7661 review, R4).
			return fmt.Sprintf("%q", claudeLegacyInlineYesLabel)
		}
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
//
// The claudeLaunchDialogLabel constants name the slice indices
// claudeFolderTrustOptionsRead classifies a block by, so the classifier does
// not read its own slice layout by raw number.
const (
	claudeLaunchDialogLabelAffirmative = iota
	claudeLaunchDialogLabelLegacyProceed
	claudeLaunchDialogLabelExit
	// claudeLaunchDialogLabelInlineYesNo: the legacy dialog's single-row inline
	// picker "Yes  No" (claudeLegacyInlinePairText), squashed "YesNo". The
	// inline pair IS the legacy dialog's affirmative — the cursor sits on its
	// Yes — and a pane too narrow for the question to fit one row leaves the
	// pair on its own row above the footer, so the re-wrap tail reader has to
	// consume it as the legacy affirmative (#7349's gap, fixed alongside
	// #7584's deferral).
	claudeLaunchDialogLabelInlineYesNo
	// claudeLaunchDialogLabelInlineYesNoOrdinal: the same pair with the No
	// option's ordinal, "Yes  2. No" (claudeLegacyInlineOrdinalPairText),
	// squashed "Yes2.No". trimClaudeTrustOrdinal drops only the LEADING "1.",
	// so the mid-row "2." stays in the label.
	claudeLaunchDialogLabelInlineYesNoOrdinal
)

var (
	claudeLaunchDialogLabels = []string{
		claudeSquash(claudeTrustAffirmativeLabel),
		claudeSquash(claudeLegacyTrustProceedLabel),
		claudeSquash(claudeFolderTrustExitLabel),
		claudeSquash(claudeLegacyInlinePairText),
		claudeSquash(claudeLegacyInlineOrdinalPairText),
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
	labels []string
	// pair is how many of labels, counted from the end, are the dialog's
	// single-row inline option rows ("Yes  No" / "Yes  2. No"). Each of those
	// IS the dialog's whole option block — the cursor's ❯ sits on it and
	// nothing the dialog paints stacks above or below it — so the reader
	// accepts one only as the block's lowest label, directly over the footer.
	// Read as one more label on top of a block that already has its
	// affirmative, the block grows upward into rows af must keep holding as
	// the dialog's body — the reading replaced master's, which is the one
	// thing a re-wrap reading may never do (#7453; #7661 review, R1). For the
	// same reason the top of a pane never cuts one.
	pair    int
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
	{labels: claudeLaunchDialogLabels, pair: 2, footers: claudeLaunchDialogFooters, read: claudeFolderTrustOptionsRead},
	{labels: claudeMCPServerLabelsSquashed, footers: []string{claudeMCPServerFooterSquashed}, read: claudeMCPServerOptionsRead},
}

// claudeFolderTrustOptionsRead: the block counts once it holds an affirmative
// label, and "Yes, proceed", the inline pair, or the legacy footer alone names
// the legacy dialog.
//
// The legacy dialog paints its affirmative either as the stacked "Yes, proceed"
// row or the single-row inline picker "Yes  No" / "Yes  2. No"
// (claudeLegacyInlinePair, claudeLegacyInlinePairText). Either is the
// legacy affirmative: read-once labels for both produce the same
// (claudeLaunchDialogLegacy, true) verdict a stacked "Yes, proceed" does, so
// the re-wrap tail reader holds the picker over the footer at any pane width
// the way it holds the stacked form (#7349's gap, fixed alongside #7584's
// deferral).
func claudeFolderTrustOptionsRead(read []int, footer int) (claudeLaunchDialogKind, bool) {
	var affirmative, proceed bool
	for _, i := range read {
		switch i {
		case claudeLaunchDialogLabelAffirmative:
			affirmative = true
		case claudeLaunchDialogLabelLegacyProceed,
			claudeLaunchDialogLabelInlineYesNo,
			claudeLaunchDialogLabelInlineYesNoOrdinal:
			// The legacy affirmative: stacked "Yes, proceed" or the inline pair.
			affirmative, proceed = true, true
		}
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
	// blockOnLabel: the option block begins on the row its first label, or
	// that label's ordinal, is painted on, and a bare ❯ above it at the same
	// offset is a row above the block (claudeLaunchDialogPromptOverOptions).
	blockOnLabel bool
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

// blockRow returns the content row an option block that begins at offset at
// begins on. A bare ❯ row has no text, so it begins at the same offset as the
// row below it. Read as the block's first row, it is the dialog's own cursor,
// which a narrow pane leaves on a row of its own above "1. Yes, …". With
// blockOnLabel it is a row above the block (#7453).
func (p claudeLaunchDialogPane) blockRow(at int) (content int, ok bool) {
	content, ok = p.rowStart(at)
	for ok && p.blockOnLabel && content+1 < len(p.starts) && p.starts[content+1] == at {
		content++
	}
	return content, ok
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
	if match, ok := claudeLaunchDialogMCPServerFromPreamble(pane); ok {
		return match, true
	}
	// Asked last, so every pane a reader above holds is held as before.
	for _, options := range claudeLaunchDialogOptionSets {
		if match, ok := claudeLaunchDialogFooterPainting(pane, options); ok {
			return match, true
		}
	}
	// Last of all, for the same reason (#7453).
	return claudeLaunchDialogPromptOverOptions(pane)
}

// claudeLaunchDialogPromptOverOptions reads the option block again, over its
// footer or the start of it, with any bare ❯ at the block's offset read as a
// row above the block (#7453). The 2.1.322 composer paints its empty prompt
// as a ❯ alone between its plain rules. Straight above the options, that ❯
// begins at the block's offset, so claudeLaunchDialogTail read it as the
// block's first row, and the composer's closing rule under it as a composer
// below the options: a dialog's head, then an empty composer, then the
// dialog's options and footer read clear. The same composer with a draft in
// it held (#7412 round-3 review, R7).
//
// Read this way, the ❯ is a prompt above the options, like any ❯ row there: it
// keeps the pane clear only when it explains every row on screen outside it
// (#7388). The first reading is not replaced: a narrow pane's own cursor row
// still reads as the block's first row, and a pane either reading holds is
// held.
func claudeLaunchDialogPromptOverOptions(pane claudeLaunchDialogPane) (claudeLaunchDialogMatch, bool) {
	shared := false
	for i := 1; i < len(pane.starts) && !shared; i++ {
		shared = pane.starts[i] == pane.starts[i-1]
	}
	if !shared {
		// No bare ❯ over another row: both readings are the same.
		return claudeLaunchDialogMatch{}, false
	}
	pane.blockOnLabel = true
	for _, options := range claudeLaunchDialogOptionSets {
		if match, ok := claudeLaunchDialogTail(pane, options); ok {
			return match, true
		}
		if match, ok := claudeLaunchDialogFooterPainting(pane, options); ok {
			return match, true
		}
	}
	return claudeLaunchDialogMatch{}, false
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
//     Those rows are the dialog's body, as far as the pane shows. A ❯ row or
//     a composer among them keeps the pane clear only when it explains every
//     row on screen outside it (#7388). Nothing else does: another dialog's
//     footer among them is a row af cannot place, like any other. It says
//     an older dialog was on screen and nothing about the one below it,
//     which a relaunch in the same pane paints under the old process's last
//     frame (#7452).
//
// A ❯ row must begin an option. A plain rule, which is what a composer draws
// above and below its prompt row, keeps the pane clear from the options down.
// Above them it is the dialog's own top rule, which Claude Code 2.1.322 paints
// over its launch dialogs (testdata/claude_2_1_322_mcp_server_dialog.ansi),
// unless it is a ruled composer's (#7380). And the footer must be the last
// content, so a composer or any other row below the dialog keeps the pane
// clear.
func claudeLaunchDialogTail(pane claudeLaunchDialogPane, options claudeLaunchDialogOptions) (claudeLaunchDialogMatch, bool) {
	end, footerIndex := -1, -1
	for i, footer := range options.footers {
		if strings.HasSuffix(pane.text, footer) {
			end, footerIndex = len(pane.text)-len(footer), i
		}
	}
	return claudeLaunchDialogTailOver(pane, options, end, footerIndex, "its footer")
}

// claudeLaunchDialogFooterPainting reports a pane whose content ends with a
// launch dialog's option block and the start of its footer, read exactly as
// claudeLaunchDialogTail reads the block over the whole footer (#7390). A
// frame caught while Claude Code paints the footer, on a pane too short for
// the dialog's top, shows whole option labels over "Enter to confirm ·"; the
// same rows over the whole footer held, and these read clear.
//
// The start is one or more whole rows, the last content in the pane, whose
// text is the start of one of the dialog's footers and not the whole of it.
// Each content row a footer could start on is tried, from the bottom up: a
// one-column pane paints a row "E" for "Esc" as well as for "Enter".
func claudeLaunchDialogFooterPainting(pane claudeLaunchDialogPane, options claudeLaunchDialogOptions) (claudeLaunchDialogMatch, bool) {
	for content := len(pane.starts) - 1; content > 0; content-- {
		start := pane.text[pane.starts[content]:]
		for i, footer := range options.footers {
			if start == "" || len(start) >= len(footer) || !strings.HasPrefix(footer, start) {
				continue
			}
			if match, ok := claudeLaunchDialogTailOver(pane, options, pane.starts[content], i,
				"the start of its footer, still being painted"); ok {
				return match, true
			}
			break
		}
	}
	return claudeLaunchDialogMatch{}, false
}

// claudeLaunchDialogTailOver reads the option block up from offset end, where
// the dialog's footers[footerIndex], or as much of it as shown names, begins
// and runs to the end of the pane's text: the rest of claudeLaunchDialogTail.
func claudeLaunchDialogTailOver(pane claudeLaunchDialogPane, options claudeLaunchDialogOptions, end, footerIndex int, shown string) (claudeLaunchDialogMatch, bool) {
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
			if len(read) > 0 && i >= len(options.labels)-options.pair {
				// An inline pair is the dialog's whole option block, not
				// one more label on top of it: accepted only directly over
				// the footer, so the block never grows upward into rows
				// the stacked labels leave for the dialog's body (#7661
				// review, R1).
				continue
			}
			if !strings.HasSuffix(pane.text[:at], label) {
				continue
			}
			if i >= len(options.labels)-options.pair && claudeLaunchDialogBoxedSpan(pane, at-len(label), at) {
				// A pair drafted inside a composer's box is the draft's
				// text, not the dialog's option row: the box's side borders
				// survive on the raw rows even though claudeTrustRowOf reads
				// past them, and no folder-trust dialog ever paints between
				// them (#7661 Codex review).
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
	// A pair label can only be the block's lowest, read first.
	match.pair = len(read) > 0 && read[0] >= len(options.labels)-options.pair
	switch {
	case at == 0 && labels > 0:
		if claudeLaunchDialogRuleFrom(pane, pane.index[0]) {
			return claudeLaunchDialogMatch{}, false
		}
		match.saw = "only its option rows and " + shown
		return match, true
	case at > 0 && claudeLaunchDialogLabelTail(pane.text[:at], options.labels[:len(options.labels)-options.pair]):
		if claudeLaunchDialogRuleFrom(pane, pane.index[0]) {
			return claudeLaunchDialogMatch{}, false
		}
		match.saw = "only the end of its option rows and " + shown
		return match, true
	case at > 0 && labels > 0 && block:
		block, ok := pane.blockRow(at)
		if !ok || claudeLaunchDialogRuleFrom(pane, pane.index[block]) {
			return claudeLaunchDialogMatch{}, false
		}
		for content := 0; content < block; content++ {
			i := pane.index[content]
			// A ❯ row above the options is a prompt: the 2.1.322
			// composer's, between its plain rules, or a shell's
			// ("❯ claude"). It explains only its own rows, so it decides
			// only when it explains every row on screen outside it
			// (#7388). A footer above the options is another dialog's,
			// and no evidence about this one, so it is read as any row
			// af cannot place (#7452).
			if pane.rows[i].selected && claudeLaunchDialogPromptExplains(pane, i, pane.index[block]) {
				return claudeLaunchDialogMatch{}, false
			}
		}
		for i := 0; i < pane.index[block]; i++ {
			if !claudeTrustFrameRule(pane.lines[i]) {
				continue
			}
			if claudeComposerRule(pane.lines[i]) {
				// A plain rule above the options is the dialog's own top
				// rule (#7380), unless it is a ruled composer's: it bounds
				// an unbordered prompt row in any of the composer's modes,
				// straight below it (its opening rule) or straight above it
				// (its closing rule, its top cropped). Even then the
				// composer decides only when it explains every row on
				// screen outside it (#7388). A bordered prompt row is a
				// box's, so the rule under it is not the composer's: the
				// box test reads it as the dialog's own top rule (#7412
				// review, R1). A ❯ prompt row is read above.
				for _, prompt := range []int{i + 1, i - 1} {
					if claudeLaunchDialogModePromptRow(pane, prompt) && !claudeTrustBordered(pane.lines[prompt]) &&
						claudeLaunchDialogPromptExplains(pane, prompt, pane.index[block]) {
						return claudeLaunchDialogMatch{}, false
					}
				}
				continue
			}
			// Box corners and tees are a framed dialog's own edges (#7284
			// review R5), not the composer's, so a frame rule above the
			// options is the dialog's top border and must NOT reject: the
			// framed form must parse identically to the unframed one
			// (claudeTrustBoxDrawing). The one box that grounds a rejection
			// here is a composer's, and a box is a composer by what is
			// inside it (claudeTrustBoxPromptRow, #7380). Rows outside the
			// box decide nothing, so a shell's "> claude" line above a live
			// framed dialog does not make it a composer. And a composer
			// explains only its own rows: one with the dialog's own first
			// rows on screen below it is a stale frame above a live dialog
			// (#7388), and decides nothing. Nor can it explain a row above
			// its box: af cannot tell the dialog's question above a stale
			// composer from transcript, so the box decides only when every
			// row above it is one af can place (#7412 review, R7).
			if prompt := claudeTrustBoxPromptRow(pane.lines, pane.rows, i); prompt >= 0 &&
				!claudeLaunchDialogOwnRowsAbove(pane, i) &&
				!claudeLaunchDialogOwnRowsBetween(pane, prompt, pane.index[block]) {
				return claudeLaunchDialogMatch{}, false
			}
			// A top-cropped boxed composer: its opening rule scrolled off the
			// top, so its prompt row sits straight above the composer's closing
			// edge — a bottom-cornered frame (╰…╯ closes; ╭…╮ opens). A bare
			// transcript ">" row sits above the framed dialog's own top border
			// (╭…╮), never above a ╰…╯ closing rule (#7399 review). It explains
			// only its own rows too, under the same #7388 anchor as the box
			// test (#7412 review, R4). And it is top-cropped only when its top
			// is off screen: every line above its prompt row is one of the
			// box's own pad rows (claudeTrustBoxTopCropped, #7412 review, R6).
			if claudeLaunchDialogPromptRow(pane, i-1) && claudeTrustFrameClosingRule(pane.lines[i]) &&
				claudeTrustBoxTopCropped(pane, i-1) &&
				!claudeLaunchDialogOwnRowsBetween(pane, i-1, pane.index[block]) {
				return claudeLaunchDialogMatch{}, false
			}
		}
		match.saw = "its option rows and " + shown + " under rows af cannot read as the dialog's question"
		return match, true
	}
	return claudeLaunchDialogMatch{}, false
}

// claudeTrustBoxTopCorners open a box: a frame rule that starts with one is a
// box's top edge.
const claudeTrustBoxTopCorners = "╭┌╔┏"

// claudeTrustBoxPromptRow returns the line of a composer box's prompt row,
// given its top edge, or -1 when the box is no composer. A box is a composer by
// what is inside it: its first row inside the box, past any pad rows, is a
// prompt in one of the composer's modes (claudeComposerPromptLabel). A bottom
// edge or a tee decides nothing on its own, and neither does a box whose top
// edge is off screen: what is inside a box is below its top edge, between its
// side borders.
func claudeTrustBoxPromptRow(lines []string, rows []claudeTrustRow, top int) int {
	if !claudeTrustBoxOpensAt(strings.TrimSpace(lines[top])) {
		return -1
	}
	for i := top + 1; i < len(lines); i++ {
		if claudeTrustFrameRule(lines[i]) || !claudeTrustBordered(lines[i]) {
			// The box closed, another edge began, or a row outside any box
			// came, before the box's first row.
			return -1
		}
		if rows[i].blank {
			continue
		}
		if !rows[i].selected && claudeComposerPromptLabel(rows[i].label) {
			return i
		}
		return -1
	}
	return -1
}

// claudeTrustBordered reports whether line carries a box's side borders.
func claudeTrustBordered(line string) bool {
	line = strings.TrimSpace(line)
	first, _ := utf8.DecodeRuneInString(line)
	last, _ := utf8.DecodeLastRuneInString(line)
	return strings.ContainsRune(claudeTrustBoxSides, first) && strings.ContainsRune(claudeTrustBoxSides, last)
}

// claudeTrustBoxBottomCorners close a box: a frame rule that starts with one
// is a box's bottom edge.
const claudeTrustBoxBottomCorners = "╰└╚┗"

// claudeTrustBoxCorners are every corner a box edge can carry. An edge with
// none on it, a tee, a junction or a rule, neither opens nor closes a box.
const claudeTrustBoxCorners = claudeTrustBoxTopCorners + "╮┐╗┓" + claudeTrustBoxBottomCorners + "╯┘╝┛"

// claudeLaunchDialogOwnRowsBetween reports whether the lines strictly between
// from, a composer's prompt row, and to, where the options begin, may hold one
// of the dialog's own first rows (#7388). Every line below the prompt is read,
// the box's own rows below it too: only a bottom edge closes a box, and a
// bottom edge, the composer's or any other, is read as no row at all (#7412
// review, R1). A composer decides only over rows af can place as quoted
// (#7412 review, R3), so any of these is one of the dialog's own first rows:
//
//   - the top edge of a box that is still open at the options;
//   - an edge with no corner on it: a tee, a junction, or a rule in a style
//     other than the composer's ─. It neither opens nor closes a box, so it is
//     an edge inside the dialog's frame or the dialog's own rule (Codex review
//     on #7412);
//   - the dialog's own top rule: a plain rule bounding no prompt row between
//     the composer and the options;
//   - a row af cannot place: any row with text on it. The rows af can place
//     are none at all and the bare options, so a blank row, a bordered blank
//     row or a lone wrapped ❯. Any other row may be the dialog's question in
//     wording af does not know, a title such as "Accessing workspace:" among
//     them. A row that reads as the end of a question af knows is no evidence
//     that the composer hides its start, so it counts too (#7412 review, R5).
func claudeLaunchDialogOwnRowsBetween(pane claudeLaunchDialogPane, from, to int) bool {
	for i := from + 1; i < to; i++ {
		line := pane.lines[i]
		switch {
		case claudeComposerRule(line):
			if !claudeLaunchDialogBoundsPrompt(pane, i, from, to) {
				return true
			}
		case claudeTrustFrameRule(line):
			edge := strings.TrimSpace(line)
			if claudeTrustBoxOpensAt(edge) && claudeTrustBoxOpenUntil(pane.lines, i, to) || !strings.ContainsAny(edge, claudeTrustBoxCorners) {
				return true
			}
		case claudeLaunchDialogUnplacedRow(pane, i):
			return true
		}
	}
	return false
}

// claudeLaunchDialogOwnRowsAbove reports whether any line above line top, a
// composer box's top edge, may hold one of the dialog's own first rows. A
// composer explains only the rows below it, so a row above its box that af
// cannot place may be the dialog's question with a stale composer between it
// and the options (#7412 review, R7). The lines af can place above a box are
// blank rows and frame rules.
func claudeLaunchDialogOwnRowsAbove(pane claudeLaunchDialogPane, top int) bool {
	for i := 0; i < top; i++ {
		if !claudeTrustFrameRule(pane.lines[i]) && claudeLaunchDialogUnplacedRow(pane, i) {
			return true
		}
	}
	return false
}

// claudeLaunchDialogPromptExplains reports whether the composer whose prompt is
// pane line prompt, a ❯ row or a ruled composer's prompt row, explains every
// row on screen outside it (#7388): nothing af cannot place between its last
// own row and to, where the options begin, and nothing above its first own row
// (#7412 review, R7, as the box test reads it). An unbordered prompt row's own
// rows are the plain rules welded straight above and straight under it, which
// 2.1.322 paints around its composer's ❯ row.
func claudeLaunchDialogPromptExplains(pane claudeLaunchDialogPane, prompt, to int) bool {
	top, end := prompt, prompt
	if !claudeTrustBordered(pane.lines[prompt]) {
		if prompt > 0 && claudeComposerRule(pane.lines[prompt-1]) {
			top = prompt - 1
		}
		if prompt+1 < len(pane.lines) && claudeComposerRule(pane.lines[prompt+1]) {
			end = prompt + 1
		}
	}
	return !claudeLaunchDialogOwnRowsAbove(pane, top) && !claudeLaunchDialogOwnRowsBetween(pane, end, to)
}

// claudeLaunchDialogUnplacedRow reports whether pane line i is a row af cannot
// place: any row with text on it. A blank row, a bordered blank row and a lone
// wrapped ❯ are rows af can place.
func claudeLaunchDialogUnplacedRow(pane claudeLaunchDialogPane, i int) bool {
	return !pane.rows[i].blank && claudeSquash(pane.rows[i].label) != ""
}

// claudeTrustBoxTopCropped reports whether the box whose prompt row is line
// prompt has its top edge off screen: every line above the prompt row is one
// of the box's own pad rows, bordered and blank. Anything else above it means
// the box's top is on screen, and the box test reads the box by its first row,
// or the row is no composer's prompt (#7412 review, R6).
func claudeTrustBoxTopCropped(pane claudeLaunchDialogPane, prompt int) bool {
	for i := 0; i < prompt; i++ {
		if !claudeTrustBordered(pane.lines[i]) || !pane.rows[i].blank {
			return false
		}
	}
	return true
}

// claudeTrustBoxOpensAt reports whether edge, a trimmed frame rule, is a box's
// top edge.
func claudeTrustBoxOpensAt(edge string) bool {
	first, _ := utf8.DecodeRuneInString(edge)
	return strings.ContainsRune(claudeTrustBoxTopCorners, first)
}

// claudeTrustBoxOpenUntil reports whether the box whose top edge is line top
// has no bottom edge before line to.
func claudeTrustBoxOpenUntil(lines []string, top, to int) bool {
	for i := top + 1; i < to; i++ {
		first, _ := utf8.DecodeRuneInString(strings.TrimSpace(lines[i]))
		if claudeTrustFrameRule(lines[i]) && strings.ContainsRune(claudeTrustBoxBottomCorners, first) {
			return false
		}
	}
	return true
}

// claudeLaunchDialogBoundsPrompt reports whether the plain rule on line i
// bounds a composer's prompt row strictly between lines from and to: a ❯ row,
// or a prompt in any of the composer's modes, straight above or below it. The
// composer above, at from or higher, explains only its own rows, so its prompt
// does not make a rule straight under it a composer's (#7412 review, R1).
func claudeLaunchDialogBoundsPrompt(pane claudeLaunchDialogPane, i, from, to int) bool {
	for _, j := range []int{i - 1, i + 1} {
		if j > from && j < to && (pane.rows[j].selected || claudeLaunchDialogModePromptRow(pane, j)) {
			return true
		}
	}
	return false
}

// claudeLaunchDialogModePromptRow reports whether pane line i is a composer's
// prompt row in one of its modes (claudeComposerPromptLabel).
func claudeLaunchDialogModePromptRow(pane claudeLaunchDialogPane, i int) bool {
	return i >= 0 && i < len(pane.lines) && !pane.rows[i].selected && claudeComposerPromptLabel(pane.rows[i].label)
}

// claudeComposerPromptLabel reports a row Claude Code's composer paints as its
// prompt, boxed or between plain rules: the glyph of one of its modes, ">"
// (prompt), "!" (bash) or "#" (memory), followed by nothing or by the space
// the composer paints after it, U+0020 or U+00A0. 2.1.322 paints U+00A0 after
// its glyph (testdata/claude_2_1_322_composer_after_mcp_decline.ansi, #7391
// review). The box test read U+00A0 since #7412, and the plain-rule test only
// U+0020, so a ruled composer in 2.1.322's own spacing straight over bare
// quoted options held where its ASCII twin cleared. Both read it now, under
// #7388's anchor: a composer decides only when it explains every row on
// screen outside it (#7454).
func claudeComposerPromptLabel(label string) bool {
	for _, mode := range []string{">", "!", "#"} {
		rest, ok := strings.CutPrefix(label, mode)
		if !ok {
			continue
		}
		if after, _ := utf8.DecodeRuneInString(rest); rest == "" || after == ' ' || after == '\u00a0' {
			return true
		}
	}
	return false
}

// claudeLaunchDialogRuleFrom reports a plain rule on line from or below it,
// where only a composer draws one.
func claudeLaunchDialogRuleFrom(pane claudeLaunchDialogPane, from int) bool {
	for i := from; i < len(pane.lines); i++ {
		if claudeComposerRule(pane.lines[i]) {
			return true
		}
	}
	return false
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
		if claudeLaunchDialogComposerBelow(pane, first, below) ||
			claudeMCPTrustDraftRow(pane.lines, pane.rows, pane.index[first], pane.index[len(pane.index)-1]) {
			continue
		}
		match.saw = "its question wrapped onto several rows"
		for i := pane.index[first]; i < len(pane.lines); i++ {
			if claudeLegacyInlinePair(strings.Fields(strings.ToLower(pane.rows[i].label))) {
				// The dialog's option row is the inline pair: name its "Yes",
				// not the stacked form's "Yes, proceed" (#7661 review, R4).
				match.pair = true
			}
		}
		return match, true
	}
	return claudeLaunchDialogMatch{}, false
}

// claudeLaunchDialogBoxedSpan reports whether the text span [from, to) sits on
// a row inside a box that proves itself a composer's (claudeTrustComposerBoxHas):
// a bordered row alone is not enough — a dialog's own frame has the same side
// borders (#7666).
func claudeLaunchDialogBoxedSpan(pane claudeLaunchDialogPane, from, to int) bool {
	for content := 0; content < len(pane.starts); content++ {
		start := pane.starts[content]
		end := len(pane.text)
		if content+1 < len(pane.starts) {
			end = pane.starts[content+1]
		}
		line := pane.index[content]
		if start < to && end > from && claudeTrustBordered(pane.lines[line]) &&
			claudeTrustComposerBoxHas(pane.lines, pane.rows, line) {
			return true
		}
	}
	return false
}

// claudeTrustComposerBoxHas reports whether line i sits inside a box that
// proves itself a composer's: the side borders over it reach a top edge whose
// first content row inside is a composer prompt row (claudeTrustBoxPromptRow).
// A box that cannot prove that — a dialog's own frame, or one whose top
// scrolled off — is not a composer's, and rows it holds still read as the
// dialog's (#7666).
func claudeTrustComposerBoxHas(lines []string, rows []claudeTrustRow, i int) bool {
	for top := i - 1; top >= 0; top-- {
		if !claudeTrustBordered(lines[top]) {
			return claudeTrustBoxPromptRow(lines, rows, top) >= 0
		}
	}
	return false
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
