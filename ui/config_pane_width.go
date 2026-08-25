package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	muesliansi "github.com/muesli/ansi"

	"github.com/sachiniyer/agent-factory/config"
)

// Width arithmetic for the config pane (#1145 split, forced by #3421 + #3430
// landing in the same file).
//
// The seam is deliberate rather than a line-count dodge: config_pane.go owns WHAT
// the pane's fragments are — the rows, the copy, the hint text, the key handling —
// and this file owns HOW MANY CELLS each one may occupy. Every budget, every
// truncator and every measurement choice lives here, together, because the whole
// class of bug these came from is two of them disagreeing:
//
//   - a rune-count budget against a cell-measuring renderer (#3421),
//   - a hint row with nothing left to shed against a narrower pane (#3430),
//   - a field width that ignored the cursor cell its own View() renders,
//   - a grapheme-aware cut against a compositor that counts per codepoint.
//
// Keeping them in one file is what makes the next disagreement visible.

// sizeEditField sizes the value field from the row it actually renders into.
//
// It used to be a flat width-24, which is only correct for a 20-cell key — so
// editing `network.require_loopback_token` (30 cells) rendered a row wider than
// the pane at EVERY geometry, not just narrow ones (#3430). The field scrolls
// horizontally, so a smaller width costs visible context, never content.
//
// Called from SetSize (which runs on every render, so a resize mid-edit reaches
// here) and from beginEdit, where the selected key is what changed.
//
// Two things this has to get right beyond the arithmetic, both from the #3430
// review:
//
// AN UNSIZED PANE CONSTRAINS NOTHING. At width 0 fitPaneLine, fitHints and window
// all pass content through untouched, and textinput reads Width 0 as unbounded
// (handleOverflow hands back the whole value). Deriving a width from a pane that
// has no width would put this one member out of step with all three and show a
// narrow scrolling tail where there is no box to fit.
//
// AND ASSIGNING Width DOES NOT REFLOW. textinput recomputes its horizontal
// viewport in handleOverflow, which only runs from SetValue and SetCursor — never
// from a bare Width assignment. So a resize mid-edit left offset/offsetRight
// describing the OLD width and View() kept rendering the old, wider slice, which
// fitPaneLine then clipped from the right: the value's tail and the cursor
// disappeared until the next keystroke happened to recompute it. Measured before
// the fix: Width 42 -> Width 10 with the value untouched still rendered a 45-cell
// View.
//
// Reflowing takes BOTH calls below, and which anchor they use is load-bearing.
// handleOverflow only moves the offsets when the cursor falls OUTSIDE the window it
// already has (pos < offset, or pos >= offsetRight); otherwise it returns with the
// old window intact. So re-setting the cursor to the position it already holds
// reflows nothing for a cursor in the interior — narrowing 42 -> 10 with the cursor
// at position 25 kept the 45-cell View — and anchoring at the START does not help
// either, because offset is already 0 there and the stale offsetRight is never
// revisited (measured: the start case still rendered the wide slice).
//
// CursorEnd is the one anchor that always satisfies a branch: pos == len(value) is
// >= offsetRight for any window, so offsetRight is rebuilt and offset walked back
// against the NEW Width. Restoring the real position then falls outside that
// window, or inside one that already fits. SetValue is not an alternative: it
// re-enters the same pos-relative branches (#3430 review, round 2).
func (c *ConfigPane) sizeEditField() {
	width := 0
	if c.width > 0 {
		key := ""
		if e := c.selectedEntry(); e != nil {
			key = e.Key
		}
		_, width = c.editRowSplit(key)
	}
	c.input.Width = width
	pos := c.input.Position()
	c.input.CursorEnd()
	c.input.SetCursor(pos)
}

// editRowSplit divides an editing row's width between the KEY and the value
// FIELD, and is the single place that arithmetic lives so the renderer and the
// field's own Width can never disagree about it.
//
// The field is served first, and the key yields (#3430 review). At af's
// supported minimum — a 40-column terminal, which app/render.go turns into 34
// content cells — the cursor, `network.require_loopback_token` and the gap
// consume all 34 by themselves. Clipping the composed row from the right then
// removes the entire focused input: the width invariant holds, the row ends in
// an ellipsis, and the user is typing into a field they cannot see. Truncating
// the KEY instead costs identification the purpose line right below already
// gives back, and is the only degradation that keeps the row's actual job doable.
//
// The key is only ever shortened when it would push the field below its minimum,
// so at ordinary widths this returns the key untouched and the same field width
// as before.
//
// Note the asymmetry with a non-editing row, which clips the VALUE and keeps the
// whole key: there the key is the row's identity and the value is a preview, so
// the preview is what can go. While editing, the field IS the task.

