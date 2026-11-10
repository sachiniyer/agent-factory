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
// renders comes from the manifest, and every write goes through the pane's one
// save seam. Adding a key to config_types.go surfaces it here with no edit to
// this file, which is what TestConfigPaneRendersEveryManifestKey pins.
//
// Both the rows and the save follow --daemon-url/AF_DAEMON_URL since #3708, so
// `,` administers whichever daemon the session is attached to — the same one
// thing the flag means for `af config set` (#3679). The target decision, and the
// reason both halves have to move together, live in config_target.go; this file
// is the form over whatever they hand it.
//
// What this pane must never do is misstate WHEN an edit takes effect. Since #2480
// most keys reach a running daemon in place, but not all do, so the pane shows the
// honest per-key answer at the moment of the edit — live now, next daemon start, or
// next af launch — from config.EffectNotice, never one canned sentence and never a
// command to run (#2479).
type ConfigPane struct {
	entries []config.ConfigEntry
	// location is what the header says these rows came from: a config.toml path
	// locally, and the daemon's URL followed by ITS path under a remote target
	// (#3708). Not a path, therefore, and neither the order nor the absence of a
	// `~` abbreviation is incidental — see remoteConfigLocation for both.
	location string
	// project is the scope the rows were read in (#7009): the active project
	// when its effective values were honored, nil for a global read or one that
	// fell back to global. Explain answers in this scope, never another.
	project *config.RepoContext

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

	// editSeed is the field's value immediately after beginEdit seeds it —
	// read back from the input so it reflects whatever the textinput sanitizer
	// did to the seed (whitespace flattened AND other control runes dropped),
	// not a separately-computed approximation. commitEdit's untouched-field
	// guard compares the live field against this. dirty tracks whether the
	// user typed anything since beginEdit: because flattening is not injective,
	// a value-equality check alone cannot tell an untouched field from one the
	// user cleared and retyped into the same flattened form (which is a real
	// edit — replacing a multi-line value with that single-line text), so the
	// guard only fires when nothing was typed.
	editSeed string
	dirty    bool

	// scrollTop is the first row-line rendered, so a list taller than the pane
	// keeps the selection on screen. It persists between renders: recomputing it
	// from scratch each frame would snap the view around while the user reads.
	scrollTop int

	// status is the echo of the last write ("key = value") or the validator's
	// error. restartNotice rides alongside a successful write.
	status        string
	statusIsError bool
	restartNotice string

	// statusLine is the one line renderStatus draws: the NEWEST outcome any
	// section produced. Every writer feeds it through its channel's setter, so
	// a stale outcome can never hide a newer one by precedence order — the
	// rule a pairwise precedence between the three channels could not hold
	// both ways (#5571 review). The per-channel status fields stay for their
	// own readers (the add form's footer, the account-outcome accessor); this
	// is what the shared line shows.
	statusLine statusEvent

	width    int
	height   int
	hasFocus bool

	// save is the write path, injected so tests drive the REAL
	// applyingConfigSet against a temp AGENT_FACTORY_HOME while staying a
	// plain unit test. It is never nil in production (NewConfigPane wires
	// it); a test that swaps it is testing the pane's plumbing, not
	// inventing a second writer.
	save func(key, value string) (result *config.SetResult, notice string, err error)

	// explain is the provenance read path (#4803), injected on the same terms:
	// NewConfigPane wires ExplainConfigForEditor, which resolves in-process
	// locally and through the targeted daemon's ExplainConfig under a remote
	// target. The rows say WHAT a value is; this answers WHERE it came from.
	explain func(key string, repo *config.RepoContext) (*config.ResolvedValue, error)

	// reread re-resolves the project's rows after a global save (#7009),
	// injected on the same terms: NewConfigPane wires rereadConfigForEditor,
	// the same read the app opens the pane with. A global write can move a
	// project's effective value in ways no single row predicts — a merged map
	// inherits the entries the project does not override, and a global-only
	// key such as root_agents feeds another row's layer — so only a fresh read
	// knows what the project uses now. honored is the scope the read got.
	reread func(repo *config.RepoContext) (entries []config.ConfigEntry, honored *config.RepoContext, err error)

	// editHosts is the hosts_edit write path (#5571 W10.5), injected on the
	// same terms as save: NewConfigPane wires applyingHostsEdit, which routes
	// the local daemon.EditGlobalHosts or the remote health-gated
	// SetConfigValue — the target decision lives in config_target.go.
	editHosts func(edit config.HostsEdit) (result *config.SetResult, notice string, err error)

	// The explain sub-view (config_pane_explain.go): 'e' on a key row swaps the
	// list for the key's full candidate trace — which layer won, which were
	// shadowed, absent, or disallowed. savedScrollTop holds the list's scroll
	// offset so esc returns the reader to the row they explained.
	explaining   bool
	explainKey   string
	explainLines []string
	explainValue *config.ResolvedValue
	// explainHost holds the row a host-check explain is showing (#5571): the
	// view is the explain view's scrollable reader pointed at a host verdict
	// instead of a key trace, so explainValue is nil for it and SetSize
	// rewraps through wrappedHostExplainLines.
	explainHost    *HostRow
	savedScrollTop int

	// accounts is the Accounts section (#3385): agent identities, rendered below
	// the config tiers and visibly not config rows. It is a separate struct rather
	// than more fields here because it is a separate domain — nothing in it goes
	// through the config write path — and config_pane_accounts.go holds all of it.
	accounts accountsSection

	// quota is the Usage section (#2983): the daemon's per-agent usage report,
	// rendered below Accounts. Same shape as accounts — a separate domain that
	// never touches the write path, held by config_pane_quota.go.
	quota quotaSection

	// hosts is the Hosts section (#5571 W10.5): the operator's sandbox host
	// list, derived from the `hosts`/`default_host` manifest rows and worked
	// through the same daemon paths `af hosts` rides. Held by
	// config_pane_hosts.go.
	hosts hostsSection

	// assistantRequested is set when the user presses the assistant key in normal
	// mode. The pane cannot spawn the config agent itself — that is a daemon round
	// trip owned by the app package — so it records the intent and the app reads it
	// through TakeAssistantRequest after routing the key (#2453). It is a request,
	// not a mode: the pane never enters an assistant state, it just asks the host
	// to open one and closes.
	assistantRequested bool
}

