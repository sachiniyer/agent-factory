package ui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// The Hosts section of the config overlay (#5571 W10.5) — the TUI analogue of
// `af hosts add|list|check|remove` and of the web config view's Hosts section
// (web/src/hosts.ts). It manages the two global-only manifest keys `hosts` and
// `default_host`, whose raw rows also render in the tiers above; what the
// section adds is the per-entry workflow the compact-JSON row cannot express:
// add one entry, remove one, probe one or all with CheckHost, and see each
// outcome on its own row.
//
// It is built on the same writers, never a second implementation:
//
//   - add/remove ride hosts_edit — ONE read-modify-write under the config file
//     lock (config.EditGlobalHosts), reached through the pane's editHosts seam
//     (daemon.EditGlobalHosts locally, the health-gated SetConfigValue call
//     under a remote target — both in config_target.go). An older daemon that
//     would silently write an EMPTY table is refused before the send.
//   - check rides CheckHost — the daemon's own probe over the local control
//     socket or /v1/CheckHost. It is the one piece the pane cannot run inline:
//     an ssh probe is a dial, not a bounded socket call, so the pane RECORDS
//     the request and the app performs it on a tea.Cmd — the same division
//     the Accounts section keeps for its daemon round trips.
//   - make-default rides the pane's ordinary `save` seam on `default_host`,
//     the same scalar write `af config set default_host` performs.
//
// The destination field shares the CLI's grammar: config.ParseHostDestination
// is `af hosts add`'s own `[user@]host[:port]` parser, so a UI can never
// accept a destination the writer would reject. Everything else — the name
// rules, the port range, the exists/default checks — is validated where the
// write happens and a refusal comes back in the writer's own words, shown
// verbatim (the commitEdit doctrine).
const (
	hostsHeading     = "Hosts"
	hostsHeadingNote = "The ssh hosts a sandbox session can run on · you own connectivity — af opens no tunnel and edits no ssh config."
	hostsEmptyNote   = "No hosts saved · select add a host to save one."
)

// HostRow is one line of the Hosts section: a saved host, or one of the two
// action rows (add, check-all) below the list. Exactly one of Entry/Action is
// meaningful on a row.
type HostRow struct {
	// Name is the host's list name — what `af sessions create --host` takes.
	Name string
	// Entry is the saved destination. Zero value on an action row.
	Entry config.HostConfig
	// IsDefault marks the entry default_host names.
	IsDefault bool
	// Action is "+ add a host" or "check all" — a verb row, not a host.
	Action string
}

const (
	hostActionAdd      = "add"
	hostActionCheckAll = "check-all"
)

// hostCheck is one cached probe verdict plus the inputs it measured under —
// the web section's own rule (#6632): a verdict belongs to the destination it
// probed and the host-key posture it ran with, not to the name it sits under.
// An edit that lands after the probe (here, or a hand-edit the next open
// picks up) retires the verdict rather than leaving an old answer beside a
// new destination.
type hostCheck struct {
	entry   config.HostConfig
	posture string
	result  daemon.HostCheckResult
}

// hostPending is the snapshot taken when a check is requested: the entry and
// the effective posture the probe is being asked under. When the result
// lands it becomes the hostCheck — the verdict describes what was probed,
// which may already differ from the row's current entry.
type hostPending struct {
	entry   config.HostConfig
	posture string
}

// hostsSection mirrors accountsSection: the derived rows are re-read from the
// manifest on every rebuild; the section's own state is the verdicts, the
// busy marks, the pending snapshots, the add form, and the recorded check
// request the app takes.
type hostsSection struct {
	checks  map[string]hostCheck
	pending map[string]hostPending
	// busy marks names with a probe in flight — "" for a check-all, which rests
	// every row's verbs for its span (the web's rule: a per-host answer landing
	// first would be overwritten by the slower aggregate).
	busy map[string]bool

	// parseError names a `hosts` value that did not decode — a hand-edit can
	// leave one, and rendering it as "no hosts" would hide the file it holds.
	parseError string

	// checkName + checkAsked are the recorded probe request the app takes
	// ("" = check all). The pane cannot run a probe itself — it is a dial.
	checkName  string
	checkAsked bool

	// status/statusIsError report the last CHECK outcome (a refused request, a
	// dead daemon, the empty-list answer). Add/remove outcomes use the pane's
	// own status line, since they are writes. statusFromCheck distinguishes a
	// check failure from a write echo so a successful retry can retire it.
	status          string
	statusIsError   bool
	statusFromCheck bool

	adding bool
	form   hostAddForm
}

