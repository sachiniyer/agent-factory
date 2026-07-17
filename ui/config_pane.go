package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
)

// ConfigPane is the direct config editor: a form over the config manifest,
// hosted as a full-screen overlay (stateConfigEditor, opened with ",").
//
// It is the DIRECT path to configuration. The config agent (#1928) is the
// conversational path. They are complementary and deliberately share one
// description of config — config.ManifestWithValues — so neither can drift from
// config_types.go or from each other. This pane holds NO key list, no per-key
// type switch, and no copy of the defaults or validation rules: every row it
// renders comes from the manifest, and every write goes to
// config.SetGlobalConfigValue, the same validated/locked/atomic call
// `af config set` makes. Adding a key to config_types.go surfaces it here with
// no edit to this file, which is what TestConfigPaneRendersEveryManifestKey
// pins.
//
// What this pane must never do is imply an edit is live. config.toml is read at
// startup, so a saved value reaches af and the daemon on their next start. The
// pane says so at the moment of the edit, and names the command — see
// config.RestartNotice.
type ConfigPane struct {
	entries []config.ConfigEntry
	path    string

	// rows is the flattened, currently-visible list: tier headings interleaved
	// with entries, rebuilt whenever the advanced toggle or the entries change.
	rows        []configRow
	selectedIdx int

	// showAdvanced gates tier 3. The core is what a user came for; the advanced
	// tier is correct by default and rarely touched, so it stays folded until
	// asked for rather than burying the five keys that matter under twenty.
	showAdvanced bool

	editing bool
	input   textinput.Model

	// scrollTop is the first row-line rendered, so a list taller than the pane
	// keeps the selection on screen. It persists between renders: recomputing it
	// from scratch each frame would snap the view around while the user reads.
	scrollTop int

	// status is the echo of the last write ("key = value") or the validator's
	// error. restartNotice rides alongside a successful write.
	status        string
	statusIsError bool
	restartNotice string

	width    int
	height   int
	hasFocus bool

	// save is the write path, injected so tests drive the REAL
	// config.SetGlobalConfigValue against a temp AGENT_FACTORY_HOME while
	// staying a plain unit test. It is never nil in production
	// (NewConfigPane wires it); a test that swaps it is testing the pane's
	// plumbing, not inventing a second writer.
	save func(key, value string) (*config.SetResult, error)
}

// configRow is one line of the flattened view: either a tier heading or an
// entry. Headings are rows rather than render-time decoration so that
// navigation, which skips them, has a single list to reason about.
type configRow struct {
	heading string
	entry   *config.ConfigEntry
}

// isSelectable reports whether the cursor may land on this row. Headings and
// rows that cannot be edited here are skipped: stopping on a row whose only
// possible action is "you cannot edit this here" wastes the user's keystrokes.
//
// It reads Editable, NOT the manifest's Settable. Settable is true for a dynamic
// family (program_overrides), meaning its LEAVES are settable — the bare key is
// not. Keying off Settable let the cursor land on program_overrides, opened a
// field pre-filled with the map's JSON, and had the writer refuse it on save:
// a dead end the user only discovered by pressing enter.
func (r configRow) isSelectable() bool {
	return r.entry != nil && r.entry.Editable
}

var (
	configTitleStyle    = lipgloss.NewStyle().Bold(true)
	configHeadingStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("62"))
	configKeyStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	configValueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("36"))
	configPurposeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	configReadOnlyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	configSelectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	configErrorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	configOKStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	configNoticeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	configHintStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

// NewConfigPane builds the pane wired to the real write path.
func NewConfigPane() *ConfigPane {
	in := textinput.New()
	in.CharLimit = 512
	in.Blur()
	return &ConfigPane{
		input: in,
		save:  config.SetGlobalConfigValue,
	}
}

// SetEntries loads the manifest rows and the path they were read from. The
// caller supplies them (rather than the pane calling LoadConfig itself) so the
// app decides when to re-read from disk — reopening the editor shows the file as
// it is now, including a hand-edit made since.
func (c *ConfigPane) SetEntries(entries []config.ConfigEntry, path string) {
	c.entries = entries
	c.path = path
	c.rebuildRows()
}

func (c *ConfigPane) SetSize(width, height int) {
	c.width = width
	c.height = height
	c.input.Width = max(20, width-24)
}

func (c *ConfigPane) HasFocus() bool { return c.hasFocus }

// IsEditing reports whether the value field is focused and taking runes.
//
// The app asks before root-routing the configured quit key: while a value is
// being typed, "q" is a character, not an exit. See handleStateConfigEditor.
func (c *ConfigPane) IsEditing() bool { return c.editing }

