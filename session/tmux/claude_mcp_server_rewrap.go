package tmux

import "strings"

// claudeMCPServerFooter is the 2.1.322 MCP server dialog's footer in full. It
// is the reworded folder-trust dialog's footer word for word
// (testdata/claude_2_1_322_mcp_server_dialog.ansi).
const claudeMCPServerFooter = "Enter to confirm · Esc to cancel"

// The 2.1.322 MCP server dialog's text, squashed (claudeSquash), as the re-wrap
// matcher reads it (#7364).
var (
	claudeMCPServerPreambleSquashed = claudeSquash(claudeMCPServerPreamble)
	// claudeMCPTrustPreambleSquashed is the old MCP modal's preamble, "New MCP
	// server found.". Text that is the start of both preambles could be
	// either dialog being painted.
	claudeMCPTrustPreambleSquashed = claudeSquash(claudeMCPServerHoldAnchor) + "."
	claudeMCPServerLabelsSquashed  = []string{
		claudeSquash(claudeMCPServerUseLabel),
		claudeSquash(claudeMCPServerUseAllLabel),
		claudeSquash(claudeMCPServerDeclineLabel),
	}
	claudeMCPServerFooterSquashed      = claudeSquash(claudeMCPServerFooter)
	claudeMCPServerFooterStartSquashed = claudeSquash(claudeTrustAffordancePrefix)
)

// claudeLaunchDialogMCPServerFromPreamble reports the 2.1.322 MCP server dialog
// from its preamble down, at any width: whole, framed, or while Claude Code is
// still painting it. claudeMCPServerDialogOf reads the preamble only as one
// whole row, and claudeMCPServerUnreadableOf reads the footer only as the
// pane's last whole row. So under 36 columns, where the footer wraps, the whole
// dialog read clear, and the create typed its prompt into it. Its Enter then
// confirmed whichever row was preselected, which can be "Use this and all
// future MCP servers in this project" (#7364).
//
// From some row down to the last content, the pane's text is the dialog's, in
// the dialog's order:
//
//	preamble  a row that begins "New MCP server found in this project:", or
//	          as much of it as is painted, from its first word ("New", the
//	          least claudeMCPTrustPaintingOf reads of the old modal too)
//	body      the server's name and the text below it: no ❯ row, no footer
//	options   whole option labels, each beginning a row, each at most once
//	end       nothing, the start of one more option (as little as its ❯,
//	          #7390), or the footer or the start of it, beginning a row
//
// A ❯ row must begin one of the options, and there is at most one. No line from
// the preamble down may be a plain rule or a boxed composer's "> " row. A
// working agent paints its composer below its output, so a dialog it only
// quotes has one under it. Rows above the preamble decide nothing: the dialog
// paints its own top rule there.
//
// Rows that are the later rows of a composer's draft are the composer's, not
// the dialog's (claudeMCPTrustDraftRow, #7344, #7350).
func claudeLaunchDialogMCPServerFromPreamble(pane claudeLaunchDialogPane) (claudeLaunchDialogMatch, bool) {
	preamble := claudeMCPServerPreambleSquashed
	for first := len(pane.starts) - 1; first >= 0; first-- {
		row := claudeLaunchDialogRowText(pane, first)
		text := pane.text[pane.starts[first]:]
		if pane.rows[pane.index[first]].selected || len(text) < len(claudeMCPTrustFirstRowAtSixColumns) ||
			!(strings.HasPrefix(preamble, row) || strings.HasPrefix(row, preamble)) ||
			!(strings.HasPrefix(preamble, text) || strings.HasPrefix(text, preamble)) {
			continue
		}
		saw, ok := claudeMCPServerPaintedFrom(pane, first)
		if !ok || claudeMCPTrustDraftRow(pane.lines, pane.rows, pane.index[first], pane.index[len(pane.index)-1]) {
			continue
		}
		if strings.HasPrefix(claudeMCPTrustPreambleSquashed, text) {
			// The start of the old MCP modal's preamble too, so af cannot say
			// which of the two is being painted.
			return claudeLaunchDialogMatch{kind: claudeLaunchDialogEither,
				saw: "the first words of an MCP server dialog's preamble, which the 2.1.322 dialog and the older MCP modal share"}, true
		}
		return claudeLaunchDialogMatch{kind: claudeLaunchDialogMCPServer, saw: saw}, true
	}
	return claudeLaunchDialogMatch{}, false
}