// editRowSplit divides an editing row's width between the KEY and the value
// FIELD, and is the single place that arithmetic lives so the renderer and the
// field's own Width can never disagree about it.
//
// The field is served first, and the key yields (#3430 review). At af's
// supported minimum — a 40-column terminal, which app/render.go turns into 34
// content cells — the cursor, `network.require_loopback_token` and the gap
// consume all 34 by themselves. Clipping the composed row from the right then
// removes the entire focused input: the width invariant holds, the row ends in
// an ellipsis, and the user is typing into a field they cannot see. Truncating
// the KEY instead costs identification the purpose line right below already
// gives back, and is the only degradation that keeps the row's actual job doable.
//
// The key is only ever shortened when it would push the field below its minimum,
// so at ordinary widths this returns the key untouched and the same field width
// as before.
//
// Note the asymmetry with a non-editing row, which clips the VALUE and keeps the
// whole key: there the key is the row's identity and the value is a preview, so
// the preview is what can go. While editing, the field IS the task.
func (c *ConfigPane) editRowSplit(key string) (keyBudget, fieldWidth int) {
	keyWidth := lipgloss.Width(key)
	// Everything the field costs beyond its text: the prompt, plus the cell a
	// focused textinput renders past its Width for the cursor.
	fieldChrome := lipgloss.Width(c.input.Prompt) + editFieldCursorWidth
	available := c.width - entryRowChromeWidth
	keyBudget = available - minEditFieldWidth - fieldChrome
	if keyBudget > keyWidth {
		keyBudget = keyWidth
	}
	if keyBudget < 0 {
		keyBudget = 0
	}
	fieldWidth = available - keyBudget - fieldChrome
	// The floor keeps the field usable at a pane too narrow for any split to fit;
	// fitPaneLine then clips the row, so the floor cannot reintroduce an overflow.
	if fieldWidth < minEditFieldWidth {
		fieldWidth = minEditFieldWidth
	}
	return keyBudget, fieldWidth
}

// entryRowChromeWidth is the fixed chrome every entry row carries: the
// two-cell selection cursor plus the two-cell gap between key and value.

// entryRowChromeWidth is the fixed chrome every entry row carries: the
// two-cell selection cursor plus the two-cell gap between key and value.
const entryRowChromeWidth = 4

// minEditFieldWidth keeps a value field usable at a degenerate pane width.

// minEditFieldWidth keeps a value field usable at a degenerate pane width.
const minEditFieldWidth = 8

// editFieldCursorWidth is the cell a focused textinput renders PAST its Width,
// for the cursor. Measured, not assumed: Width 42 with a 2-cell prompt renders a
// 45-cell View. Without it the composed row is one cell over the pane and the
// fitPaneLine backstop clips the field's last character — which is invisible to a
// width assertion (the row does fit, after clipping) and showed up only in the
// real terminal, as `…TAILMAR…` where the value's tail should have been.

// editFieldCursorWidth is the cell a focused textinput renders PAST its Width,
// for the cursor. Measured, not assumed: Width 42 with a 2-cell prompt renders a
// 45-cell View. Without it the composed row is one cell over the pane and the
// fitPaneLine backstop clips the field's last character — which is invisible to a
// width assertion (the row does fit, after clipping) and showed up only in the
// real terminal, as `…TAILMAR…` where the value's tail should have been.
const editFieldCursorWidth = 1

// fitPaneLine clips one composed line to the pane's width. It is the backstop
// behind every per-fragment budget in this file, not a substitute for them: the
// budgets keep the common case readable, and this keeps the INVARIANT true — the
// pane never renders a line wider than itself, because a wider line is wrapped
// by the overlay frame, and a wrapped line makes the height window's count (which
// is taken from renderRowLines) a lie (#3430).
//
// A pane with no width yet (before the first SetSize) is left alone: clipping to
// zero would blank the frame.
func (c *ConfigPane) fitPaneLine(line string) string {
	line = flattenToOneLine(line)
	if c.width <= 0 {
		return line
	}
	return fitLine(line, c.width)
}

