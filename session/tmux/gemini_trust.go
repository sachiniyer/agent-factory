package tmux

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Gemini CLI's folder-trust dialog (#7162). Captured from gemini-cli 0.51.0 with
// af's own capture flags in a fresh, untrusted worktree
// (session/tmux/testdata/gemini_0_51_0_*.ansi):
//
//	╭──────────────────────────────────────────────────────────────╮
//	│                                                              │
//	│ Do you trust the files in this folder?                       │
//	│                                                              │
//	│ Trusting a folder allows Gemini CLI to load its local        │
//	│ configurations, including custom commands, hooks, MCP …      │
//	│                                                              │
//	│ This folder contains:          (only when the repo has any)  │
//	│   • Commands (8):                                            │
//	│     - build                                                  │
//	│ ... last 7 lines hidden (Ctrl+O to show) ...                 │
//	│                                                              │
//	│ ● 1. Trust folder (wt)                                       │
//	│   2. Trust parent folder (worktrees)                         │
//	│   3. Don't trust                                             │
//	│                                                              │
//	╰──────────────────────────────────────────────────────────────╯
//	  Press Ctrl+O to show more lines      (only when the body is cut)
//
// Gemini opens it in an effect after its first render, so its composer paints
// first and the dialog replaces it 80–156 ms later (measured). af used to
// recognize none of it: the Gemini branch of CheckAndHandleTrustPrompt knew only
// the aider-style doc-trust prompt, so it reported the pane clear and the create
// typed the prompt into the dialog. That is worse than a lost prompt. The
// dialog's picker COMMITS an option on a digit keypress and moves on j/k, so a
// typed prompt chose for the user: measured, "fix the 2 failing tests" recorded
// TRUST_PARENT and "just make it work" recorded DO_NOT_TRUST. af's worktrees
// share one parent directory, so the first trusts every af worktree.
//
// af answers it the way it answers Claude Code's and Codex's folder trust: trust
// THIS folder only. It locates "Trust folder" by label and drives the shared
// trustPicker to it, never by position or a digit. "Trust parent folder" is
// never af's choice.
const (
	geminiFolderTrustQuestion = "Do you trust the files in this folder?"
	geminiTrustFolderLabel    = "Trust folder"
	geminiTrustParentLabel    = "Trust parent folder"
	geminiDontTrustLabel      = "Don't trust"
	// geminiTrustSelectionGlyph is the radio indicator Gemini's selection list
	// draws on the highlighted row (BaseSelectionList's selectedIndicator).
	geminiTrustSelectionGlyph = "●"
	// geminiTrustShowMoreLines is the dialog's own overflow hint, painted
	// below its box when the body is cut to fit the pane.
	geminiTrustShowMoreLines = "Press Ctrl+O to show more lines"
	// geminiTrustRestartingNotice is what Gemini paints below the dialog once a
	// trust level is chosen: it then relaunches itself (about 2 s, measured), and
	// a prompt typed in that window reaches the process that is exiting.
	geminiTrustRestartingNotice = "Gemini CLI is restarting to apply the trust changes..."
)

// geminiTrustVerdict is what one capture says about the dialog.
type geminiTrustVerdict int

const (
	// geminiTrustNone: the dialog does not own the pane.
	geminiTrustNone geminiTrustVerdict = iota
	// geminiTrustPartial: the dialog's question and some of its bordered rows
	// end the pane, but its box is not closed yet. Hold.
	geminiTrustPartial
	// geminiTrustOpen: the whole dialog, box closed, nothing but its own
	// overflow hint below it. The parse may still have failed (err), in which
	// case af refuses rather than guess.
	geminiTrustOpen
	// geminiTrustRestarting: the dialog with the restart notice below it — a
	// trust level was chosen and Gemini is relaunching. Hold.
	geminiTrustRestarting
)

// geminiTrustScan is a parsed capture.
type geminiTrustScan struct {
	verdict geminiTrustVerdict
	dialog  claudeFolderTrustDialog
	err     error
}