// hostAddField is one line of the add form: a text input, or a checkbox-style
// toggle (input nil, on carries the value).
type hostAddField struct {
	label       string
	placeholder string
	input       textinput.Model
	toggle      bool
	on          bool
}

// hostAddForm is the section's add view — a swap of the list, like the explain
// view: fields navigated top to bottom, enter advancing to the last field's
// save. It deliberately spans the whole HostConfig so no field of the wire's
// HostsEdit.host is unreachable from this surface.
type hostAddForm struct {
	fields []hostAddField
	focus  int
}

// hostField order — indexes are load-bearing in handleHostAddKey.
const (
	hostFieldName = iota
	hostFieldDestination
	hostFieldIdentityFile
	hostFieldWorkspaceRoot
	hostFieldHostKeyPosture
	hostFieldUseSSHConfig
	hostFieldMakeDefault
	hostFieldCount
)

func newHostAddForm() hostAddForm {
	specs := []struct {
		label       string
		placeholder string
		toggle      bool
	}{
		{"name", "what --host picks, e.g. box1", false},
		{"destination", "[user@]host[:port]", false},
		{"identity file", "absolute path · optional", false},
		{"workspace root", "absolute path · optional", false},
		{"host key check", "strict | accept-new | insecure · optional", false},
		{"use ssh_config", "space toggles · your ssh_config decides host-key checking", true},
		{"make default", "space toggles · sessions that do not pick run here", true},
	}
	form := hostAddForm{fields: make([]hostAddField, len(specs))}
	for i, s := range specs {
		form.fields[i] = hostAddField{label: s.label, placeholder: s.placeholder, toggle: s.toggle}
		if !s.toggle {
			in := textinput.New()
			in.Placeholder = s.placeholder
			form.fields[i].input = in
		}
	}
	return form
}

// hostRowsFromEntries derives the section's rows from the `hosts` and
// `default_host` manifest entries the pane already holds — the same read the
// web section makes of its entries (hostRows in web/src/hosts.ts): a compact
// JSON table, sorted, each row beside the default mark. A value that does not
// decode is reported rather than rendered as "no hosts" — a hand-edit can
// leave one, and "no hosts" would hide the file it actually holds.
func (c *ConfigPane) hostRowsFromEntries() ([]HostRow, string) {
	raw := "{}"
	defaultName := ""
	for _, e := range c.entries {
		switch e.Key {
		case "hosts":
			raw = e.Value
		case "default_host":
			defaultName = e.Value
		}
	}
	var table map[string]config.HostConfig
	if err := json.Unmarshal([]byte(raw), &table); err != nil {
		return nil, "hosts could not be read: " + err.Error()
	}
	if table == nil {
		return nil, fmt.Sprintf("hosts is %s, not a table of entries", raw)
	}
	rows := make([]HostRow, 0, len(table))
	for _, name := range config.SortedHostNames(table) {
		rows = append(rows, HostRow{Name: name, Entry: table[name], IsDefault: name == defaultName})
	}
	return rows, ""
}

// appendHostRows renders the section into the pane's flattened row list,
// straight after the tiers: the section derives from the manifest entries
// the pane already holds (its `hosts`/`default_host` rows also render in
// the tiers above), so it needs no load and no daemon answer to exist — a
// pane built from SetEntries alone still shows it. Daemon-reported state
// (the verdicts) lives on the rows, not on the section's presence, and
// sitting ahead of Accounts keeps the register rows last — the invariant
// the register flow's navigate-to-bottom drives rely on.
func (c *ConfigPane) appendHostRows() {
	rows, parseError := c.hostRowsFromEntries()
	c.hosts.parseError = parseError
	c.rows = append(c.rows, configRow{heading: hostsHeading})
	if parseError != "" {
		return
	}
	for i := range rows {
		c.rows = append(c.rows, configRow{host: &rows[i]})
	}
	c.rows = append(c.rows, configRow{host: &HostRow{Action: hostActionAdd}})
	if len(rows) > 0 {
		c.rows = append(c.rows, configRow{host: &HostRow{Action: hostActionCheckAll}})
	}
}

