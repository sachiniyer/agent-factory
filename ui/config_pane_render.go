package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
)

// window returns the slice of rowLines to render, plus how many lines are hidden
// above and below, scrolling only as far as it must to reveal the selection.
//
// It moves the view by the MINIMUM needed rather than centering the selection:
// centering would shift the whole list on every keypress, which reads as the
// content moving under the cursor instead of the cursor moving through it.
func (c *ConfigPane) window(rowLines []string, selStart, selEnd, budget int) (visible []string, above, below int) {
	if c.height <= 0 {
		// No size yet: SetSize has not run. There is no box to fit, so render
		// everything rather than inventing a window from a height of zero.
		c.scrollTop = 0
		return rowLines, 0, 0
	}
	if budget <= 0 {
		// A real but tiny box: the header and footer already consume it. Show one
		// row — the selected one — rather than dumping all 38 lines into a 5-line
		// pane, which is what "budget <= 0 means render everything" used to do.
		// Degenerate, but bounded: the user still sees where they are.
		budget = 1
	}
	if len(rowLines) <= budget {
		c.scrollTop = 0
		return rowLines, 0, 0
	}

	if selStart >= 0 && !c.scrolledWithinSelection(selStart, selEnd, budget) {
		if selStart < c.scrollTop {
			c.scrollTop = selStart
		}
		// A selected row taller than the window (a long purpose line) pins to its
		// top on arrival: showing its tail and hiding its key would be worse than
		// clipping. ↑/↓ then scroll through the rest of it (scrollSelection).
		if selEnd > c.scrollTop+budget {
			c.scrollTop = min(selStart, selEnd-budget)
		}
	}
	maxTop := len(rowLines) - budget
	c.scrollTop = max(0, min(c.scrollTop, maxTop))

	return rowLines[c.scrollTop : c.scrollTop+budget], c.scrollTop, len(rowLines) - (c.scrollTop + budget)
}

// scrolledWithinSelection reports whether the view sits inside a selection
// taller than the window — a top line from the selection's own first line down
// to the one that shows its last line. window() leaves such a view alone
// instead of snapping it back to the row's top, which would undo every ↓ the
// user spent reading the row. Arriving on an over-tall row never starts in that
// range — from above, the last frame's top line was above the row; from below,
// that frame showed the next row, so its top was past the row's last
// window-full — so arrival still pins the row's top, exactly as before.
func (c *ConfigPane) scrolledWithinSelection(selStart, selEnd, budget int) bool {
	return selEnd-selStart > budget && c.scrollTop >= selStart && c.scrollTop <= selEnd-budget
}

// scrollSelection moves the view one line through a selection taller than the
// window and reports whether it did; the caller moves the cursor only when it
// did not. Without it, every line of such a row below its first window-full was
// unreachable — the list scrolls only to reveal the selection, and an over-tall
// selection pins to its top. On a 40x10 terminal the list is one line tall, so
// that is every line of every multi-line row: a quota or account-evidence row's
// detail, a long purpose, and the mover line and footnote the last
// account-evidence row's span carries (#7056 review). A selection that fits
// the window never scrolls here, so ↑/↓ on a normal-sized pane are unchanged.
func (c *ConfigPane) scrollSelection(delta int) bool {
	if c.height <= 0 {
		return false
	}
	lines, selStart, selEnd := c.renderRowLines()
	budget := max(1, c.windowBudget(c.renderHeader(), c.renderStatus()+c.renderHints()))
	if selStart < 0 || selEnd-selStart <= budget || len(lines) <= budget {
		return false
	}
	// Settle the view to what the last frame showed for this selection, so the
	// step below starts from the screen rather than from a stale scrollTop.
	c.window(lines, selStart, selEnd, budget)
	switch {
	case delta > 0 && c.scrollTop+budget < selEnd:
		c.scrollTop++
		return true
	case delta < 0 && c.scrollTop > selStart:
		c.scrollTop--
		return true
	}
	return false
}

// windowBudget is how many list lines fit between a header and a footer once
// the cue rows are reserved — the one budget renderWindowed draws with and
// scrollSelection steps by, so the two can never disagree about what is shown.
func (c *ConfigPane) windowBudget(header, footer string) int {
	return c.height - countLines(header) - countLines(footer) - cueRows
}