// The statusLine field and its setStatus/clearStatus/renderStatus machinery
// live in config_pane_status.go — one line, fed by every channel's setter.

// configRow is one line of the flattened view: either a tier heading or an
// entry. Headings are rows rather than render-time decoration so that
// navigation, which skips them, has a single list to reason about.
type configRow struct {
	heading string
	entry   *config.ConfigEntry
	// account is set for a row of the Accounts section (#3385) — an agent
	// identity, not a config key. Exactly one of heading, entry, account,
	// quota and host is meaningful on any row.
	account *AccountRow
	// quota is set for a row of the Usage section (#2983) — a daemon-rendered
	// report line. It takes the cursor so the scroll window can reach it, but
	// nothing on it opens for editing.
	quota *QuotaRow
	// host is set for a row of the Hosts section (#5571 W10.5) — a saved host
	// or one of the section's action rows (add, check-all), derived from the
	// `hosts`/`default_host` manifest entries themselves.
	host *HostRow
}

// isSelectable reports whether the cursor may land on this row. Every manifest
// entry is editable since #3345; only tier headings are skipped. A pre-#3345
// daemon may reject a newly supported structured save during version skew; that
// rejection stays visible in this real field. Turning the row read-only would
// restore the class #3345 explicitly removed, while a local-write fallback
// would bypass the running daemon's lifecycle admission gate.
//
// Quota rows take the cursor even though there is nothing to edit on them
// (#2983): the selection is the only scroll driver this list has — window()
// scrolls just far enough to reveal it — so a non-selectable Usage section
// below the last editable row would render but never scroll into view. Enter
// on one is a no-op because selectedEntry returns nil for it.
func (r configRow) isSelectable() bool {
	return r.entry != nil || r.account != nil || r.quota != nil || r.host != nil
}