// selectedHost returns the section row under the cursor, or nil.
func (c *ConfigPane) selectedHost() *HostRow {
	if c.selectedIdx < 0 || c.selectedIdx >= len(c.rows) {
		return nil
	}
	return c.rows[c.selectedIdx].host
}

// hostBusy reports whether a probe is running for name — its own, or the
// check-all that would overwrite a per-host answer mid-flight.
func (c *ConfigPane) hostBusy(name string) bool {
	return c.hosts.busy[name] || c.hosts.busy[""]
}

// handleHostKey routes a key while a Hosts row is selected, returning false
// when the row or key is not the section's. enter probes the host, d removes
// it, D makes it the default — all on the selected row only.
func (c *ConfigPane) handleHostKey(msg tea.KeyMsg) bool {
	row := c.selectedHost()
	if row == nil {
		return false
	}
	if row.Action != "" {
		if msg.String() != "enter" {
			return false
		}
		switch row.Action {
		case hostActionAdd:
			c.beginHostAdd()
		case hostActionCheckAll:
			c.requestHostCheck(nil)
		}
		return true
	}
	if c.hostBusy(row.Name) {
		// A probing row rests its VERBS, not the pane: navigation, esc and the
		// rest still fall through to the ordinary switch so a slow ssh never
		// locks the overlay. 'e' still opens the reader — it is a view, not a
		// verb, and it says the probe is in flight.
		switch msg.String() {
		case "enter", "d", "D":
			return true
		case "e":
			c.beginHostExplain(row)
			return true
		}
		return false
	}
	switch msg.String() {
	case "enter":
		c.requestHostCheck([]HostRow{*row})
		return true
	case "e":
		c.beginHostExplain(row)
		return true
	case "d":
		c.removeHost(row)
		return true
	case "D":
		c.makeHostDefault(row)
		return true
	}
	return false
}

// requestHostCheck records a probe request for the app to perform and marks
// the row busy — "" (empty slice? no: the check-all action passes nil) probes
// every listed host. A name already probing rests instead of stacking a second
// request behind the same slow ssh. Every probed row is snapshotted NOW: the
// verdict belongs to the entry and the effective posture it was asked under,
// so a row that changes underneath retires the answer rather than inheriting
// it — the web section's own cache rule (#6632).
func (c *ConfigPane) requestHostCheck(rows []HostRow) {
	name := ""
	if len(rows) == 1 {
		name = rows[0].Name
	} else {
		// A check-all while anything is probing would overwrite a real verdict
		// with the daemon's skipped-probe answer ("already running"), so it rests
		// like a host that is itself busy.
		if len(c.hosts.busy) > 0 {
			return
		}
		rows, _ = c.hostRowsFromEntries()
	}
	if c.hostBusy(name) {
		return
	}
	if c.hosts.busy == nil {
		c.hosts.busy = map[string]bool{}
	}
	if c.hosts.pending == nil {
		c.hosts.pending = map[string]hostPending{}
	}
	for _, row := range rows {
		c.hosts.pending[row.Name] = hostPending{
			entry:   row.Entry,
			posture: c.effectiveHostPosture(row.Entry),
		}
	}
	c.hosts.busy[name] = true
	c.hosts.checkName = name
	c.hosts.checkAsked = true
}

// TakeHostCheckRequest reports the probe the section asked for since the last
// call — the host name, "" for check-all — clearing the flag so one keypress
// fires exactly one probe. The app performs it on a tea.Cmd.
func (c *ConfigPane) TakeHostCheckRequest() (string, bool) {
	name, asked := c.hosts.checkName, c.hosts.checkAsked
	c.hosts.checkName, c.hosts.checkAsked = "", false
	return name, asked
}