// flattenToOneLine turns embedded control whitespace into spaces so the result is
// genuinely ONE line.
//
// Load-bearing, not hygiene, and for the same reason at both of its call sites. An
// unrestricted string key (on_archive_command) may hold a newline, and every width
// function here reports the WIDEST line of a multi-line string — so an unflattened
// value measures as narrow, passes a width check whole, and turns one list row into
// several. That is the overflow this file guards against arriving through height
// instead of width, and it defeats countLines the same way. A tab is included for
// the matching reason: a terminal expands it to the next tab stop, which no width
// measurement predicts.
//
// Runs are NOT collapsed: a list row is a preview of the real value, and collapsing
// whitespace would misreport what is stored.

// flattenToOneLine turns embedded control whitespace into spaces so the result is
// genuinely ONE line.
//
// Load-bearing, not hygiene, and for the same reason at both of its call sites. An
// unrestricted string key (on_archive_command) may hold a newline, and every width
// function here reports the WIDEST line of a multi-line string — so an unflattened
// value measures as narrow, passes a width check whole, and turns one list row into
// several. That is the overflow this file guards against arriving through height
// instead of width, and it defeats countLines the same way. A tab is included for
// the matching reason: a terminal expands it to the next tab stop, which no width
// measurement predicts.
//
// Runs are NOT collapsed: a list row is a preview of the real value, and collapsing
// whitespace would misreport what is stored.
func flattenToOneLine(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', '\v', '\f':
			return ' '
		}
		return r
	}, s)
}

// renderRowLines renders every row to lines, reporting the line span of the
// selected row so the window can keep it on screen. A row is not one line: the
// selected row also shows its purpose and its allowed values, so the span is
// what must stay visible, not a single index.

// displayValue renders a value for the LIST, which is a different job from
// rendering it into an edit field.
//
// Two decorations live here and MUST NOT leak into the edit field (c.input is
// always filled from e.Value directly):
//
//   - An unset value reads as "(unset)". A blank column looks like a rendering
//     bug; the empty edit field it opens does not.
//   - A long value is truncated. A [theme] table serializes to ~700 characters
//     of JSON — rendered whole it wraps over the entire pane and buries every
//     row after it. The edit field still receives the complete value.
//
// This is the same split CurrentValue documents: what you SHOW and what you can
// SAVE BACK are different, and conflating them is how `""` ends up in a user's
// config.toml.
func (c *ConfigPane) displayValue(e config.ConfigEntry) string {
	if e.Value == "" {
		return "(unset)"
	}
	// Leave room for the cursor and the key, and measure in terminal CELLS rather
	// than runes (#3421). A CJK or emoji value renders 2+ cells per rune, so a
	// rune-count budget under-truncates: at a 72-column pane a CJK path rendered an
	// 85-cell row, which the overlay frame then wraps — and a wrapped row makes the
	// height window's line count a lie, so the pane overflows its box (the exact
	// failure TestConfigPaneNeverRendersALineWiderThanThePane exists to prevent).
	budget := c.width - lipgloss.Width(e.Key) - 8
	if budget < 12 {
		budget = 12
	}
	return truncateConfigPreview(e.Value, budget)
}

// maxConfigPreviewRunes caps how much of a value the LIST will even look at.
//
// A cell budget alone does not bound the work: a run of combining marks, zero-width
// spaces or joiners measures ~0 cells, so a width-based cut keeps all of it and every
// repaint emits and processes the whole thing. The budget is at most ~70 cells and a
// legitimate value needs about one rune per cell, so this is orders of magnitude
// above any real value while keeping a hostile one bounded.

// maxConfigPreviewRunes caps how much of a value the LIST will even look at.
//
// A cell budget alone does not bound the work: a run of combining marks, zero-width
// spaces or joiners measures ~0 cells, so a width-based cut keeps all of it and every
// repaint emits and processes the whole thing. The budget is at most ~70 cells and a
// legitimate value needs about one rune per cell, so this is orders of magnitude
// above any real value while keeping a hostile one bounded.
const maxConfigPreviewRunes = 512

// truncateConfigPreview renders an untrusted config value as a bounded ONE-LINE
// preview for the list. Config values are user text — a free-form string key like
// on_archive_command accepts anything TOML can express, escapes included — so each
// step below answers a specific way that text can break the pane rather than being
// general hygiene (#3421 review).
//
// The edit field is untouched by all of this: c.input is filled from e.Value
// directly, so what you can SAVE BACK is still exactly what is stored. That is the
// same show-vs-save split CurrentValue documents.