var (
	configTitleStyle    = lipgloss.NewStyle().Bold(true)
	configHeadingStyle  = lipgloss.NewStyle().Bold(true).Foreground(activeTheme.Ink)
	configKeyStyle      = lipgloss.NewStyle().Foreground(activeTheme.Ink)
	configValueStyle    = lipgloss.NewStyle().Foreground(activeTheme.Ink)
	configLocationStyle = lipgloss.NewStyle().Foreground(activeTheme.InkMuted)
	configPurposeStyle  = lipgloss.NewStyle().Foreground(activeTheme.Ink)
	configSelectedStyle = lipgloss.NewStyle().Bold(true).Background(activeTheme.SurfaceRaised).Foreground(activeTheme.Ink)
	configErrorStyle    = lipgloss.NewStyle().Foreground(activeTheme.Dead)
	configOKStyle       = lipgloss.NewStyle().Foreground(activeTheme.Ink)
	configNoticeStyle   = lipgloss.NewStyle().Foreground(activeTheme.Ink)
	configHintStyle     = lipgloss.NewStyle().Foreground(activeTheme.Ink)
	configLimitStyle    = lipgloss.NewStyle().Foreground(activeTheme.LimitReached)
)

// NewConfigPane builds the pane wired to the real write path.
func NewConfigPane() *ConfigPane {
	in := textinput.New()
	// NO CharLimit. It is a truncating limit, not a validating one: SetValue
	// silently drops everything past it, so pre-filling a longer value and
	// pressing enter — without typing a character — wrote the truncated version
	// back and destroyed the rest. Reproduced at 920 chars → 512.
	//
	// An editor's first duty is not to corrupt what it was not asked to change.
	// Length is the loader's business, not a text field's; the pane never
	// shortens a value it will write. Display is bounded elsewhere and only for
	// display: the input scrolls horizontally (in.Width), and the LIST truncates
	// with displayValue, which never feeds the writer.
	in.Blur()
	return &ConfigPane{
		input:     in,
		save:      applyingConfigSet,
		explain:   ExplainConfigForEditor,
		reread:    rereadConfigForEditor,
		editHosts: applyingHostsEdit,
	}
}

// SetEntries loads the manifest rows, the location they were read from, and the
// project scope they were read in (nil for global). The caller supplies them
// (rather than the pane reading config itself) so the app decides when to
// re-read — reopening the editor shows config as it is now, including a
// hand-edit made since — and so the pane stays a form regardless of which daemon
// answered (ReadConfigForEditor).
func (c *ConfigPane) SetEntries(entries []config.ConfigEntry, location string, project *config.RepoContext) {
	c.entries = visibleAppearanceEntries(entries)
	c.location = location
	c.project = project
	c.rebuildRows()
}

func (c *ConfigPane) SetSize(width, height int) {
	resized := width != c.width
	c.width = width
	c.height = height
	c.sizeEditField()
	c.sizeHostFields()
	c.sizeAccountField()
	// An open trace was wrapped for the old width: lines emitted wider than
	// the pane wrap or clamp in the dialog while the scroll window still
	// budgets against the old count, leaving parts of it unreachable. Keep
	// the resolved value and rewrap it so the line math stays exact (#4803
	// review).
	if resized && c.explaining {
		if c.explainValue != nil {
			c.explainLines = c.wrappedExplainLines(*c.explainValue)
		} else if c.explainHost != nil {
			c.explainLines = c.wrappedHostExplainLines(*c.explainHost)
		}
		c.scrollTop = min(max(0, len(c.explainLines)-1), c.scrollTop)
	}
}

func (c *ConfigPane) HasFocus() bool { return c.hasFocus }

// IsEditing reports whether the value field is focused and taking runes.
//
// The app asks before root-routing the configured quit key: while a value is
// being typed, "q" is a character, not an exit. See handleStateConfigEditor.
// It reports the ACCOUNT name field too (#3385): that field takes arbitrary
// runes, and "q" in an account name must be a character rather than an exit for
// exactly the same reason it is in a config value.
func (c *ConfigPane) IsEditing() bool { return c.editing || c.accounts.registering || c.hosts.adding }