// SetHostCheckResults lands a finished probe: the busy mark rests, each
// returned verdict is stored against the snapshot it was asked under, and a
// request-level failure (a refused name, a dead daemon) becomes the section's
// status. A successful retry retires the earlier check failure — the web's
// own rule — while a write refusal is left alone (it is not a check outcome).
func (c *ConfigPane) SetHostCheckResults(requested string, results []daemon.HostCheckResult, err error) {
	delete(c.hosts.busy, requested)
	if err != nil {
		c.setHostStatus(err.Error(), true, true)
		return
	}
	if c.hosts.checks == nil {
		c.hosts.checks = map[string]hostCheck{}
	}
	for _, r := range results {
		pend := c.hosts.pending[r.Name]
		c.hosts.checks[r.Name] = hostCheck{entry: pend.entry, posture: pend.posture, result: r}
		delete(c.hosts.pending, r.Name)
	}
	if len(results) == 0 {
		c.setHostStatus("no hosts saved; add one below", false, true)
		return
	}
	if c.hosts.statusFromCheck && c.hosts.statusIsError {
		c.clearHostStatus()
	}
}

// HostStatusLine reports the section's own channel outcome — a probe the
// daemon refused, the empty-list answer, or a refused write. The shared
// status line is a separate slot every channel's writer feeds; this is the
// section's own bookkeeping.
func (c *ConfigPane) HostStatusLine() (string, bool) {
	return c.hosts.status, c.hosts.statusIsError
}

// setHostStatus records a Hosts-section outcome on its channel and takes the
// status line — the newest event wins whichever section produced it (#5571
// review). fromCheck marks a probe outcome so a successful retry can retire
// it; a write refusal is left alone (it is not a check outcome).
func (c *ConfigPane) setHostStatus(text string, isError, fromCheck bool) {
	c.hosts.status = text
	c.hosts.statusIsError = isError
	c.hosts.statusFromCheck = fromCheck
	c.statusLine = statusEvent{text: text, isError: isError, from: statusFromHosts}
}

// clearHostStatus retires the check outcome — a write's echo is the newer
// event, so the pane's own status line should win from here. The line blanks
// only when the hosts channel owns what it shows.
func (c *ConfigPane) clearHostStatus() {
	c.hosts.status = ""
	c.hosts.statusIsError = false
	c.hosts.statusFromCheck = false
	if c.statusLine.from == statusFromHosts {
		c.statusLine = statusEvent{}
	}
}

// removeHost applies one hosts_edit remove through the pane's save seam —
// synchronously, like commitEdit's own write: the call is a bounded socket or
// HTTP round trip, not a dial. The default's removal carries clear_default
// (the web's "Remove and clear the default" button and the CLI's
// --clear-default flag are the same decision named up front).
func (c *ConfigPane) removeHost(row *HostRow) {
	c.clearHostStatus()
	edit := config.HostsEdit{Op: config.HostsEditRemove, Name: row.Name, ClearDefault: row.IsDefault}
	result, notice, err := c.editHosts(edit)
	if err != nil {
		c.setStatus(err.Error(), true, "")
		return
	}
	// The writer answered with the whole table's canonical value — reflect it
	// so the rebuilt rows show the file as it is, and clear the verdict the
	// removed row can no longer display.
	c.setEntryValue("hosts", result.Value)
	if edit.ClearDefault {
		c.setEntryValue("default_host", "")
	}
	delete(c.hosts.checks, row.Name)
	delete(c.hosts.pending, row.Name)
	outcome := "removed host " + row.Name
	if edit.ClearDefault {
		outcome += " (the default was cleared)"
	}
	c.setStatus(outcome, false, notice)
	c.rebuildRows()
}

// makeHostDefault writes `default_host = <name>` through the pane's ordinary
// save seam — the same scalar write `af config set default_host` performs, so
// its effect notice is the writer's own.
func (c *ConfigPane) makeHostDefault(row *HostRow) {
	if row.IsDefault {
		return
	}
	c.clearHostStatus()
	result, notice, err := c.save("default_host", row.Name)
	if err != nil {
		c.setStatus(err.Error(), true, "")
		return
	}
	c.setEntryValue("default_host", result.Value)
	c.setStatus(fmt.Sprintf("set %s = %s", result.Key, result.Value), false, notice)
	c.rebuildRows()
}