// truncateConfigPreview renders an untrusted config value as a bounded ONE-LINE
// preview for the list. Config values are user text — a free-form string key like
// on_archive_command accepts anything TOML can express, escapes included — so each
// step below answers a specific way that text can break the pane rather than being
// general hygiene (#3421 review).
//
// The edit field is untouched by all of this: c.input is filled from e.Value
// directly, so what you can SAVE BACK is still exactly what is stored. That is the
// same show-vs-save split CurrentValue documents.
func truncateConfigPreview(value string, budget int) string {
	// 1. ESCAPE SEQUENCES OUT. A value may hold an ANSI/OSC sequence (TOML writes
	// one with \u001B), and a cell-measuring truncator deliberately preserves
	// sequences across its cut — that is what keeps styled content from losing its
	// reset. Preserved here, an ED or CUP sequence in a config value would clear the
	// screen or move the cursor from inside a list row. ui/err.go strips for the
	// same reason, and notes the same gap: Strip leaves a bare \r, which step 2 takes.
	value = xansi.Strip(value)
	// 2. ONE LINE. lipgloss.Width reports the WIDEST line of a multi-line string, so
	// a value made of many short lines measures as narrow, survives any width check
	// whole, and turns one list row into several — the same overflow arriving through
	// height instead of width. A tab goes too: a terminal expands it to the next tab
	// stop, which no width measurement predicts.
	value = flattenToOneLine(value)
	// 3. BOUND THE VOLUME before measuring width, because zero-width runes are free
	// under every width measure. See maxConfigPreviewRunes.
	if runes := []rune(value); len(runes) > maxConfigPreviewRunes {
		value = string(runes[:maxConfigPreviewRunes])
	}
	// 4. CUT TO THE BUDGET IN THE MEASURE THE COMPOSITOR ACTUALLY USES.
	//
	// Three width functions are in play here and they do NOT agree, so the choice
	// matters (all measured, on one joined-emoji family):
	//
	//   lipgloss.Width / x-ansi   2 cells — groups codepoints into graphemes
	//   runewidth.StringWidth     2 cells — likewise
	//   muesli/ansi.PrintableRuneWidth  8 cells — sums runewidth.RuneWidth per rune
	//
	// The last one is the one that decides whether the frame survives:
	// ui/overlay.PlaceOverlay measures the foreground with it and RETURNS THE
	// FOREGROUND ALONE when it reads wider than the background, so a modal holding a
	// few joined-emoji values would erase the whole TUI behind it. A grapheme-aware
	// cut cannot prevent that — it would admit 22 families in a 44-cell budget, which
	// that function reads as 176 cells. tmux advances per codepoint too, so the
	// pessimistic count is also what a real terminal does.
	//
	// So budget against the compositor's own function, walking runes by the exact
	// per-rune width it sums. Bounding it bounds the grapheme measures as well
	// (grouping can only narrow), so the pane's other width assertions still hold.
	// The cost is density: a value of joined emoji shows about a quarter as many as a
	// grapheme-aware cut would allow. That is the right trade against dropping the
	// frame, and it matches what the emulator renders.
	if compositorWidth(value) <= budget {
		return value
	}
	tail := "…"
	if budget < compositorWidth(tail) {
		tail = ""
	}
	limit := budget - compositorWidth(tail)
	var b strings.Builder
	width := 0
	for _, r := range value {
		rw := runewidth.RuneWidth(r)
		if width+rw > limit {
			break
		}
		b.WriteRune(r)
		width += rw
	}
	return b.String() + tail
}

// compositorWidth measures a string the way ui/overlay.PlaceOverlay does, which
// is the measurement that decides whether a modal still fits over the frame. It
// is deliberately NOT lipgloss.Width: see truncateConfigPreview step 4.

// compositorWidth measures a string the way ui/overlay.PlaceOverlay does, which
// is the measurement that decides whether a modal still fits over the frame. It
// is deliberately NOT lipgloss.Width: see truncateConfigPreview step 4.
func compositorWidth(s string) int {
	return muesliansi.PrintableRuneWidth(s)
}