// claudeMCPServerPaintedFrom reports whether every row from content row first,
// where the preamble begins, down to the last content is one the 2.1.322 MCP
// server dialog paints, and says which part of the dialog they show.
func claudeMCPServerPaintedFrom(pane claudeLaunchDialogPane, first int) (string, bool) {
	for i := pane.index[first]; i < len(pane.lines); i++ {
		if claudeComposerRule(pane.lines[i]) ||
			(i > 0 && claudeTrustFrameRule(pane.lines[i-1]) && claudeLaunchDialogPromptRow(pane, i)) {
			return "", false
		}
	}
	n := len(pane.starts)
	// The body runs to the first row that begins an option.
	block := n
	for content := first + 1; content < n; content++ {
		if claudeMCPServerOptionAt(pane, content) {
			block = content
			break
		}
		if pane.rows[pane.index[content]].selected ||
			strings.HasPrefix(pane.text[pane.starts[content]:], claudeMCPServerFooterStartSquashed) {
			// A ❯ row that begins no option is a composer, and the dialog
			// paints its footer only under its options.
			return "", false
		}
	}
	if block == n {
		return claudeMCPServerOneCursor(pane, first, nil, "the start of it, still being painted")
	}

	// The options: whole labels, each beginning a row and ending where the
	// next row begins, each once.
	at := pane.starts[block]
	cursorRows := map[int]bool{} // content rows a ❯ may sit on: an option's first row, or its ordinal
	seen := map[int]bool{}
	for at < len(pane.text) {
		content, _ := pane.rowStart(at)
		ordinal := content
		if content+1 < n && claudeMCPTrustOrdinalWord(claudeLaunchDialogRowText(pane, content)) {
			// "❯ 1." on a row of its own, its label on the next.
			content++
		}
		label := claudeMCPServerWholeLabelAt(pane, content, seen)
		if label < 0 {
			break
		}
		seen[label] = true
		cursorRows[ordinal], cursorRows[content] = true, true
		at = pane.starts[content] + len(claudeMCPServerLabelsSquashed[label])
	}

	rest := pane.text[at:]
	switch {
	case rest == "" && !claudeMCPServerCursorPainted(pane):
		return claudeMCPServerOneCursor(pane, first, cursorRows, "its options, with its footer not painted yet")
	case rest == claudeMCPServerFooterSquashed:
		return claudeMCPServerOneCursor(pane, first, cursorRows, "all of it, in rows a narrow pane wrapped or a frame surrounds")
	case rest != "" && strings.HasPrefix(claudeMCPServerFooterSquashed, rest):
		return claudeMCPServerOneCursor(pane, first, cursorRows, "its options, with its footer still being painted")
	}
	if rows, ok := claudeMCPServerPartialOption(pane, at, seen); ok {
		// One more option, painted as far as its ordinal, its first rows, or
		// its cursor alone (#7390).
		for _, content := range rows {
			cursorRows[content] = true
		}
		return claudeMCPServerOneCursor(pane, first, cursorRows, "its options, still being painted")
	}
	// Anything else below the options is not this dialog's.
	return "", false
}