// cueRows is the vertical space reserved for the "↑ n more" / "↓ n more" cues.
const cueRows = 2

// countLines counts the rendered lines in a fragment. Exact only because every
// fragment String() composes ends with a newline.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n")
}

// renderEntryRow renders one key row: cursor, key, value (or the live edit
// field), and its one-line purpose.
func (c *ConfigPane) renderEntryRow(i int, row configRow, e config.ConfigEntry) string {
	var b strings.Builder
	selected := i == c.selectedIdx

	cursor := "  "
	if selected {
		cursor = SelectionMarker("› ")
	}
	b.WriteString(cursor)

	keyText := e.Key
	if selected && c.editing {
		// Clip the KEY, not the field — see editRowSplit. Clipped as plain text
		// before styling, so the truncation never lands inside an escape sequence.
		if budget, _ := c.editRowSplit(e.Key); budget < lipgloss.Width(keyText) {
			keyText = fitLine(keyText, budget)
		}
	}
	key := configKeyStyle.Render(keyText)
	if selected {
		key = configSelectedStyle.Render(keyText)
	}
	b.WriteString(key)
	valueStyle := configValueStyle
	if selected {
		valueStyle = configSelectedStyle
	}
	b.WriteString(valueStyle.Render("  "))

	if selected && c.editing {
		b.WriteString(renderConfigInput(&c.input))
	} else {
		b.WriteString(valueStyle.Render(c.displayValue(e)))
		// The marker rides the value it qualifies (#7009): without it a
		// project-overridden row reads as the global value it replaced.
		if marker := scopeMarker(e); marker != "" {
			markerStyle := configLocationStyle
			if selected {
				markerStyle = configSelectedStyle
			}
			b.WriteString(markerStyle.Render(marker))
		}
	}
	// Clip, do not wrap (#3430). displayValue and sizeEditField already budget
	// this row, but neither can shrink the KEY: at a narrow pane the key alone
	// (network.require_loopback_token is 30 cells) plus the cursor and the gap
	// outgrows the box, and a wrapped row is the failure the height window cannot
	// survive. The purpose lines below wrap on purpose and are already bounded.
	line := c.fitPaneLine(b.String())
	b.Reset()
	b.WriteString(line)
	b.WriteString("\n")

	if selected {
		b.WriteString(c.wrapIndented(e.Purpose, configPurposeStyle))
		if len(e.Enum) > 0 && e.Type != "table" {
			b.WriteString(c.wrapIndented("one of: "+configEnumLabels(e), configHintStyle))
		}
		if note := scopeNote(e); note != "" {
			b.WriteString(c.wrapIndented(note, configHintStyle))
		}
	}
	return b.String()
}

// scopeMarker is the suffix a project read puts after a row's value (#7009).
// ConfigEntry.Scope is an open set, so a value this build does not know still
// gets a marker: the one thing it certainly means is "not the global value".
func scopeMarker(e config.ConfigEntry) string {
	switch e.Scope {
	case "":
		return ""
	case config.ScopeProject:
		return " (project)"
	case config.ScopePersonal:
		return " (personal)"
	case config.ScopeProjectPersonal:
		return " (project+personal)"
	case config.ScopePersonalIgnored:
		return " (personal: stored)"
	}
	return " (project-scoped)"
}

// scopeNoteGlobalBudget bounds the global value quoted in a scope note, so a
// long table cannot turn the note into a paragraph under the row.
const scopeNoteGlobalBudget = 40

// scopeNote is the selected row's account of a marker: which layer supplied
// the value, what the global value is, and that the field edits the global
// value — the issue's whole complaint was an editor that looked authoritative
// about a value it would not change.
func scopeNote(e config.ConfigEntry) string {
	if e.Scope == "" {
		return ""
	}
	if e.Scope == config.ScopePersonalIgnored {
		return "a personal override is stored but not applied — this project uses the global value"
	}
	global := "(unset)"
	if e.GlobalValue != "" {
		global = truncateConfigPreview(e.GlobalValue, scopeNoteGlobalBudget)
	}
	label := "project-scoped override"
	switch e.Scope {
	case config.ScopeProject:
		label = "project override"
	case config.ScopePersonal:
		label = "personal override"
	case config.ScopeProjectPersonal:
		label = "project + personal overrides"
	}
	return label + " · global: " + global + " · enter edits the global value"
}