// SetFocus focuses the pane. Dropping focus is how the overlay closes (the app
// notices and returns to stateDefault), so it also abandons any in-progress
// edit — an unsaved buffer must never survive to be applied later against a row
// the user has since moved off.
func (c *ConfigPane) SetFocus(focus bool) {
	c.hasFocus = focus
	if !focus {
		c.accounts.busy = false
		c.cancelEdit()
		c.cancelRegister()
		c.cancelHostAdd()
		c.hosts.busy = nil
		c.hosts.pending = nil
		c.hosts.checkAsked = false
		c.clearHostStatus()
		c.clearAccountStatus()
		c.explaining = false
		c.explainLines = nil
		c.explainValue = nil
		c.explainHost = nil
		c.clearStatus()
		// A pending request is consumed the moment the app opens the assistant, so
		// one that survives to a close was never taken (the pane was dismissed
		// another way). Drop it so the next open cannot inherit a stale intent.
		c.assistantRequested = false
	}
}

// TakeAssistantRequest reports whether the user asked to open the config
// assistant since the last call, clearing the flag. The app calls it after
// routing a key to the pane: a true here means close this overlay and spawn the
// assistant (#2453). Read-and-clear so the request fires exactly once.
func (c *ConfigPane) TakeAssistantRequest() bool {
	req := c.assistantRequested
	c.assistantRequested = false
	return req
}

