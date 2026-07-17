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
// read-only (hand-edited) keys are skipped: stopping on a row whose only
// possible action is "you cannot edit this here" wastes the user's keystrokes.
func (r configRow) isSelectable() bool {
	return r.entry != nil && r.entry.Settable
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
		c.status = ""
		c.restartNotice = ""
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
		// Drop focus; the app reads that as "close the overlay".
		c.hasFocus = false
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
	if entry == nil || !entry.Settable {
		return
	}
	c.editing = true
	c.status = ""
	c.restartNotice = ""
	c.input.SetValue(entry.Value)
	c.input.CursorEnd()
	c.input.Focus()
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

// String renders the pane.
func (c *ConfigPane) String() string {
	var b strings.Builder

	b.WriteString(configTitleStyle.Render("Config"))
	if c.path != "" {
		b.WriteString(configPurposeStyle.Render("  " + c.path))
	}
	b.WriteString("\n\n")

	for i, row := range c.rows {
		if row.entry == nil {
			b.WriteString(configHeadingStyle.Render(strings.ToUpper(row.heading)))
			b.WriteString("\n")
			continue
		}
		b.WriteString(c.renderEntryRow(i, row, *row.entry))
	}

	b.WriteString("\n")
	b.WriteString(c.renderStatus())
	b.WriteString(c.renderHints())
	return b.String()
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
	case !e.Settable:
		// Say WHY it is not editable here, rather than showing a dead row. The
		// key is real and the file is hand-editable by design.
		b.WriteString(configValueStyle.Render(c.displayValue(e)))
		b.WriteString(configReadOnlyStyle.Render(" · hand-edited in config.toml"))
	default:
		b.WriteString(configValueStyle.Render(c.displayValue(e)))
	}
	b.WriteString("\n")

	if selected {
		b.WriteString("    ")
		b.WriteString(configPurposeStyle.Render(e.Purpose))
		b.WriteString("\n")
		if len(e.Enum) > 0 && e.Type != "table" {
			b.WriteString("    ")
			b.WriteString(configHintStyle.Render("one of: " + strings.Join(e.Enum, " · ")))
			b.WriteString("\n")
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
	// Leave room for the cursor, the key, and the read-only suffix.
	budget := c.width - len(e.Key) - 34
	if budget < 12 {
		budget = 12
	}
	runes := []rune(e.Value)
	if len(runes) <= budget {
		return e.Value
	}
	return string(runes[:budget-1]) + "…"
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
	var b strings.Builder
	if c.statusIsError {
		b.WriteString(configErrorStyle.Render(c.status))
		b.WriteString("\n")
		return b.String()
	}
	b.WriteString(configOKStyle.Render(c.status))
	b.WriteString("\n")
	if c.restartNotice != "" {
		b.WriteString(configNoticeStyle.Render(c.restartNotice))
		b.WriteString("\n")
	}
	return b.String()
}

func (c *ConfigPane) renderHints() string {
	if c.editing {
		return configHintStyle.Render("\n↵ save · esc cancel")
	}
	advanced := "a show advanced"
	if c.showAdvanced {
		advanced = "a hide advanced"
	}
	return configHintStyle.Render("\n↑/↓ move · ↵ edit · " + advanced + " · esc close")
}

// SetEditValueForTest and EditValueForTest expose the value field's buffer to
// the app package's tests, which drive the REAL handleStateConfigEditor (where
// the #1961 quit-key bug class lives) and must assert what actually reached the
// field. The pane's own tests reach c.input directly; app's cannot.
func (c *ConfigPane) SetEditValueForTest(v string) { c.input.SetValue(v) }
func (c *ConfigPane) EditValueForTest() string     { return c.input.Value() }