// claudeMCPServerCursorPainted reports whether the pane's last content row is
// a ❯ and nothing else, with no rule between it and the content row above it:
// one more option, painted as far as its cursor glyph (#7390). Claude Code
// paints the cursor before the label it points at, so a frame caught between
// the two shows a bare ❯ under the options. It is no composer: a composer's ❯
// sits under its own rule, and claudeMCPServerPaintedFrom has already turned
// down a plain rule anywhere below the preamble; a box's edge straight above
// the ❯ is turned down here.
func claudeMCPServerCursorPainted(pane claudeLaunchDialogPane) bool {
	last := len(pane.starts) - 1
	if last < 1 || !pane.rows[pane.index[last]].selected || pane.starts[last] != len(pane.text) {
		return false
	}
	for i := pane.index[last-1] + 1; i < pane.index[last]; i++ {
		if claudeTrustFrameRule(pane.lines[i]) {
			return false
		}
	}
	return true
}

// claudeMCPServerOneCursor reports saw, unless a ❯ row from content row first
// down is anything but the one row cursorRows allows.
func claudeMCPServerOneCursor(pane claudeLaunchDialogPane, first int, cursorRows map[int]bool, saw string) (string, bool) {
	cursors := 0
	for content := first; content < len(pane.starts); content++ {
		if !pane.rows[pane.index[content]].selected {
			continue
		}
		cursors++
		if cursors > 1 || !cursorRows[content] {
			return "", false
		}
	}
	return saw, true
}

// claudeMCPServerOptionAt reports whether content row content begins one of
// the dialog's options: its text from there starts with a whole label, its
// ordinal, or is the start of a label the paint stopped inside.
func claudeMCPServerOptionAt(pane claudeLaunchDialogPane, content int) bool {
	if claudeMCPTrustOrdinalWord(claudeLaunchDialogRowText(pane, content)) {
		if content+1 == len(pane.starts) {
			// The paint stopped after an option's ordinal.
			return true
		}
		content++
	}
	text := pane.text[pane.starts[content]:]
	for _, label := range claudeMCPServerLabelsSquashed {
		if strings.HasPrefix(text, label) || strings.HasPrefix(label, text) {
			return true
		}
	}
	return false
}

// claudeMCPServerWholeLabelAt returns which option label not yet seen content
// row content begins and the row after it ends, or -1.
func claudeMCPServerWholeLabelAt(pane claudeLaunchDialogPane, content int, seen map[int]bool) int {
	text := pane.text[pane.starts[content]:]
	for i, label := range claudeMCPServerLabelsSquashed {
		if seen[i] || !strings.HasPrefix(text, label) {
			continue
		}
		end := pane.starts[content] + len(label)
		if _, ok := pane.rowStart(end); ok || end == len(pane.text) {
			return i
		}
	}
	return -1
}

// claudeMCPServerPartialOption reports whether the text from offset at, which
// begins a row, is one more option with the paint stopped inside it: its
// ordinal alone, or the start of a label not yet seen, after its ordinal or
// not. The start can be empty: a ❯ row with no label painted yet, which the
// caller has checked with claudeMCPServerCursorPainted (#7390). rows are the
// content rows a ❯ may sit on.
func claudeMCPServerPartialOption(pane claudeLaunchDialogPane, at int, seen map[int]bool) ([]int, bool) {
	content, ok := pane.rowStart(at)
	if !ok {
		return nil, false
	}
	if !claudeMCPTrustOrdinalWord(claudeLaunchDialogRowText(pane, content)) {
		return []int{content}, claudeMCPServerLabelPrefix(pane.text[at:], seen)
	}
	if content+1 == len(pane.starts) {
		return []int{content}, true
	}
	return []int{content, content + 1}, claudeMCPServerLabelPrefix(pane.text[pane.starts[content+1]:], seen)
}

// claudeMCPServerLabelPrefix reports whether text is the start of an option
// label not yet seen: the last option, with the paint stopped inside it.
func claudeMCPServerLabelPrefix(text string, seen map[int]bool) bool {
	for i, label := range claudeMCPServerLabelsSquashed {
		if !seen[i] && strings.HasPrefix(label, text) {
			return true
		}
	}
	return false
}