// SetFocus focuses the pane. Dropping focus is how the overlay closes (the app
// notices and returns to stateDefault), so it also abandons any in-progress
// edit — an unsaved buffer must never survive to be applied later against a row
// the user has since moved off.
func (c *ConfigPane) SetFocus(focus bool) {
	c.hasFocus = focus
	if !focus {
		c.cancelEdit()
		c.clearStatus()
	}
}

// rebuildRows flattens the manifest into the visible list, honoring the
// advanced toggle, and keeps the cursor on something selectable.
func (c *ConfigPane) rebuildRows() {
	c.rows = nil
	for _, tier := range config.ManifestTiers {
		if tier == config.TierAdvanced && !c.showAdvanced {
			continue
		}
		var inTier []config.ConfigEntry
		for _, e := range c.entries {
			if e.Tier == int(tier) {
				inTier = append(inTier, e)
			}
		}
		if len(inTier) == 0 {
			continue
		}
		c.rows = append(c.rows, configRow{heading: config.TierName(tier)})
		for i := range inTier {
			entry := inTier[i]
			c.rows = append(c.rows, configRow{entry: &entry})
		}
	}
	c.clampSelection()
}

// clampSelection moves the cursor onto the nearest selectable row.
func (c *ConfigPane) clampSelection() {
	if len(c.rows) == 0 {
		c.selectedIdx = 0
		return
	}
	if c.selectedIdx >= len(c.rows) {
		c.selectedIdx = len(c.rows) - 1
	}
	if c.selectedIdx < 0 {
		c.selectedIdx = 0
	}
	if c.rows[c.selectedIdx].isSelectable() {
		return
	}
	for i := c.selectedIdx; i < len(c.rows); i++ {
		if c.rows[i].isSelectable() {
			c.selectedIdx = i
			return
		}
	}
	for i := c.selectedIdx; i >= 0; i-- {
		if c.rows[i].isSelectable() {
			c.selectedIdx = i
			return
		}
	}
}

// move steps the cursor by delta over selectable rows only.
func (c *ConfigPane) move(delta int) {
	for i := c.selectedIdx + delta; i >= 0 && i < len(c.rows); i += delta {
		if c.rows[i].isSelectable() {
			c.selectedIdx = i
			return
		}
	}
}

// selectedEntry returns the entry under the cursor, or nil.
func (c *ConfigPane) selectedEntry() *config.ConfigEntry {
	if c.selectedIdx < 0 || c.selectedIdx >= len(c.rows) {
		return nil
	}
	return c.rows[c.selectedIdx].entry
}

// HandleKeyPress routes a key. It returns true when the pane consumed the key.
//
// The caller checks ctrl+c and the configured quit key BEFORE calling this
// (#1727): a text field consumes ctrl+c as "cancel edit", so a pane-first order
// would swallow the quit and wedge the user inside the editor.
func (c *ConfigPane) HandleKeyPress(msg tea.KeyMsg) bool {
	if c.editing {
		return c.handleEditKey(msg)
	}
	switch msg.String() {
	case "esc":
		// Drop focus through SetFocus, NOT by assigning hasFocus: closing must
		// clear the last write's echo and restart notice, and assigning the field
		// directly skipped that — so reopening the editor showed "set
		// default_program = codex" and a restart notice for an edit made minutes
		// ago, as though it had just happened. Every close funnels through one
		// place so a future close path cannot miss the reset.
		c.SetFocus(false)
		return true
	case "up", "k":
		c.move(-1)
		return true
	case "down", "j":
		c.move(1)
		return true
	case "a":
		c.showAdvanced = !c.showAdvanced
		c.rebuildRows()
		return true
	case "enter":
		c.beginEdit()
		return true
	}
	return true
}

// beginEdit opens the value field for the selected key, pre-filled with the
// live value — the value config.CurrentValue rendered, which the write path is
// proven to accept back unchanged (TestCurrentValueRoundTripsThroughConfigSet),
// so saving an untouched field is a no-op rather than a corruption.
func (c *ConfigPane) beginEdit() {
	entry := c.selectedEntry()
	if entry == nil || !entry.Editable {
		return
	}
	c.editing = true
	c.status = ""
	c.restartNotice = ""
	c.input.SetValue(entry.Value)
	c.input.CursorEnd()
	c.input.Focus()
}

// clearStatus drops the last write's echo, its restart notice, and any error.
//
// All three must go together: leaving statusIsError set while clearing the text
// would render an empty error line, and leaving the notice would tell a user to
// restart for an edit they cannot see.
func (c *ConfigPane) clearStatus() {
	c.status = ""
	c.statusIsError = false
	c.restartNotice = ""
}

func (c *ConfigPane) cancelEdit() {
	c.editing = false
	c.input.SetValue("")
	c.input.Blur()
}