// rebuildRows flattens the manifest into the visible list, honoring the
// advanced toggle, and keeps the cursor on something selectable.
func (c *ConfigPane) rebuildRows() {
	selectedAccount := c.selectedAccount()
	var selectedHost HostRow
	if host := c.selectedHost(); host != nil {
		selectedHost = *host
	}
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
		heading := config.TierName(tier)
		c.rows = append(c.rows, configRow{heading: strings.ToUpper(heading[:1]) + heading[1:]})
		for i := range inTier {
			entry := inTier[i]
			c.rows = append(c.rows, configRow{entry: &entry})
		}
	}
	// Hosts (#5571 W10.5) follows the tiers: it is config content — the
	// section renders the same `hosts`/`default_host` keys the tiers hold,
	// so it belongs ahead of the daemon-owned domains. Usage then Accounts
	// keep the tail, newest last — and the account register rows stay the
	// list's last selectable rows, the invariant the register flow's
	// navigate-to-bottom drives rely on. Usage sits AHEAD of Accounts for
	// exactly this reason: its rows take the cursor (selection is the list's
	// only scroll driver), so appending them after Accounts would make the
	// last selectable row a quota row, where Enter is a no-op, and the
	// navigate-to-bottom register drive would dead-end. Putting Usage first
	// keeps Accounts — and its register rows — at the selectable tail.
	c.appendHostRows()
	c.appendQuotaRows()
	c.appendAccountRows()
	if selectedAccount != nil {
		found := false
		for i, row := range c.rows {
			account := row.account
			if account != nil && account.Agent == selectedAccount.Agent && account.Name == selectedAccount.Name && account.Register == selectedAccount.Register {
				c.selectedIdx = i
				found = true
				break
			}
		}
		if !found && c.accounts.registering {
			c.cancelRegister() // the editor's row disappeared; never retain an invisible field
		}
	}
	if selectedHost.Name != "" || selectedHost.Action != "" {
		for i, row := range c.rows {
			host := row.host
			if host == nil {
				continue
			}
			if host.Action != "" {
				if selectedHost.Action == host.Action {
					c.selectedIdx = i
					break
				}
			} else if selectedHost.Action == "" && host.Name == selectedHost.Name {
				c.selectedIdx = i
				break
			}
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
	// The account name field takes runes before anything else reads them, for the
	// same reason the value field does: "a" is a character here, not the advanced
	// toggle, and "C" is a character, not the assistant.
	if c.accounts.registering {
		return c.handleRegisterKey(msg)
	}
	// The host add form takes runes next, on the same terms — its fields type
	// the same letters the list reads as verbs.
	if c.hosts.adding {
		return c.handleHostAddKey(msg)
	}
	// The explain view is a reader, not a form: scroll and leave. It sits above
	// the account-row routing because 'e' is its own way out, not an account verb.
	if c.explaining {
		return c.handleExplainKey(msg)
	}
	// An account row answers enter itself — with a login or a register field —
	// rather than falling through to beginEdit, which would open a config value
	// editor over a row that has no config key.
	if c.handleAccountKey(msg) {
		return true
	}
	// A Hosts row answers enter, d and D itself — a probe, a remove or a
	// default pick, never a config value editor.
	if c.handleHostKey(msg) {
		return true
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
	case "C":
		// Ask the host to open the config assistant. Uppercase C matches the global
		// hotkey the assistant already has (keys.KeyConfigAgent), and it is free
		// here because value text is only typed in edit mode, which this switch does
		// not reach. The pane records the request and closes; the app spawns the
		// agent (see TakeAssistantRequest). This is the "button" #2453 asks for —
		// the always-available way into the assistant from the config surface, so a
		// user editing config never has to know the global shortcut exists.
		//
		// Drop focus FIRST — SetFocus(false) clears a pending request as part of its
		// reset — then record this one, so closing the overlay does not wipe the
		// intent that is closing it.
		c.SetFocus(false)
		c.assistantRequested = true
		return true
	case "e":
		c.beginExplain()
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
// so saving an untouched field is a no-op rather than a corruption. A
// scope-marked row opens on its GLOBAL value (editSeedValue): that is the value
// the field writes.
func (c *ConfigPane) beginEdit() {
	entry := c.selectedEntry()
	if entry == nil {
		return
	}
	c.editing = true
	c.clearStatus()
	c.sizeEditField()
	// Seed the field with the pre-flattened value. bubbles v0.20.0's
	// textinput.SetValue runs a sanitizer that folds newlines/tabs to spaces,
	// so seeding with the raw multi-line value would flatten it inside the
	// field and let commitEdit's guard read the untouched field as edited.
	c.input.SetValue(flattenToOneLine(editSeedValue(*entry)))
	// Read the seed back AFTER the sanitizer runs: it drops not just
	// whitespace but every unicode.IsControl rune (e.g. ESC from a TOML
	// \u001b escape), so a flatten-only seed would diverge from the field
	// and rewrite an untouched value that lost a control rune.
	c.editSeed = c.input.Value()
	c.dirty = false
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
		before := c.input.Value()
		c.input, cmd = c.input.Update(msg)
		_ = cmd
		if c.input.Value() != before {
			c.dirty = true
		}
		return true
	}
}

// commitEdit writes the edited value through the real path and reports the
// outcome.
//
// Validation is NOT done here. The value goes straight to the write path,
// where config.SetGlobalConfigValue applies the loader's own rules and refuses
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

	// An untouched field writes NOTHING. This is belt-and-braces, not an
	// optimization: a key nobody edited must never be rewritten, so no future
	// mangling bug in this pane can reach a value the user only looked at. It is
	// also honest — a no-op write still echoes and still raises the restart
	// notice, telling the user they changed something when they did not.
	//
	// The GUARANTEE against truncation is the absent CharLimit above, not this:
	// a truncating field would make value != editSeed, which reads as an edit
	// and would write straight through this check.
	//
	// Two no-op signals, either sufficient:
	//   - value == editSeedValue(*entry): the field holds the raw stored value again,
	//     a true no-op even when dirty is true (e.g. the user edited a
	//     sanitizer-stable single-line value and restored it — without this
	//     arm that would run the config writer / daemon apply path for no
	//     semantic change).
	//   - !c.dirty && value == c.editSeed: the field is untouched since
	//     beginEdit. editSeed is read back AFTER the textinput sanitizer ran,
	//     so control runes the sanitizer drops (not just the whitespace
	//     flattenToOneLine folds) are gone from both sides. flattenToOneLine
	//     is not injective (a\nb and "a b" both flatten to "a b"), so a
	//     value-equality guard alone would block a genuine edit that lands
	//     back on the flattened seed; dirty, set only when the input value
	//     changes, lets that write through. A multi-line entry.Value can
	//     never equal the single-line field, so the first arm does not weaken
	//     this. See TestConfigPaneMultiLineValueUntouchedSaveIsNoOp and TestConfigPaneEditRestoreIsNoOp.
	if value == editSeedValue(*entry) || (!c.dirty && value == c.editSeed) {
		c.cancelEdit()
		return
	}

	result, notice, err := c.save(entry.Key, value)
	// The write produced an outcome either way, so it retires the Hosts
	// section's status too: one line reports the LATEST event, and a stale
	// check outcome must never sit in front of the validator's refusal or the
	// save's echo and notice (#6718 review).
	c.clearHostStatus()
	if err != nil {
		// Stay in edit mode on a rejection: the bad value is still in the field
		// for the user to correct, which is the whole point of validating before
		// the write rather than after.
		c.setStatus(err.Error(), true, "")
		return
	}

	c.cancelEdit()
	// Reflect the write back into the rows so the list shows what af uses, not
	// what was typed: the canonical value on a global row, and a fresh project
	// read when the rows are a project's (see reread).
	if stale := c.refreshAfterSave(result.Key, result.Value, entry.Scope != ""); stale != "" {
		notice = joinNotices(notice, stale)
	}
	// Echo what was actually written, from the write path's own result — not
	// from what this pane believes it sent. Same contract as `af config set`
	// and the config agent. The per-key effect notice (#2480) rides along:
	// live now, next daemon start, or next af launch — the honest answer for
	// THIS key, computed by the write path.
	c.setStatus(fmt.Sprintf("set %s = %s", result.Key, result.Value), false, notice)
	// The `hosts`/`default_host` rows remain editable keys AND feed the Hosts
	// section (#5571 W10.5): rebuild so a hand-edited table row re-derives the
	// section's rows instead of leaving last save's destinations on screen.
	c.rebuildRows()
}

// String renders the pane, windowed so the selected row is always visible.
//
// The list is taller than the overlay once the advanced tier is open (~31 lines
// of rows in a 20-line pane), so without a window the cursor walks off the
// bottom and the user is editing a row they cannot see — and a selection you
// cannot see is one you will change by accident.
func (c *ConfigPane) String() string {
	if c.hosts.adding {
		return c.renderHostAddView()
	}
	if c.explaining {
		return c.renderExplainView()
	}
	header := c.renderHeader()
	footer := c.renderStatus() + c.renderHints()
	rowLines, selStart, selEnd := c.renderRowLines()
	return c.renderWindowed(header, footer, rowLines, selStart, selEnd)
}

// renderWindowed renders a header, a scrollable window of lines with overflow
// cues, and a footer — the one layout String() and the explain view share, so
// both scroll and budget identically.
//
// The two cue rows are reserved unconditionally. Making the budget depend on
// whether the cues happen to show is circular — and it would make the list
// grow and shrink by a line as the user scrolls past either end.
func (c *ConfigPane) renderWindowed(header, footer string, lines []string, selStart, selEnd int) string {
	budget := c.height - countLines(header) - countLines(footer) - cueRows
	visible, above, below := c.window(lines, selStart, selEnd, budget)

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

// renderHeader renders the title and where the rows came from — a path locally,
// the daemon URL and then its path under a remote target.
func (c *ConfigPane) renderHeader() string {
	var b strings.Builder
	b.WriteString(configTitleStyle.Render("Config"))
	if c.location != "" {
		b.WriteString(configLocationStyle.Render("  " + c.location))
	}
	// The location is the one part of this line the pane does not control the
	// length of, so it is clipped rather than allowed to wrap (#3430): a wrapped
	// header costs a list row at exactly the width where rows are scarcest, and
	// makes countLines lie. What survives the clip is therefore load-bearing, and
	// it is why remoteConfigLocation leads with the daemon — "Config" plus a
	// clipped path still says which file is open, but only once you know which
	// MACHINE it is on.
	return c.fitPaneLine(b.String()) + "\n\n"
}

func (c *ConfigPane) renderRowLines() (lines []string, selStart, selEnd int) {
	selStart, selEnd = -1, -1
	for i, row := range c.rows {
		start := len(lines)
		switch {
		case row.account != nil:
			rendered := c.renderAccountRow(i, *row.account)
			lines = append(lines, strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")...)
		case row.quota != nil:
			rendered := c.renderQuotaRow(i, *row.quota)
			lines = append(lines, strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")...)
		case row.host != nil:
			rendered := c.renderHostRow(i, *row.host)
			lines = append(lines, strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")...)
		case row.entry != nil:
			rendered := c.renderEntryRow(i, row, *row.entry)
			lines = append(lines, strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")...)
		default:
			lines = append(lines, configHeadingStyle.Render(row.heading))
			if row.heading == accountsHeading {
				if c.accounts.loading {
					lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented("Loading accounts…", configHintStyle), "\n"), "\n")...)
				} else if c.accounts.unavailable != "" {
					lines = append(lines, strings.Split(c.renderAccountsUnavailable(), "\n")...)
				} else if c.accounts.empty {
					lines = append(lines, strings.Split(DialogRecoveryContent("No accounts", "", "Select register · enter to add an account.", false, c.width), "\n")...)
				} else {
					lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented(accountsHeadingNote, configHintStyle), "\n"), "\n")...)
				}
			}
			if row.heading == quotaHeading {
				switch {
				case c.quota.loading:
					lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented("Loading usage…", configHintStyle), "\n"), "\n")...)
				case c.quota.unavailable != "":
					lines = append(lines, strings.Split(c.renderQuotaUnavailable(), "\n")...)
				default:
					lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented(quotaHeadingNote, configHintStyle), "\n"), "\n")...)
					// The mover line is daemon-rendered and only present when
					// the auto-handoff machinery is engaged or left records —
					// a halt in it must surface here, never silently (#6776).
					if c.quota.mover != "" {
						lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented("mover: "+c.quota.mover, configHintStyle), "\n"), "\n")...)
					}
					if len(c.quota.rows) == 0 {
						lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented("No agent CLIs are configured, so there is nothing to report.", configHintStyle), "\n"), "\n")...)
					}
				}
				// Completeness caveats ride under the heading note: a skipped or
				// unparseable record may be hiding exactly the parked session the
				// report exists to find, so the warning goes where the reader
				// sees it rather than into a log.
				for _, warning := range c.quota.warnings {
					lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented("warning: "+warning, configErrorStyle), "\n"), "\n")...)
				}
			}
			if row.heading == hostsHeading {
				switch {
				case c.hosts.parseError != "":
					lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented("The host table could not be read: "+c.hosts.parseError, configErrorStyle), "\n"), "\n")...)
				default:
					lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented(hostsHeadingNote, configHintStyle), "\n"), "\n")...)
					if len(c.rows) == i+1 || c.rows[i+1].host == nil || c.rows[i+1].host.Action != "" {
						lines = append(lines, strings.Split(strings.TrimSuffix(c.wrapIndented(hostsEmptyNote, configHintStyle), "\n"), "\n")...)
					}
				}
			}
		}
		if i == c.selectedIdx {
			selStart, selEnd = start, len(lines)
		}
	}
	return lines, selStart, selEnd
}