// geminiTrustRowOf reduces one pane line to what af navigates by, as
// claudeTrustRowOf does for Claude: ANSI and the box's side borders stripped,
// the ● indicator read as the selection, the "N. " ordinal dropped.
func geminiTrustRowOf(line string) claudeTrustRow {
	line = strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(line, "\r"), ""))
	line = strings.TrimSpace(strings.Trim(line, claudeTrustBoxDrawing))
	if line == "" {
		return claudeTrustRow{blank: true}
	}
	row := claudeTrustRow{}
	if rest, ok := strings.CutPrefix(line, geminiTrustSelectionGlyph); ok {
		row.selected = true
		line = strings.TrimSpace(rest)
	}
	row.label = strings.TrimSpace(trimClaudeTrustOrdinal(line))
	return row
}

// geminiTrustSideBordered reports whether a raw line is a row painted inside a
// box: it opens and closes on the box's vertical border.
func geminiTrustSideBordered(line string) bool {
	line = strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(line, "\r"), ""))
	return strings.HasPrefix(line, "│") && strings.HasSuffix(line, "│") && len(line) > len("│")
}

// geminiTrustOption classifies an option label. "Trust folder" and "Trust parent
// folder" carry the directory's base name in parentheses on 0.51.0; the bare
// label is accepted too. Matching is EXACT otherwise: "Trust folder anyway" is
// not an option this dialog paints.
type geminiTrustOption int

const (
	geminiTrustNotAnOption geminiTrustOption = iota
	geminiTrustThisFolder
	geminiTrustParentFolder
	geminiTrustNoFolder
)

func geminiTrustOptionOf(label string) geminiTrustOption {
	names := func(option string) bool {
		if label == option {
			return true
		}
		rest, ok := strings.CutPrefix(label, option+" (")
		return ok && len(rest) > 1 && strings.HasSuffix(rest, ")")
	}
	switch {
	case label == geminiDontTrustLabel:
		return geminiTrustNoFolder
	case names(geminiTrustParentLabel):
		return geminiTrustParentFolder
	case names(geminiTrustFolderLabel):
		return geminiTrustThisFolder
	}
	return geminiTrustNotAnOption
}

// geminiFolderTrustDialogOf decides whether Gemini's folder-trust dialog owns
// the pane, from one property, as the Claude parsers do:
//
// The question, painted as a whole row inside a box, opens the pane's trailing
// region; every row from it down to the box's bottom border is a bordered row of
// the dialog; and below that border there is nothing but the dialog's own
// overflow hint and, once a choice is made, its restart notice.
//
// A working Gemini pane always paints its composer at the bottom, so a dialog
// quoted in a transcript — or the answered dialog's box still visible above the
// relaunched composer — has rows below the border and is not the dialog
// (#4254, #4735). A region that is a prefix of the dialog with its box not yet
// closed is partial.
//
// Inside the box, the options must be one unbroken run of known option rows,
// each at most once, with "Trust folder" among them and the ● cursor on exactly
// one of them. Anything else is reported as an unreadable dialog, which af
// refuses to answer: a refusal costs a retry, a guess can trust the parent
// directory.
func geminiFolderTrustDialogOf(content string) geminiTrustScan {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	rows := make([]claudeTrustRow, len(lines))
	question := -1
	optionShaped := make([]bool, len(lines))
	for i, line := range lines {
		rows[i] = geminiTrustRowOf(line)
		optionShaped[i] = rows[i].selected || geminiTrustHasOrdinal(line)
		if !rows[i].selected && rows[i].label == geminiFolderTrustQuestion && geminiTrustSideBordered(line) {
			question = i
		}
	}
	if question < 0 {
		return geminiTrustScan{}
	}

	bottom := -1
	for i := question + 1; i < len(lines); i++ {
		if claudeTrustFrameRule(lines[i]) {
			bottom = i
			break
		}
		if !rows[i].blank && !geminiTrustSideBordered(lines[i]) {
			// Content outside any box before the dialog closed: this is not
			// the dialog being painted, it is a question quoted in output.
			return geminiTrustScan{}
		}
	}
	if bottom < 0 {
		return geminiTrustScan{verdict: geminiTrustPartial}
	}

	verdict := geminiTrustOpen
	showMore, restart := false, false
	for i := bottom + 1; i < len(rows); i++ {
		switch label := rows[i].label; {
		case rows[i].blank && !claudeTrustFrameRule(lines[i]):
		case !restart && !showMore && label == geminiTrustShowMoreLines:
			showMore = true
		case !restart && label == geminiTrustRestartingNotice:
			restart = true
			verdict = geminiTrustRestarting
		default:
			return geminiTrustScan{}
		}
	}

	dialog, err := parseGeminiFolderTrustRows(rows, optionShaped, question, bottom)
	return geminiTrustScan{verdict: verdict, dialog: dialog, err: err}
}