// handleEditKey drives the value field. Enter commits, esc abandons; everything
// else is a rune for the input (so ":" and "." in 127.0.0.1:8080 land intact).
func (c *ConfigPane) handleEditKey(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEsc:
		c.cancelEdit()
		return true
	case tea.KeyEnter:
		c.commitEdit()
		return true
	default:
		var cmd tea.Cmd
		c.input, cmd = c.input.Update(msg)
		_ = cmd
		return true
	}
}

// commitEdit writes the edited value through the real path and reports the
// outcome.
//
// Validation is NOT done here. The value goes straight to
// config.SetGlobalConfigValue, which applies the loader's own rules and refuses
// before writing; a rejection surfaces the validator's message verbatim. A
// second copy of the rules in this pane is exactly how a UI comes to accept a
// value the loader rejects at the next startup — the user then meets it as a
// failure to start instead of a red line in a form.
func (c *ConfigPane) commitEdit() {
	entry := c.selectedEntry()
	if entry == nil {
		c.cancelEdit()
		return
	}
	value := c.input.Value()

	result, err := c.save(entry.Key, value)
	if err != nil {
		// Stay in edit mode on a rejection: the bad value is still in the field
		// for the user to correct, which is the whole point of validating before
		// the write rather than after.
		c.status = err.Error()
		c.statusIsError = true
		c.restartNotice = ""
		return
	}

	c.cancelEdit()
	// Echo what was actually written, from the write path's own result — not
	// from what this pane believes it sent. Same contract as `af config set`
	// and the config agent.
	c.status = fmt.Sprintf("set %s = %s", result.Key, result.Value)
	c.statusIsError = false
	if result.RequiresRestart {
		c.restartNotice = config.RestartNotice
	}

	// Reflect the canonical value back into the row so the list shows what the
	// file holds, not what was typed.
	entry.Value = result.Value
	for i := range c.entries {
		if c.entries[i].Key == result.Key {
			c.entries[i].Value = result.Value
		}
	}
}

// String renders the pane, windowed so the selected row is always visible.
//
// The list is taller than the overlay once the advanced tier is open (~31 lines
// of rows in a 20-line pane), so without a window the cursor walks off the
// bottom and the user is editing a row they cannot see — and a selection you
// cannot see is one you will change by accident.
func (c *ConfigPane) String() string {
	header := c.renderHeader()
	footer := c.renderStatus() + c.renderHints()

	rowLines, selStart, selEnd := c.renderRowLines()

	// Reserve the two cue rows unconditionally. Making the budget depend on
	// whether the cues happen to show is circular — and it would make the list
	// grow and shrink by a line as the user scrolls past either end.
	budget := c.height - countLines(header) - countLines(footer) - cueRows
	visible, above, below := c.window(rowLines, selStart, selEnd, budget)

	var b strings.Builder
	b.WriteString(header)
	if above > 0 {
		b.WriteString(configHintStyle.Render(fmt.Sprintf("  ↑ %d more", above)))
		b.WriteString("\n")
	}
	b.WriteString(strings.Join(visible, "\n"))
	if len(visible) > 0 {
		b.WriteString("\n")
	}
	if below > 0 {
		b.WriteString(configHintStyle.Render(fmt.Sprintf("  ↓ %d more", below)))
		b.WriteString("\n")
	}
	b.WriteString(footer)
	return b.String()
}

// renderHeader renders the title and the file being edited.
func (c *ConfigPane) renderHeader() string {
	var b strings.Builder
	b.WriteString(configTitleStyle.Render("Config"))
	if c.path != "" {
		b.WriteString(configPurposeStyle.Render("  " + c.path))
	}
	b.WriteString("\n\n")
	return b.String()
}