// setEntryValue reflects one manifest key's canonical written value back into
// the loaded entries, so the rows the section derives show the file as it is
// rather than the value that was typed — commitEdit's own contract.
func (c *ConfigPane) setEntryValue(key, value string) {
	for i := range c.entries {
		if c.entries[i].Key == key {
			c.entries[i].Value = value
		}
	}
}

// effectiveHostPosture is the host-key mode a probe of entry runs under —
// hostPosture in session/ssh_command_host.go and effectiveHostKeyPosture in
// web/src/hosts.ts: the entry's own override, else the global
// ssh.host_key_verification as served, else the manifest's strict default. A
// use_ssh_config host answers "" — its probe composes no host-key options at
// all, so the global is not an input its verdict can stale under.
func (c *ConfigPane) effectiveHostPosture(entry config.HostConfig) string {
	if entry.UseSSHConfig {
		return ""
	}
	if entry.HostKeyVerification != "" {
		return entry.HostKeyVerification
	}
	global := c.globalHostPosture()
	if global == "" {
		return string(config.SSHHostKeyStrict)
	}
	return global
}

// globalHostPosture is the served `ssh.host_key_verification` value — "" when
// the manifest does not carry one, which the composer reads as its default.
func (c *ConfigPane) globalHostPosture() string {
	for _, e := range c.entries {
		if e.Key == "ssh.host_key_verification" {
			return e.Value
		}
	}
	return ""
}

// sameHostEntry reports whether two saved entries are the same probing target,
// field by field — the web's sameHostEntry. A verdict recorded against one
// renders only while the row's saved entry still equals it.
func sameHostEntry(a, b config.HostConfig) bool {
	return a.Host == b.Host &&
		a.User == b.User &&
		a.Port == b.Port &&
		a.IdentityFile == b.IdentityFile &&
		a.WorkspaceRoot == b.WorkspaceRoot &&
		a.HostKeyVerification == b.HostKeyVerification &&
		a.UseSSHConfig == b.UseSSHConfig
}

// hostVerdictLines renders the cached verdict for name — only while every
// input it was probed under still holds: the same destination (sameHostEntry)
// and the same effective posture. The lines are daemon.HostCheckLines, the one
// rendering `af hosts check` and the web's verdict share, phrased for this
// surface by HostCheckOrigin and a UI retry.
func (c *ConfigPane) hostVerdictLines(row HostRow) []string {
	check, ok := c.hosts.checks[row.Name]
	if !ok || !sameHostEntry(check.entry, row.Entry) || check.posture != c.effectiveHostPosture(row.Entry) {
		return nil
	}
	return daemon.HostCheckLines(check.result, check.entry, c.globalHostPosture(), HostCheckOrigin(), "check it again")
}

// beginHostExplain opens the selected host's detail as a scrollable reader —
// the explain view's own swap, pointed at a host instead of a key (#5571
// review). A long verdict and its wrapped remedy can exceed the list window
// in a short overlay, and the window pins the oversized row it cannot split:
// the row keeps what fits, and 'e' reads the whole of it here.
func (c *ConfigPane) beginHostExplain(row *HostRow) {
	host := *row // snapshot — rebuilds replace the row list under the open view
	c.savedScrollTop = c.scrollTop
	c.scrollTop = 0
	c.explainKey = host.Name
	c.explainValue = nil
	c.explainHost = &host
	c.explainLines = c.wrappedHostExplainLines(host)
	c.explaining = true
	c.clearStatus()
}