// parseGeminiFolderTrustRows reads the option block between the question and the
// box's bottom border into the shape the shared trustPicker drives.
//
// A row shaped like an option — an "N. " ordinal or the ● — that is not one of
// the known options makes the dialog unreadable, wherever it sits. Read as body
// text, a relabelled build's "1. Trust this folder" above "2. Trust folder
// (<parent>)" would make af aim at the parent (#7169 review, P3-1).
func parseGeminiFolderTrustRows(rows []claudeTrustRow, optionShaped []bool, question, bottom int) (claudeFolderTrustDialog, error) {
	dialog := claudeFolderTrustDialog{rows: rows, cursor: -1, affirmative: -1}
	const (
		inBody = iota
		inOptions
		afterOptions
	)
	phase := inBody
	seen := map[geminiTrustOption]bool{}
	cursors := 0
	for i := question + 1; i < bottom; i++ {
		row := rows[i]
		option := geminiTrustOptionOf(row.label)
		switch phase {
		case inBody:
			switch {
			case row.blank:
				continue
			case option != geminiTrustNotAnOption:
				phase = inOptions
			case optionShaped[i]:
				return claudeFolderTrustDialog{}, fmt.Errorf("a row is shaped like an option but is not one af knows: %q", row.label)
			default:
				continue
			}
		case inOptions:
			switch {
			case row.blank:
				phase = afterOptions
				continue
			case option == geminiTrustNotAnOption:
				return claudeFolderTrustDialog{}, fmt.Errorf("%q interrupts the dialog's options", row.label)
			}
		case afterOptions:
			if !row.blank {
				return claudeFolderTrustDialog{}, fmt.Errorf("%q is painted below the dialog's options", row.label)
			}
			continue
		}
		if seen[option] {
			return claudeFolderTrustDialog{}, fmt.Errorf("the dialog paints %q twice", row.label)
		}
		seen[option] = true
		if option == geminiTrustThisFolder {
			dialog.affirmative = i
		}
		if row.selected {
			dialog.cursor, cursors = i, cursors+1
		}
	}
	switch {
	case dialog.affirmative < 0:
		return claudeFolderTrustDialog{}, fmt.Errorf("no option is labelled %q", geminiTrustFolderLabel)
	case len(seen) < 2:
		return claudeFolderTrustDialog{}, errors.New("the dialog paints a single option; a picker af can navigate has more than one")
	case cursors != 1:
		return claudeFolderTrustDialog{}, fmt.Errorf("%d option rows carry the %q cursor, so af cannot tell which option is selected",
			cursors, geminiTrustSelectionGlyph)
	}
	return dialog, nil
}

// geminiFolderTrustPicker is Gemini CLI's folder-trust dialog, driven by the same
// trustPicker as Claude Code's.
var geminiFolderTrustPicker = trustPicker{
	name:        geminiFolderTrustDialogName,
	affirmative: geminiTrustFolderLabel,
	read: func(content string) (claudeFolderTrustDialog, error) {
		scan := geminiFolderTrustDialogOf(content)
		if scan.verdict != geminiTrustOpen {
			return claudeFolderTrustDialog{}, errors.New("the folder-trust dialog is no longer on screen")
		}
		return scan.dialog, scan.err
	},
}