// renderRowLines renders every row to lines, reporting the line span of the
// selected row so the window can keep it on screen. A row is not one line: the
// selected row also shows its purpose and its allowed values, so the span is
// what must stay visible, not a single index.
func (c *ConfigPane) renderRowLines() (lines []string, selStart, selEnd int) {
	selStart, selEnd = -1, -1
	for i, row := range c.rows {
		start := len(lines)
		if row.entry == nil {
			lines = append(lines, configHeadingStyle.Render(strings.ToUpper(row.heading)))
		} else {
			rendered := c.renderEntryRow(i, row, *row.entry)
			lines = append(lines, strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")...)
		}
		if i == c.selectedIdx {
			selStart, selEnd = start, len(lines)
		}
	}
	return lines, selStart, selEnd
}

// window returns the slice of rowLines to render, plus how many lines are hidden
// above and below, scrolling only as far as it must to reveal the selection.
//
// It moves the view by the MINIMUM needed rather than centering the selection:
// centering would shift the whole list on every keypress, which reads as the
// content moving under the cursor instead of the cursor moving through it.
func (c *ConfigPane) window(rowLines []string, selStart, selEnd, budget int) (visible []string, above, below int) {
	if budget <= 0 || len(rowLines) <= budget {
		// It all fits (or the pane has no size yet — SetSize has not run, as in a
		// unit test): render everything rather than guessing a window.
		c.scrollTop = 0
		return rowLines, 0, 0
	}

	if selStart >= 0 {
		if selStart < c.scrollTop {
			c.scrollTop = selStart
		}
		// A selected row taller than the window (a long purpose line) pins to its
		// top: showing its tail and hiding its key would be worse than clipping.
		if selEnd > c.scrollTop+budget {
			c.scrollTop = min(selStart, selEnd-budget)
		}
	}
	maxTop := len(rowLines) - budget
	c.scrollTop = max(0, min(c.scrollTop, maxTop))

	return rowLines[c.scrollTop : c.scrollTop+budget], c.scrollTop, len(rowLines) - (c.scrollTop + budget)
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
		cursor = configSelectedStyle.Render("› ")
	}
	b.WriteString(cursor)

	key := configKeyStyle.Render(e.Key)
	if selected {
		key = configSelectedStyle.Render(e.Key)
	}
	b.WriteString(key)
	b.WriteString("  ")

	switch {
	case selected && c.editing:
		b.WriteString(c.input.View())
	case !e.Editable:
		b.WriteString(configValueStyle.Render(c.displayValue(e)))
	default:
		b.WriteString(configValueStyle.Render(c.displayValue(e)))
	}
	b.WriteString("\n")

	if !e.Editable && e.EditHint != "" {
		// Say WHY it cannot be edited here, and what to do instead — on its own
		// wrapped line, not inline. The hint is derived from the real allowlist,
		// so for a dynamic family it names the command that DOES work rather than
		// sending the user to a text editor for something af can do. That makes it
		// long ("set one entry: af config set program_overrides.<name> <value>"),
		// and inline it pushed the row to 106 cells in a 72-cell pane — which the
		// frame would wrap, breaking the height window's line count.
		b.WriteString(c.wrapIndented("· "+e.EditHint, configReadOnlyStyle))
	}

	if selected {
		b.WriteString(c.wrapIndented(e.Purpose, configPurposeStyle))
		if len(e.Enum) > 0 && e.Type != "table" {
			b.WriteString(c.wrapIndented("one of: "+strings.Join(e.Enum, " · "), configHintStyle))
		}
	}
	return b.String()
}

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
//     row after it. Truncating is honest here precisely because the key is
//     read-only: the file is where you edit it, and the row says so.
//
// This is the same split CurrentValue documents: what you SHOW and what you can
// SAVE BACK are different, and conflating them is how `""` ends up in a user's
// config.toml.
func (c *ConfigPane) displayValue(e config.ConfigEntry) string {
	if e.Value == "" {
		return "(unset)"
	}
	// Leave room for the cursor and the key. The read-only hint no longer shares
	// this line (it wraps onto its own), so the only competition is the key.
	budget := c.width - len(e.Key) - 8
	if budget < 12 {
		budget = 12
	}
	runes := []rune(e.Value)
	if len(runes) <= budget {
		return e.Value
	}
	return string(runes[:budget-1]) + "…"
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

// renderStatus renders the echo of the last write, or the validator's error,
// plus the restart notice.
//
// The notice is shown AT THE MOMENT OF THE EDIT rather than on close or in a
// banner: a user who sets a value and looks away has been told exactly when they
// were in a position to act on it.
func (c *ConfigPane) renderStatus() string {
	if c.status == "" {
		return ""
	}
	// Wrapped for the same reason the purpose is: these are the longest strings
	// on screen (a validator error runs to 200+ characters), and an unwrapped one
	// makes countLines undercount the footer, which steals the window's budget.
	var b strings.Builder
	if c.statusIsError {
		b.WriteString(c.wrap(c.status, configErrorStyle))
		return b.String()
	}
	b.WriteString(c.wrap(c.status, configOKStyle))
	if c.restartNotice != "" {
		b.WriteString(c.wrap(c.restartNotice, configNoticeStyle))
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
		return "\n" + configHintStyle.Render("↵ save · esc cancel") + "\n"
	}
	advanced := "a show advanced"
	if c.showAdvanced {
		advanced = "a hide advanced"
	}
	return "\n" + configHintStyle.Render("↑/↓ move · ↵ edit · "+advanced+" · esc close") + "\n"
}

// SetEditValueForTest and EditValueForTest expose the value field's buffer to
// the app package's tests, which drive the REAL handleStateConfigEditor (where
// the #1961 quit-key bug class lives) and must assert what actually reached the
// field. The pane's own tests reach c.input directly; app's cannot.
func (c *ConfigPane) SetEditValueForTest(v string) { c.input.SetValue(v) }
func (c *ConfigPane) EditValueForTest() string     { return c.input.Value() }