// wrapIndented renders prose wrapped to the pane's width, indented under its key.
//
// Wrapping HERE rather than letting the overlay frame do it is load-bearing, not
// cosmetic. The window's budget counts the lines renderRowLines produces, so a
// line the frame later wraps into three physical rows makes that count a lie and
// the pane overflows its box anyway — the selection scrolls off exactly as it did
// before the window existed. A purpose line is genuinely long (worktree_root's is
// 147 characters, over 2x a 72-column pane), so this is the common case, not an
// edge one.
//
// Prose WRAPS rather than truncating, unlike a value (displayValue): a value's
// tail is usually noise, but a sentence's is the half that says what the setting
// does.

// configHint is one fragment of a hint row, with the order it is shed in when
// the row does not fit the pane. drop 0 means NEVER shed; 1 goes first.
type configHint struct {
	text string
	drop int
}

// fitHints renders the richest hint row that fits the pane (#1936/#3430).
//
// Adding a hint is a WIDTH change (#1936), and the row had exactly one
// sheddable fragment — so once the assistant button landed, the shed remainder
// was still 43 cells and any pane narrower than that overflowed with nothing
// left to drop. The ladder below replaces that single special case: each step
// sheds one more fragment, in `drop` order, and the first step that fits wins.
//
// The ORDER is a product decision, so it is stated here rather than left to fall
// out of the composition:
//
//  1. the advanced toggle — `a` still works, and pressing it reveals the tier,
//  2. `↑/↓ move` — arrow keys are the most conventional binding on the row,
//  3. `↵ edit` — Enter to activate is nearly as conventional,
//  4. `C assistant` — deliberately always-on (#2453), so it goes last of all,
//
// and `esc close` is shed by NOTHING. A modal must always advertise the way out:
// the key stays live either way, but a user who cannot see it is stuck in a pane
// they do not know how to leave, which is the #2830 failure (an advertised key
// that is not live where focus is) run backwards.
//
// If even the un-sheddable remainder is too wide — a pane narrower than
// "esc close" — the row is CLIPPED rather than left for the overlay frame to
// wrap. Never exceed the box.

// fitHints renders the richest hint row that fits the pane (#1936/#3430).
//
// Adding a hint is a WIDTH change (#1936), and the row had exactly one
// sheddable fragment — so once the assistant button landed, the shed remainder
// was still 43 cells and any pane narrower than that overflowed with nothing
// left to drop. The ladder below replaces that single special case: each step
// sheds one more fragment, in `drop` order, and the first step that fits wins.
//
// The ORDER is a product decision, so it is stated here rather than left to fall
// out of the composition:
//
//  1. the advanced toggle — `a` still works, and pressing it reveals the tier,
//  2. `↑/↓ move` — arrow keys are the most conventional binding on the row,
//  3. `↵ edit` — Enter to activate is nearly as conventional,
//  4. `C assistant` — deliberately always-on (#2453), so it goes last of all,
//
// and `esc close` is shed by NOTHING. A modal must always advertise the way out:
// the key stays live either way, but a user who cannot see it is stuck in a pane
// they do not know how to leave, which is the #2830 failure (an advertised key
// that is not live where focus is) run backwards.
//
// If even the un-sheddable remainder is too wide — a pane narrower than
// "esc close" — the row is CLIPPED rather than left for the overlay frame to
// wrap. Never exceed the box.
func (c *ConfigPane) fitHints(hints []configHint) string {
	maxDrop := 0
	for _, h := range hints {
		if h.drop > maxDrop {
			maxDrop = h.drop
		}
	}
	row := joinHints(hints, 0)
	for shed := 0; shed <= maxDrop; shed++ {
		row = joinHints(hints, shed)
		if c.width <= 0 || lipgloss.Width(row) <= c.width {
			return row
		}
	}
	return c.fitPaneLine(row)
}

// joinHints composes the hint row with every fragment whose drop order is at or
// below shedThrough removed. shedThrough 0 keeps them all.

// joinHints composes the hint row with every fragment whose drop order is at or
// below shedThrough removed. shedThrough 0 keeps them all.
func joinHints(hints []configHint, shedThrough int) string {
	kept := make([]string, 0, len(hints))
	for _, h := range hints {
		if h.drop != 0 && h.drop <= shedThrough {
			continue
		}
		kept = append(kept, h.text)
	}
	return strings.Join(kept, " · ")
}

// SetEditValueForTest and EditValueForTest expose the value field's buffer to
// the app package's tests, which drive the REAL handleStateConfigEditor (where
// the #1961 quit-key bug class lives) and must assert what actually reached the
// field. The pane's own tests reach c.input directly; app's cannot.