// handleGeminiFolderTrust is CheckAndHandleTrustPrompt's Gemini branch for the
// folder-trust dialog. ok is false when the dialog is not on screen at all;
// otherwise handled is the #3302 answer: the dialog, a partial one, or the
// relaunch after it is still in the way of the composer.
func (t *TmuxSession) handleGeminiFolderTrust(content string) (handled, ok bool) {
	switch geminiFolderTrustDialogOf(content).verdict {
	case geminiTrustOpen:
		return t.selectTrustPickerOption(geminiFolderTrustPicker, content), true
	case geminiTrustPartial, geminiTrustRestarting:
		return true, true
	}
	// The strict parser cannot read the dialog, but the folder-trust flow is
	// still what the pane shows: its fingerprints with no composer below them.
	// A pane narrower than 44 columns wraps the question and truncates the
	// labels (measured), and a narrow relaunch wraps the restart notice. Hold
	// without a key; only the strict parser above ever sends one (#7169 review).
	if fingerprinted, last, relaunching := geminiTrustFingerprints(content); fingerprinted && !geminiComposerBelow(content, last) {
		if !relaunching {
			t.refuseTrustPicker(geminiFolderTrustPicker, errors.New(
				"the folder-trust dialog is on screen in a shape af cannot read, such as a pane too narrow for its rows; holding without a key"))
		}
		return true, true
	}
	// No dialog: retire the refusal notice and the settle clock, exactly as the
	// Claude branch does. A pending movement key stays pending — see there.
	t.claudeTrust.refusalLogged = false
	t.claudeTrust.firstSeen = time.Time{}
	return false, false
}

// GeminiFolderTrustPromptPresent reports whether Gemini's folder-trust dialog
// owns the pane, complete — the state CheckAndHandleTrustPrompt answers (or, if
// it cannot read it, refuses to). task's readiness check names it as a
// handled stop, the way it names tmux.CodexTrustPromptPresent.
func GeminiFolderTrustPromptPresent(content string) bool {
	return geminiFolderTrustDialogOf(content).verdict == geminiTrustOpen
}

// GeminiFolderTrustDialogOnScreen reports whether the dialog owns the pane in
// any state: complete, still painting, or relaunching after a choice. Readiness
// uses it to keep the dialog's own box from counting as a ready signal.
func GeminiFolderTrustDialogOnScreen(content string) bool {
	return geminiFolderTrustDialogOf(content).verdict != geminiTrustNone
}

// GeminiRelaunchedAfterTrust reports whether the pane shows Gemini's composer
// painted BELOW its own "restarting to apply the trust changes" notice: the
// relaunched process, which reads the trust level just recorded and so cannot
// open the dialog again. That ordering is the proof, not a timer — the composer
// of a first launch paints before Gemini decides whether to open the dialog, so
// a composer on its own says nothing about what comes next. The notice is
// found as a phrase, so a pane narrow enough to wrap it still qualifies.
//
// The composer is Gemini 0.51.0's input box: a row of ▄, the "> " prompt row
// (plus any continuation rows), then a row of ▀.
func GeminiRelaunchedAfterTrust(content string) bool {
	if GeminiFolderTrustDialogOnScreen(content) {
		return false
	}
	lines := geminiTrustLines(content)
	end := geminiTrustPhraseEnd(lines, geminiTrustRestartingNotice)
	return end >= 0 && geminiComposerBelow(content, end)
}

// GeminiFolderTrustFingerprinted reports whether any of the folder-trust
// flow's fingerprints is on screen: its question, its "Trust parent folder"
// label, or its restart notice, each found as a phrase so that wrapping and
// truncation cannot hide it. Readiness treats such a pane as not ready unless
// it is the dialog itself (a handled stop) or the relaunched composer; the old
// box's "╰" is never the reason a launch proceeds.
func GeminiFolderTrustFingerprinted(content string) bool {
	fingerprinted, _, _ := geminiTrustFingerprints(content)
	return fingerprinted
}