func (c *ConfigPane) wrapIndented(text string, style lipgloss.Style) string {
	const indent = "    "
	width := c.width - len(indent) - 2
	if width < 20 {
		width = 20
	}
	wrapped := style.Width(width).Render(text)
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(wrapped, "\n"), "\n") {
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// wrap renders text wrapped to the pane width, one fragment per line, always
// ending in a newline so countLines stays exact.
func (c *ConfigPane) wrap(text string, style lipgloss.Style) string {
	width := c.width - 2
	if width < 20 {
		width = 20
	}
	return strings.TrimSuffix(style.Width(width).Render(text), "\n") + "\n"
}

// renderHints renders the footer. Every fragment String() composes ends with a
// newline so countLines is an exact line count — the window's budget is computed
// from it, and an off-by-one there is an overflowing pane.
func (c *ConfigPane) renderHints() string {
	if c.editing {
		return "\n" + configHintStyle.Render(c.fitHints([]configHint{
			{text: "↵ save", drop: 1},
			{text: "esc cancel"},
		})) + "\n"
	}
	if c.accounts.registering {
		return "\n" + configHintStyle.Render(c.fitHints([]configHint{
			{text: "↵ register", drop: 1},
			{text: "esc cancel"},
		})) + "\n"
	}
	if account := c.selectedAccount(); account != nil {
		verb := "↵ log in"
		if account.Register {
			verb = "↵ register"
			if c.accounts.busy {
				verb = "registration pending"
			}
		} else if c.accounts.loginRefusal != "" {
			verb = "login unavailable"
		}
		return "\n" + configHintStyle.Render(c.fitHints([]configHint{
			{text: "↑/↓ move", drop: 2},
			{text: verb, drop: 3},
			{text: "C assistant", drop: 4},
			{text: "esc close"},
		})) + "\n"
	}
	if host := c.selectedHost(); host != nil {
		verb := "↵ check"
		if host.Action == hostActionAdd {
			verb = "↵ add a host"
		} else if host.Action == hostActionCheckAll {
			verb = "↵ check all"
			if c.hostBusy("") {
				verb = "probing…"
			}
		} else if c.hostBusy(host.Name) {
			verb = "probing…"
		}
		hints := []configHint{
			{text: "↑/↓ move", drop: 2},
			{text: verb, drop: 3},
		}
		if host.Action == "" {
			// The verdict reader — a saved host's own verb; the action rows
			// have no verdict to explain.
			hints = append(hints, configHint{text: "e detail", drop: 4})
		}
		hints = append(hints,
			configHint{text: "C assistant", drop: 5},
			configHint{text: "esc close"})
		return "\n" + configHintStyle.Render(c.fitHints(hints)) + "\n"
	}
	if c.selectedUsageRow() {
		// A Usage row (quota agent, per-account evidence, or the empty-accounts
		// row) takes the cursor so the window can scroll the section into view,
		// but nothing on it opens: selectedEntry returns nil, so `↵ edit` and
		// `e explain` are no-ops there (#7047). List only what works — move,
		// the advanced toggle, the assistant, and the exit — so the footer is
		// not advertising verbs the row refuses.
		advanced := "a show advanced"
		if c.showAdvanced {
			advanced = "a hide advanced"
		}
		return "\n" + configHintStyle.Render(c.fitHints([]configHint{
			{text: "↑/↓ move", drop: 2},
			{text: advanced, drop: 1},
			{text: "C assistant", drop: 3},
			{text: "esc close"},
		})) + "\n"
	}
	advanced := "a show advanced"
	if c.showAdvanced {
		advanced = "a hide advanced"
	}
	// Display order left to right; `drop` is the shed order (see fitHints).
	//
	// "C assistant" is the button #2453 adds: the discoverable way to open the
	// config assistant from the config surface. It sits before "esc close" so the
	// exit stays last, where a reader looks for it.
	return "\n" + configHintStyle.Render(c.fitHints([]configHint{
		{text: "↑/↓ move", drop: 2},
		{text: "↵ edit", drop: 3},
		{text: "e explain", drop: 4},
		{text: advanced, drop: 1},
		{text: "C assistant", drop: 5},
		{text: "esc close"},
	})) + "\n"
}