// wrappedHostExplainLines lays out the host explain view: the saved entry and
// the posture a probe would run under, then the full verdict — the same
// daemon.HostCheckLines the row shows until the window runs out. Every line
// passes explainSafe first: verdict text quotes what the probe saw, and a
// free-form path or problem string can carry escapes the compositor would
// emit verbatim.
func (c *ConfigPane) wrappedHostExplainLines(row HostRow) []string {
	var logical []string
	logical = append(logical, "destination: "+config.DescribeHost(row.Entry))
	if row.IsDefault {
		logical = append(logical, "the default host")
	}
	if detail := hostRowDetail(row); detail != "" {
		logical = append(logical, detail)
	}
	if posture := c.effectiveHostPosture(row.Entry); posture == "" {
		logical = append(logical, "host key: decided by your ssh_config")
	} else {
		logical = append(logical, "host key: "+posture)
	}
	verdict := c.hostVerdictLines(row)
	if len(verdict) > 0 {
		logical = append(logical, "")
		logical = append(logical, verdict...)
	}
	switch {
	case c.hostBusy(row.Name):
		logical = append(logical, "", "a probe is running…")
	case len(verdict) == 0:
		logical = append(logical, "", "no verdict yet — enter on the row probes this host")
	}
	var lines []string
	for _, l := range logical {
		wrapped := configValueStyle.Width(c.width - 2).Render(explainSafe(l))
		lines = append(lines, strings.Split(strings.TrimSuffix(wrapped, "\n"), "\n")...)
	}
	return lines
}

// --- the add form ---

// beginHostAdd swaps the list for the add form — the pane's "+ add a host"
// row, the TUI's `af hosts add`. The probe the CLI runs after the save rides
// as a recorded check request on success (commitHostAdd).
func (c *ConfigPane) beginHostAdd() {
	c.hosts.adding = true
	c.hosts.form = newHostAddForm()
	c.sizeHostFields()
	c.hosts.form.fields[0].input.Focus()
	c.clearStatus()
	// The form renders hosts.status in its footer: open clean — a leftover
	// check verdict would read as a form error before anything was submitted.
	c.clearHostStatus()
}

// sizeHostFields bounds each form input to the pane — an unbounded textinput
// renders its whole value, so a long path would scroll the cursor off the
// clipped row. Same job sizeEditField does for the value editor.
func (c *ConfigPane) sizeHostFields() {
	if c.width <= 0 {
		return
	}
	// cursor(2) + label column(21) + a little room for the toggle box's note.
	width := c.width - 26
	if width < 16 {
		width = 16
	}
	for i := range c.hosts.form.fields {
		if !c.hosts.form.fields[i].toggle {
			c.hosts.form.fields[i].input.Width = width
		}
	}
}

func (c *ConfigPane) cancelHostAdd() {
	c.hosts.adding = false
	c.hosts.form = hostAddForm{}
}

// handleHostAddKey drives the form. ↑/↓ and tab/shift+tab move between
// fields, space toggles the checkbox rows, enter advances and saves on the
// last field, esc abandons. Runes on a text field go to that field — so "a",
// "C", "d" and "e" type rather than firing the list's verbs.
func (c *ConfigPane) handleHostAddKey(msg tea.KeyMsg) bool {
	form := &c.hosts.form
	current := &form.fields[form.focus]
	switch msg.String() {
	case "esc":
		c.cancelHostAdd()
		return true
	case "up", "shift+tab":
		form.focus = (form.focus + hostFieldCount - 1) % hostFieldCount
		c.focusHostField()
		return true
	case "down", "tab":
		form.focus = (form.focus + 1) % hostFieldCount
		c.focusHostField()
		return true
	case "enter":
		if form.focus == hostFieldCount-1 {
			c.commitHostAdd()
			return true
		}
		form.focus++
		c.focusHostField()
		return true
	}
	if current.toggle {
		switch msg.String() {
		case " ", "space":
			current.on = !current.on
		case "y":
			current.on = true
		case "n":
			current.on = false
		}
		return true
	}
	var cmd tea.Cmd
	current.input, cmd = current.input.Update(msg)
	_ = cmd
	return true
}

// focusHostField moves input focus to the selected field and blurs the rest —
// only one text field takes runes at a time.
func (c *ConfigPane) focusHostField() {
	for i := range c.hosts.form.fields {
		field := &c.hosts.form.fields[i]
		if field.toggle {
			continue
		}
		if i == c.hosts.form.focus {
			field.input.Focus()
		} else {
			field.input.Blur()
		}
	}
}