// geminiTrustFingerprints finds the folder-trust flow's fingerprints. last is
// the row where the lowest of them ends, and relaunching reports that the
// restart notice is among them.
func geminiTrustFingerprints(content string) (fingerprinted bool, last int, relaunching bool) {
	lines := geminiTrustLines(content)
	last = -1
	for _, phrase := range []string{geminiFolderTrustQuestion, geminiTrustParentLabel, geminiTrustRestartingNotice} {
		if end := geminiTrustPhraseEnd(lines, phrase); end >= 0 {
			last = max(last, end)
			relaunching = relaunching || phrase == geminiTrustRestartingNotice
		}
	}
	return last >= 0, last, relaunching
}

// geminiComposerBelow reports whether any part of Gemini's composer is painted
// below row from: a full composer opening there, or either of its edges (a row
// of ▄ or of ▀). An edge alone is enough, because a draft pasted INTO the
// composer sits between its edges, so only the bottom edge is below it.
func geminiComposerBelow(content string, from int) bool {
	lines := geminiTrustLines(content)
	for i := from + 1; i < len(lines); i++ {
		text := strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(lines[i], "\r"), ""))
		if geminiComposerOpensAt(lines, i) ||
			(text != "" && (strings.Trim(text, "▄") == "" || strings.Trim(text, "▀") == "")) {
			return true
		}
	}
	return false
}

func geminiTrustLines(content string) []string {
	return strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
}

// geminiTrustPhraseEnd finds phrase in the pane's text as Gemini wraps it —
// each row stripped of ANSI and box borders, rows joined by a single space —
// and returns the row its LAST occurrence ends on, or -1.
func geminiTrustPhraseEnd(lines []string, phrase string) int {
	var joined strings.Builder
	var rowAt []int // rowAt[k] is the row byte k of joined came from
	for i, line := range lines {
		text := strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(line, "\r"), ""))
		text = strings.Join(strings.Fields(strings.Trim(text, claudeTrustBoxDrawing)), " ")
		if text == "" {
			continue
		}
		if joined.Len() > 0 {
			joined.WriteByte(' ')
			rowAt = append(rowAt, i)
		}
		joined.WriteString(text)
		for range len(text) {
			rowAt = append(rowAt, i)
		}
	}
	at := strings.LastIndex(joined.String(), phrase)
	if at < 0 {
		return -1
	}
	return rowAt[at+len(phrase)-1]
}

// geminiTrustHasOrdinal reports whether a pane line, stripped of ANSI, its box
// borders and the ● indicator, opens with an "N. " picker ordinal.
func geminiTrustHasOrdinal(line string) bool {
	text := strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(line, "\r"), ""))
	text = strings.TrimSpace(strings.Trim(text, claudeTrustBoxDrawing))
	text = strings.TrimSpace(strings.TrimPrefix(text, geminiTrustSelectionGlyph))
	return text != "" && trimClaudeTrustOrdinal(text) != text
}

// geminiComposerOpensAt reports whether lines[i] is the top edge of Gemini's
// input box: a row of ▄, then the "> " prompt row, then — after any
// continuation rows — a row of ▀.
func geminiComposerOpensAt(lines []string, i int) bool {
	plain := func(j int) string {
		return strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(lines[j], "\r"), ""))
	}
	only := func(s, r string) bool { return s != "" && strings.Trim(s, r) == "" }
	if i+2 >= len(lines) || !only(plain(i), "▄") || !strings.HasPrefix(plain(i+1), ">") {
		return false
	}
	for j := i + 2; j < len(lines); j++ {
		if only(plain(j), "▀") {
			return true
		}
		if only(plain(j), "▄") || plain(j) == "" {
			return false
		}
	}
	return false
}