// editSeedValue is the value the edit field opens on and compares against: the
// write target. The editor writes the global file, so a scope-marked row's
// baseline is its global value — seeding it with the project's effective value
// would make an untouched-looking save copy the override into the global file.
func editSeedValue(e config.ConfigEntry) string {
	if e.Scope != "" {
		return e.GlobalValue
	}
	return e.Value
}

// staleProjectValueNote is what a project pane's save says when the project
// could not be re-read: the write landed, but project values may be out of date.
const staleProjectValueNote = "could not re-read this project's value — reopen the editor to see it"

// refreshAfterSave brings the rows up to date after key's global value became
// written. A global pane needs only the written value on its row. A project
// pane re-reads every row, since the write may have moved any of them; when
// that read fails or falls back to global rows (which cannot say what the
// project uses), the written value lands on the edited row's global half and
// the returned note says the project values may be stale. scoped reports
// whether the edited row carried a marker, so a marker without a project to
// re-read is never left looking current either.
func (c *ConfigPane) refreshAfterSave(key, written string, scoped bool) string {
	if c.project != nil {
		if entries, honored, err := c.reread(c.project); err == nil && honored != nil {
			c.entries = visibleAppearanceEntries(entries)
			return ""
		}
	}
	for i := range c.entries {
		if c.entries[i].Key != key {
			continue
		}
		if c.entries[i].Scope != "" {
			c.entries[i].GlobalValue = written
		} else {
			c.entries[i].Value = written
		}
	}
	if c.project != nil || scoped {
		return staleProjectValueNote
	}
	return ""
}

func joinNotices(a, b string) string {
	if a == "" {
		return b
	}
	return a + " · " + b
}

func (c *ConfigPane) SetEditValueForTest(v string) { c.input.SetValue(v) }
func (c *ConfigPane) EditValueForTest() string     { return c.input.Value() }