// commitHostAdd sends ONE hosts_edit add through the pane's editHosts seam,
// then — in `af hosts add`'s own order — the optional `default_host` write and
// the tail probe, which is recorded as a check request for the app to run.
//
// The only check done here is the parse that splits the destination into the
// wire's fields: config.ParseHostDestination is `af hosts add`'s own grammar.
// Everything else — the name rules, the port range, the exists check — is
// validated where the write happens; a refusal comes back in the writer's
// words, shown verbatim, and the form stays open for the correction.
func (c *ConfigPane) commitHostAdd() {
	form := &c.hosts.form
	text := func(i int) string { return strings.TrimSpace(form.fields[i].input.Value()) }
	name := text(hostFieldName)
	parsed, err := config.ParseHostDestination(text(hostFieldDestination))
	if err != nil {
		c.setHostStatus(err.Error(), true, false)
		return
	}
	// Every HostConfig field is assigned by NAME here, not carried inside the
	// parsed value: the form covers the whole wire type, and the parity
	// audit's field walk proves it only from field-by-field assignment.
	var entry config.HostConfig
	entry.Host, entry.User, entry.Port = parsed.Host, parsed.User, parsed.Port
	entry.IdentityFile = text(hostFieldIdentityFile)
	entry.WorkspaceRoot = text(hostFieldWorkspaceRoot)
	entry.HostKeyVerification = text(hostFieldHostKeyPosture)
	entry.UseSSHConfig = form.fields[hostFieldUseSSHConfig].on
	edit := config.HostsEdit{Op: config.HostsEditAdd, Name: name, Host: &entry}
	c.clearHostStatus()
	result, notice, err := c.editHosts(edit)
	if err != nil {
		c.setHostStatus(err.Error(), true, false)
		return
	}

	wantDefault := form.fields[hostFieldMakeDefault].on
	c.cancelHostAdd()
	c.setEntryValue("hosts", result.Value)
	desc := config.DescribeHost(entry)

	// The optional default write is `af hosts add --default`'s own second step
	// and its own outcome: the host is saved either way, so a refused default
	// is reported against the save, never as a lost write.
	madeDefault := false
	defaultErr := ""
	if wantDefault {
		dres, _, derr := c.save("default_host", name)
		if derr != nil {
			defaultErr = fmt.Sprintf("saved host %q (%s), but could not make it the default: %s", name, desc, derr)
		} else {
			madeDefault = true
			c.setEntryValue("default_host", dres.Value)
		}
	}
	if defaultErr != "" {
		c.setStatus(defaultErr, true, "")
	} else {
		c.setStatus(fmt.Sprintf("saved host %s (%s)%s", name, desc, map[bool]string{true: " as the default", false: ""}[madeDefault]), false, notice)
	}
	c.rebuildRows()

	// `af hosts add` probes after the save: the entry is kept whatever the
	// check says and its verdict lands on the new row. The snapshot comes from
	// the SERVED rows, not the typed form — the verdict belongs to the entry
	// the file now holds, which is what hostVerdictLines compares against.
	if rows, _ := c.hostRowsFromEntries(); len(rows) > 0 {
		for _, row := range rows {
			if row.Name == name {
				c.requestHostCheck([]HostRow{row})
				break
			}
		}
	}
}

// renderHostRow draws one host line: cursor, name, the `user@host:port`
// summary, the default mark, a probe-in-flight note, and — when selected —
// the verbs and the freshest verdict.
func (c *ConfigPane) renderHostRow(i int, row HostRow) string {
	var b strings.Builder
	selected := i == c.selectedIdx

	cursor := "  "
	if selected {
		cursor = SelectionMarker("› ")
	}
	b.WriteString(cursor)

	style := configKeyStyle
	if selected {
		style = configSelectedStyle
	}
	if row.Action != "" {
		label := "+ add a host"
		if row.Action == hostActionCheckAll {
			label = "↻ check all hosts"
			if c.hostBusy("") {
				label += " · probing…"
			}
		}
		b.WriteString(style.Render(label))
		return c.fitPaneLine(b.String()) + "\n"
	}

	b.WriteString(style.Render(row.Name))
	valueStyle := configValueStyle
	if selected {
		valueStyle = configSelectedStyle
	}
	b.WriteString(valueStyle.Render("  " + config.DescribeHost(row.Entry)))
	if row.IsDefault {
		b.WriteString(valueStyle.Render("  default"))
	}
	if c.hostBusy(row.Name) {
		b.WriteString(valueStyle.Render(" · probing…"))
	}
	line := c.fitPaneLine(b.String()) + "\n"
	if !selected {
		return line
	}

	var out strings.Builder
	out.WriteString(line)
	out.WriteString(c.wrapIndented(hostRowPurpose(row), configPurposeStyle))
	if detail := hostRowDetail(row); detail != "" {
		out.WriteString(c.wrapIndented(detail, configHintStyle))
	}
	for _, verdict := range c.hostVerdictLines(row) {
		out.WriteString(c.wrapIndented(verdict, configHintStyle))
	}
	return out.String()
}

// hostRowPurpose is the verbs line under the selected host — what enter, e, d
// and D do to THIS row. Removing the default is named up front, the same
// decision the web's "Remove and clear the default" button and the CLI's
// --clear-default flag carry.
func hostRowPurpose(row HostRow) string {
	remove := "d remove"
	if row.IsDefault {
		remove = "d remove (clears the default)"
	}
	makeDefault := ""
	if !row.IsDefault {
		makeDefault = " · D make default"
	}
	return "enter check · e detail · " + remove + makeDefault
}

// hostRowDetail names the entry's optional fields when set — the parts
// `user@host:port` cannot say, in the same place a config row's purpose goes.
func hostRowDetail(row HostRow) string {
	var parts []string
	if row.Entry.IdentityFile != "" {
		parts = append(parts, "identity file "+row.Entry.IdentityFile)
	}
	if row.Entry.WorkspaceRoot != "" {
		parts = append(parts, "workspace "+row.Entry.WorkspaceRoot)
	}
	if row.Entry.HostKeyVerification != "" {
		parts = append(parts, "host key "+row.Entry.HostKeyVerification)
	}
	if row.Entry.UseSSHConfig {
		parts = append(parts, "uses your ssh_config")
	}
	return strings.Join(parts, " · ")
}

// renderHostAddView lays out the add form through the same header/window/
// footer assembly as the list and the explain view. The outcome — nearly
// always a refused write or destination — rides the FOOTER, not the field
// list: the window follows the focused field and field navigation cannot
// pass the last field, so an error appended after it would sit below the
// visible slice in a short overlay with no way to scroll to it.
func (c *ConfigPane) renderHostAddView() string {
	header := c.fitPaneLine(configTitleStyle.Render("Add a host")) + "\n" +
		c.wrap("saved through the daemon's own host-table edit · fields validate where the write happens", configHintStyle)
	var footer strings.Builder
	if c.hosts.status != "" {
		style := configOKStyle
		if c.hosts.statusIsError {
			style = configErrorStyle
		}
		footer.WriteString(c.wrap(c.hosts.status, style))
	}
	footer.WriteString("\n" + configHintStyle.Render(c.fitHints([]configHint{
		{text: "↑/↓ field", drop: 2},
		{text: "space toggle", drop: 3},
		{text: "↵ next · save on last", drop: 1},
		{text: "esc cancel"},
	})) + "\n")

	var lines []string
	form := &c.hosts.form
	for i, field := range form.fields {
		focused := i == form.focus
		cursor := "  "
		if focused {
			cursor = SelectionMarker("› ")
		}
		labelStyle := configKeyStyle
		if focused {
			labelStyle = configSelectedStyle
		}
		var control string
		if field.toggle {
			box := "[ ]"
			if field.on {
				box = "[x]"
			}
			control = box
			if field.placeholder != "" && focused {
				control += "  " + field.placeholder
			}
		} else {
			control = renderConfigInput(&c.hosts.form.fields[i].input)
		}
		line := cursor + labelStyle.Render(fmt.Sprintf("%-21s", field.label)) + control
		lines = append(lines, c.fitPaneLine(line))
	}
	return c.renderWindowed(header, footer.String(), lines, form.focus, form.focus+1)
}
