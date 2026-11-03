package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// The Hosts section (#5571 W10.5) is the TUI's `af hosts add|list|check|remove`.
// These tests drive the pane's own seams — editHosts for the table writes,
// save for the default pick — the same way the config-row tests drive save:
// the real writers live in config_target.go and are exercised by the app, but
// what this file proves is that the SECTION asks for the right write, shows
// the writer's outcome, and records exactly one probe per keypress.

func newHostTestPane(t *testing.T, hosts map[string]config.HostConfig, def string) *ConfigPane {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Hosts = hosts
	cfg.DefaultHost = def
	c := NewConfigPane()
	c.SetSize(100, 200)
	c.SetEntries(config.ManifestWithValues(cfg), "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()
	return c
}

func selectHostRow(t *testing.T, c *ConfigPane, name string) {
	t.Helper()
	for i, row := range c.rows {
		if row.host != nil && row.host.Name == name {
			c.selectedIdx = i
			return
		}
	}
	t.Fatalf("host %q is not a row in the config editor", name)
}

func selectHostAction(t *testing.T, c *ConfigPane, action string) {
	t.Helper()
	for i, row := range c.rows {
		if row.host != nil && row.host.Action == action {
			c.selectedIdx = i
			return
		}
	}
	t.Fatalf("the %q action is not a row in the config editor", action)
}

func twoHosts() map[string]config.HostConfig {
	return map[string]config.HostConfig{
		"box1": {Host: "box1.tail.example", User: "dev", Port: 2222},
		"box2": {Host: "box2.tail.example"},
	}
}

// The section lists what the manifest carries: one row per saved host in name
// order, the default named on its row, and the two action rows below — the
// same list `af hosts list` prints and the web section renders.
func TestConfigPaneHostsRows(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "box1")
	view := c.String()

	for _, want := range []string{
		hostsHeading,
		"box1", "box2",
		"dev@box1.tail.example:2222", // config.DescribeHost — the shared summary
		"default",
		"+ add a host",
		"↻ check all hosts",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the Hosts section does not render %q\n%s", want, view)
		}
	}
	// Sorted: box1 before box2, as the CLI and web order them.
	if strings.Index(view, "box1") > strings.Index(view, "box2") {
		t.Error("hosts must render in name order")
	}
}

// An empty table still shows the section — the add verb is how the first host
// gets in — but offers no check-all.
func TestConfigPaneHostsEmpty(t *testing.T) {
	c := newHostTestPane(t, nil, "")
	view := c.String()
	if !strings.Contains(view, "No hosts saved") {
		t.Error("an empty host table should say so rather than render nothing")
	}
	if !strings.Contains(view, "+ add a host") {
		t.Error("the add verb must exist on the empty table — it is how the first host gets in")
	}
	if strings.Contains(view, "↻ check all hosts") {
		t.Error("check-all on an empty table would probe nothing; it must not render")
	}
}

// A host table that will not decode — a hand-edit can leave one — is named as
// unparseable rather than hidden behind "No hosts saved", which would lie
// about the file's contents.
func TestConfigPaneHostsUnparseable(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	for i := range c.entries {
		if c.entries[i].Key == "hosts" {
			c.entries[i].Value = "not json"
		}
	}
	c.rebuildRows()
	view := c.String()
	if !strings.Contains(view, "could not be read") {
		t.Error("a hosts value that does not decode must say so, not render as empty")
	}
	if strings.Contains(view, "+ add a host") {
		t.Error("mutating verbs on an unreadable table would write over a file we could not parse")
	}
}

// enter on a host row records exactly one probe request, marks the row busy,
// and the landed verdict renders under it. The app performs the recorded
// request; the pane only stores it.
func TestConfigPaneHostCheckRequest(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	selectHostRow(t, c, "box1")
	pressKey(c, "enter")

	name, asked := c.TakeHostCheckRequest()
	if !asked || name != "box1" {
		t.Fatalf("enter on a host row must record a probe for that host, got (%q, %v)", name, asked)
	}
	if _, askedAgain := c.TakeHostCheckRequest(); askedAgain {
		t.Error("one keypress is one probe — the request must not replay")
	}
	if !c.hostBusy("box1") {
		t.Error("the row must show the probe in flight")
	}
	if !strings.Contains(c.String(), "probing…") {
		t.Error("a probing row should show it, the way the web dims its check button")
	}
	// A second enter while probing rests rather than stacking behind the ssh.
	pressKey(c, "enter")
	if _, askedAgain := c.TakeHostCheckRequest(); askedAgain {
		t.Error("enter on an already-probing host must not stack a second request")
	}

	c.SetHostCheckResults("box1", []daemon.HostCheckResult{{
		Name: "box1", Probed: true, Reachable: true, Complete: true, Readiness: "ready",
	}}, nil)
	if c.hostBusy("box1") {
		t.Error("the landed answer must rest the busy mark")
	}
	if !strings.Contains(c.String(), "ready (git, tmux and gh answered)") {
		t.Error("the verdict must render under the row — the same arms `af hosts check` prints")
	}
}

// check-all records the empty-name request and rests every row's verbs for its
// span — a per-host answer landing first would be overwritten by the slower
// aggregate anyway.
func TestConfigPaneHostCheckAll(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	selectHostAction(t, c, hostActionCheckAll)
	pressKey(c, "enter")

	name, asked := c.TakeHostCheckRequest()
	if !asked || name != "" {
		t.Fatalf("check-all must record the empty-name probe, got (%q, %v)", name, asked)
	}
	if !c.hostBusy("box1") || !c.hostBusy("box2") {
		t.Error("check-all probes every row, so every row's verbs rest")
	}
	// Verbs rest during the aggregate probe.
	selectHostRow(t, c, "box1")
	pressKey(c, "d")
	if c.hostBusy("") {
		// still busy; nothing should have happened
	}

	c.SetHostCheckResults("", []daemon.HostCheckResult{
		{Name: "box1", Probed: true, Reachable: true, Complete: true, Readiness: "ready"},
		{Name: "box2", Probed: true, Reachable: false, Error: "connection refused"},
	}, nil)
	if c.hostBusy("") {
		t.Error("the landed aggregate must rest the busy mark")
	}
	selectHostRow(t, c, "box2")
	if !strings.Contains(c.String(), "could not be probed") {
		t.Error("each host's own verdict lands on its own row")
	}
}

// A request-level failure — a refused name, a dead daemon — becomes the
// section's status in the writer's words, and the busy mark rests.
func TestConfigPaneHostCheckError(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	selectHostRow(t, c, "box1")
	pressKey(c, "enter")
	c.TakeHostCheckRequest()

	c.SetHostCheckResults("box1", nil, errors.New("the daemon is not running"))
	if c.hostBusy("box1") {
		t.Error("a failed request must still rest the busy mark")
	}
	status, isErr := c.HostStatusLine()
	if !isErr || !strings.Contains(status, "the daemon is not running") {
		t.Errorf("the refusal must surface verbatim, got %q", status)
	}
	if !strings.Contains(c.String(), "the daemon is not running") {
		t.Error("the section's status wins the pane's status line while set")
	}
}

// 'd' removes through the editHosts seam — one hosts_edit remove — and the
// saved row leaves the list. Removing the default names it up front
// (clear_default, the same decision the web's button and the CLI's flag are).
func TestConfigPaneHostRemove(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "box1")
	var got config.HostsEdit
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		got = edit
		return &config.SetResult{Key: "hosts", Value: `{"box2":{"host":"box2.tail.example"}}`}, "live now", nil
	}

	selectHostRow(t, c, "box1")
	pressKey(c, "d")
	if got.Op != config.HostsEditRemove || got.Name != "box1" || !got.ClearDefault {
		t.Fatalf("removing the default must send a remove with clear_default, got %+v", got)
	}
	view := c.String()
	if strings.Contains(view, "dev@box1.tail.example:2222") {
		t.Error("the removed host must leave the list")
	}
	if !strings.Contains(view, "removed host box1") {
		t.Error("the removal echoes in the pane's words")
	}
}

func TestConfigPaneHostRemoveKeepsDefaultQuiet(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "box1")
	var got config.HostsEdit
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		got = edit
		return &config.SetResult{Key: "hosts", Value: `{"box1":{"host":"box1.tail.example","user":"dev","port":2222}}`}, "", nil
	}
	selectHostRow(t, c, "box2")
	pressKey(c, "d")
	if got.ClearDefault {
		t.Error("removing a non-default host must not claim it clears the default")
	}
}

// A refused remove shows the writer's error and changes nothing.
func TestConfigPaneHostRemoveRefused(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "box1")
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		return nil, "", errors.New("the daemon is not running")
	}
	selectHostRow(t, c, "box1")
	pressKey(c, "d")
	if !strings.Contains(c.String(), "the daemon is not running") {
		t.Error("a refused write shows the writer's words")
	}
	if !strings.Contains(c.String(), "dev@box1.tail.example:2222") {
		t.Error("a refused write changes nothing")
	}
}

// 'D' writes default_host through the pane's ordinary save seam — the same
// scalar write `af config set default_host` performs.
func TestConfigPaneHostMakeDefault(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "box1")
	var gotKey, gotValue string
	c.save = func(k, v string) (*config.SetResult, string, error) {
		gotKey, gotValue = k, v
		return &config.SetResult{Key: k, Value: v}, "live now", nil
	}
	selectHostRow(t, c, "box2")
	pressKey(c, "D")
	if gotKey != "default_host" || gotValue != "box2" {
		t.Fatalf("D must save default_host=<name>, got %s=%s", gotKey, gotValue)
	}
	selectHostRow(t, c, "box2")
	if !c.rows[c.selectedIdx].host.IsDefault {
		t.Error("the default mark must move to the saved row")
	}
}

// The add form sends ONE hosts_edit add through the seam — the destination
// field through the CLI's own grammar — then records the tail probe `af hosts
// add` runs, so the new row starts with a verdict in flight.
func TestConfigPaneHostAdd(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	var got config.HostsEdit
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		got = edit
		return &config.SetResult{Key: "hosts", Value: `{"box1":{"host":"box1.tail.example"},"box2":{"host":"box2.tail.example"},"box3":{"host":"box3.example","user":"me","port":2222}}`}, "live now", nil
	}

	selectHostAction(t, c, hostActionAdd)
	pressKey(c, "enter")
	if !c.IsEditing() {
		t.Fatal("the add form takes runes — q in a name must be a character, not an exit")
	}
	// Type the name, advance, type the destination.
	typeInto(c, "box3")
	pressKey(c, "enter")
	typeInto(c, "me@box3.example:2222")
	// Focus the last field and commit.
	for c.hosts.form.focus < hostFieldCount-1 {
		pressKey(c, "enter")
	}
	pressKey(c, "enter")

	if c.hosts.adding {
		t.Fatal("a committed form leaves the add view")
	}
	if got.Op != config.HostsEditAdd || got.Name != "box3" {
		t.Fatalf("the form must send an add for the typed name, got %+v", got)
	}
	if got.Host == nil || got.Host.Host != "box3.example" || got.Host.User != "me" || got.Host.Port != 2222 {
		t.Fatalf("the destination must parse through the CLI's grammar, got %+v", got.Host)
	}
	if !strings.Contains(c.String(), "saved host box3") {
		t.Error("the save echoes in the writer's words")
	}
	name, asked := c.TakeHostCheckRequest()
	if !asked || name != "box3" {
		t.Fatalf("af hosts add probes after the save — the pane must too, got (%q, %v)", name, asked)
	}
}

// --default: the second write is the default_host scalar, reported against the
// save rather than as a lost write when it is refused.
func TestConfigPaneHostAddDefault(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		return &config.SetResult{Key: "hosts", Value: "{}"}, "", nil
	}
	var savedKey, savedValue string
	c.save = func(k, v string) (*config.SetResult, string, error) {
		savedKey, savedValue = k, v
		return &config.SetResult{Key: k, Value: v}, "", nil
	}

	c.beginHostAdd()
	c.hosts.form.fields[hostFieldName].input.SetValue("box3")
	c.hosts.form.fields[hostFieldDestination].input.SetValue("box3.example")
	c.hosts.form.fields[hostFieldMakeDefault].on = true
	c.hosts.form.focus = hostFieldCount - 1
	pressKey(c, "enter")

	if savedKey != "default_host" || savedValue != "box3" {
		t.Fatalf("make-default must save default_host=<name>, got %s=%s", savedKey, savedValue)
	}
	if !strings.Contains(c.String(), "as the default") {
		t.Error("the echo names the default pick, as `af hosts add --default` reports it")
	}
}

func TestConfigPaneHostAddDefaultRefused(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		return &config.SetResult{Key: "hosts", Value: "{}"}, "", nil
	}
	c.save = func(k, v string) (*config.SetResult, string, error) {
		return nil, "", errors.New("refused")
	}
	c.beginHostAdd()
	c.hosts.form.fields[hostFieldName].input.SetValue("box3")
	c.hosts.form.fields[hostFieldDestination].input.SetValue("box3.example")
	c.hosts.form.fields[hostFieldMakeDefault].on = true
	c.hosts.form.focus = hostFieldCount - 1
	pressKey(c, "enter")

	if !strings.Contains(c.String(), "could not make it the default") {
		t.Error("a refused default is reported against the save, never as a lost host")
	}
}

// A destination the CLI's grammar rejects never reaches the writer — the form
// stays open with the parse refusal, exactly as `af hosts add` would print it.
func TestConfigPaneHostAddBadDestination(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	called := false
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		called = true
		return nil, "", nil
	}
	c.beginHostAdd()
	c.hosts.form.fields[hostFieldName].input.SetValue("box3")
	c.hosts.form.fields[hostFieldDestination].input.SetValue("box3.example:notaport")
	c.hosts.form.focus = hostFieldCount - 1
	pressKey(c, "enter")

	if called {
		t.Error("a destination the grammar rejects must never reach the writer")
	}
	if !c.hosts.adding {
		t.Error("the form stays open for the correction")
	}
	status, isErr := c.HostStatusLine()
	if !isErr || status == "" {
		t.Error("the parse refusal shows in the form's own words")
	}
}

// A verdict belongs to the entry it probed: change the saved destination and
// the stale answer stops rendering — the web's own cache rule (#6632).
func TestConfigPaneHostVerdictStalesWithEntry(t *testing.T) {
	hosts := twoHosts()
	c := newHostTestPane(t, hosts, "")
	selectHostRow(t, c, "box1")
	pressKey(c, "enter")
	c.TakeHostCheckRequest()
	c.SetHostCheckResults("box1", []daemon.HostCheckResult{{
		Name: "box1", Probed: true, Reachable: true, Complete: true, Readiness: "ready",
	}}, nil)
	if !strings.Contains(c.String(), "ready (git, tmux and gh answered)") {
		t.Fatal("the fresh verdict renders")
	}
	// The saved entry moves underneath the verdict — a hand-edit, or a remove
	// and re-add under the same name.
	changed := twoHosts()
	e := changed["box1"]
	e.Port = 2200
	changed["box1"] = e
	cfg := config.DefaultConfig()
	cfg.Hosts = changed
	c.SetEntries(config.ManifestWithValues(cfg), "/tmp/config.toml")
	selectHostRow(t, c, "box1")
	if strings.Contains(c.String(), "ready (git, tmux and gh answered)") {
		t.Error("a verdict measured on another destination must stop rendering")
	}
}

// Busy rests the row's verbs — the web disables its buttons during a probe; a
// remove mid-flight would leave a verdict for a row that vanished.
func TestConfigPaneHostBusyRestsVerbs(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "box1")
	editCalled := false
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		editCalled = true
		return nil, "", nil
	}
	saveCalled := false
	c.save = func(k, v string) (*config.SetResult, string, error) {
		saveCalled = true
		return nil, "", nil
	}

	selectHostRow(t, c, "box1")
	pressKey(c, "enter")
	c.TakeHostCheckRequest()
	// Busy now: d, D and enter are all no-ops.
	pressKey(c, "d")
	pressKey(c, "D")
	pressKey(c, "enter")
	if editCalled || saveCalled {
		t.Error("a probing row's verbs rest — no remove or default write mid-flight")
	}
	if _, asked := c.TakeHostCheckRequest(); asked {
		t.Error("no second probe stacks behind the first")
	}
}

// Busy rests the row's verbs, not the pane: navigation, esc and the rest still
// work while a probe runs — a slow ssh must never lock the overlay (#6718
// review).
func TestConfigPaneHostBusyStillNavigates(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	selectHostRow(t, c, "box1")
	pressKey(c, "enter")
	c.TakeHostCheckRequest()

	pressKey(c, "down")
	if row := c.rows[c.selectedIdx].host; row == nil || row.Name != "box2" {
		t.Error("a busy row must not swallow navigation — the overlay is not locked by a probe")
	}
	selectHostRow(t, c, "box1")
	pressKey(c, "esc")
	if c.HasFocus() {
		t.Error("esc must still close the overlay while a probe runs")
	}
}

// The `hosts` and `default_host` manifest rows stay editable keys, and the
// section derives FROM them: hand-editing one must re-derive the section's
// rows rather than leaving the old destinations and marks on screen.
func TestConfigPaneHostsFollowHandEdits(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "box1")
	c.save = func(k, v string) (*config.SetResult, string, error) {
		return &config.SetResult{Key: k, Value: v}, "", nil
	}
	selectKey(t, c, "default_host")
	c.beginEdit()
	c.input.SetValue("box2")
	// The value editor switches on msg.Type, not String: a real KeyEnter.
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	selectHostRow(t, c, "box2")
	if !c.rows[c.selectedIdx].host.IsDefault {
		t.Error("editing default_host must move the section's default mark")
	}
	selectHostRow(t, c, "box1")
	if c.rows[c.selectedIdx].host.IsDefault {
		t.Error("the old default must lose its mark on the same edit")
	}
}

// The section sits ahead of Accounts: the host list is config content — the
// tiers' own `hosts`/`default_host` keys — and the register rows stay the
// list's last selectable rows, the invariant the register flow's
// navigate-to-bottom drives rely on.
func TestConfigPaneHostsPrecedeAccounts(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	c.SetAccounts([]AccountRow{{Agent: "codex", Name: "existing"}},
		[]string{"codex", "gemini"}, nil)

	hostsIdx, accountsIdx := -1, -1
	lastSelectable := -1
	for i, row := range c.rows {
		switch row.heading {
		case hostsHeading:
			hostsIdx = i
		case accountsHeading:
			accountsIdx = i
		}
		if row.isSelectable() {
			lastSelectable = i
		}
	}
	if hostsIdx < 0 || accountsIdx < 0 || hostsIdx > accountsIdx {
		t.Fatalf("the Hosts section must sit ahead of Accounts, got hosts=%d accounts=%d", hostsIdx, accountsIdx)
	}
	if account := c.rows[lastSelectable].account; account == nil || !account.Register {
		t.Fatal("the last selectable row must be a register row — the register flow navigates to the bottom")
	}
}

// A rebuild keeps the selected host under the cursor: the section's rows are
// re-derived on every rebuild, so selection must follow the row's identity —
// the name, or the action — not its old index. Otherwise an 'a' fold, an
// async SetAccounts/SetQuota landing under a remote target, or a removed
// sibling leaves the cursor on a DIFFERENT host and 'd' removes it (#6718
// review — the same preservation accounts get).
func TestConfigPaneHostSelectionSurvivesRebuild(t *testing.T) {
	hosts := twoHosts()
	hosts["box3"] = config.HostConfig{Host: "box3.example"}
	c := newHostTestPane(t, hosts, "")
	selectHostRow(t, c, "box3")

	// The 'a' fold removes the advanced rows ABOVE the section: box3's index
	// moves up by the folded count while its identity stays.
	pressKey(c, "a")
	if host := c.selectedHost(); host == nil || host.Name != "box3" {
		t.Fatalf("the cursor must follow the selected host across a rebuild, got %+v", host)
	}
	pressKey(c, "a")
	if host := c.selectedHost(); host == nil || host.Name != "box3" {
		t.Fatalf("…and back again, got %+v", host)
	}

	// A sibling leaving the table above the selection — a hand-edit, or a
	// remote write landing — must not slide the cursor onto another row.
	for i := range c.entries {
		if c.entries[i].Key == "hosts" {
			c.entries[i].Value = `{"box2":{"host":"box2.tail.example"},"box3":{"host":"box3.example"}}`
		}
	}
	c.rebuildRows()
	if host := c.selectedHost(); host == nil || host.Name != "box3" {
		t.Fatalf("a removed sibling must not move the selection to another row, got %+v", host)
	}

	// The action rows keep their identity too — "+ add a host" is one row
	// wherever the host count puts it.
	selectHostAction(t, c, hostActionAdd)
	pressKey(c, "a")
	if host := c.selectedHost(); host == nil || host.Action != hostActionAdd {
		t.Fatalf("the add row must keep the cursor across a rebuild, got %+v", host)
	}
}

// A refused submit must be VISIBLE in the windowed render, not just in the
// model: the form's fields outgrow a short overlay and field navigation
// cannot pass the last field, so the error rides the footer where the window
// cannot clip it (#6718 review).
func TestConfigPaneHostAddErrorStaysVisible(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	c.SetSize(64, 12)
	c.beginHostAdd()
	c.hosts.form.fields[hostFieldName].input.SetValue("box3")
	c.hosts.form.fields[hostFieldDestination].input.SetValue("box3:bad")
	c.hosts.form.focus = hostFieldCount - 1
	pressKey(c, "enter")

	if !c.hosts.adding {
		t.Fatal("a refused submit keeps the form open")
	}
	if view := c.String(); !strings.Contains(view, "must be a number") {
		t.Fatalf("the parse refusal must render inside the short overlay, got:\n%s", view)
	}
}

// A stale host-check status must never hide the outcome of a newer action:
// the next outcome — a refused edit, a save echo with its notice, an account
// result — retires it. One line reports the LATEST event (#6718 review).
func TestConfigPaneHostStatusRetiresForNewerOutcome(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	selectHostRow(t, c, "box1")
	pressKey(c, "enter")
	c.TakeHostCheckRequest()
	c.SetHostCheckResults("box1", nil, errors.New("the daemon is not running"))
	if view := c.String(); !strings.Contains(view, "the daemon is not running") {
		t.Fatal("a request-level check failure lands in the status line")
	}

	// A refused edit: the validator's words must show, not the check's.
	c.save = func(k, v string) (*config.SetResult, string, error) {
		return nil, "", errors.New("default_program must be one of: claude, codex")
	}
	selectKey(t, c, "default_program")
	c.beginEdit()
	c.input.SetValue("not-an-agent")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	view := c.String()
	if strings.Contains(view, "the daemon is not running") {
		t.Error("a stale check error must not hide the validation failure")
	}
	if !strings.Contains(view, "must be one of: claude, codex") {
		t.Error("the refused edit keeps the validator's message visible")
	}
	if !c.editing {
		t.Error("the rejected value keeps the field open for correction")
	}

	// A successful save: the confirmation AND its restart notice.
	c.save = func(k, v string) (*config.SetResult, string, error) {
		return &config.SetResult{Key: k, Value: v}, "apply on next daemon start", nil
	}
	c.input.SetValue("codex")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	view = c.String()
	if !strings.Contains(view, "set default_program = codex") {
		t.Error("the save echoes in the writer's words")
	}
	if !strings.Contains(view, "apply on next daemon start") {
		t.Error("the restart notice survives next to the echo")
	}
	if strings.Contains(view, "the daemon is not running") {
		t.Error("nothing stale may outlive the save")
	}

	// An account operation's outcome retires a later check error the same way.
	c.SetHostCheckResults("box1", nil, errors.New("check refused again"))
	c.SetAccountStatus("registration refused by the daemon", true)
	view = c.String()
	if strings.Contains(view, "check refused again") {
		t.Error("a stale check error must not hide the account outcome")
	}
	if !strings.Contains(view, "registration refused by the daemon") {
		t.Error("the account outcome shows")
	}
}

// The status line reports the LATEST event whichever section produced it —
// one rule, not a precedence between channels (#5571 review: the account
// channel kept precedence over a newer host WRITE's outcome the same way the
// check channel once hid the config write's). The ordering must run every
// way: accounts → host write → host check → accounts again.
func TestConfigPaneLatestOutcomeWinsAcrossSections(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	c.SetAccountStatus("registration refused by the daemon", true)
	if view := c.String(); !strings.Contains(view, "registration refused by the daemon") {
		t.Fatal("the account outcome lands on the line")
	}

	// A host write's outcome is NEWER — it must displace the account status
	// even though that status is still populated.
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		return &config.SetResult{Key: "hosts", Value: `{"box2":{"host":"box2.tail.example"}}`}, "", nil
	}
	selectHostRow(t, c, "box1")
	pressKey(c, "d")
	view := c.String()
	if !strings.Contains(view, "removed host box1") {
		t.Error("the host write's outcome must take the line")
	}
	if strings.Contains(view, "registration refused") {
		t.Error("a stale account outcome must not hide the newer host write")
	}

	// A still-later check outcome on the hosts channel displaces the write.
	selectHostRow(t, c, "box2")
	pressKey(c, "enter")
	c.TakeHostCheckRequest()
	c.SetHostCheckResults("box2", nil, errors.New("check refused again"))
	if view := c.String(); !strings.Contains(view, "check refused again") {
		t.Error("the check outcome takes the line over the write echo")
	}

	// And the account channel takes it back — the rule runs every direction.
	c.SetAccountStatus("login finished", false)
	view = c.String()
	if !strings.Contains(view, "login finished") {
		t.Error("the newer account outcome takes the line back")
	}
	if strings.Contains(view, "check refused again") {
		t.Error("a stale check error must not hide it")
	}
}

// A long verdict and its wrapped remedy exceed a short overlay's list window,
// and the window pins the oversized row it cannot split. 'e' opens the
// scrollable reader so the whole of it is reachable (#5571 review).
func TestConfigPaneHostExplainReadsWholeVerdict(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	c.SetSize(64, 12)
	selectHostRow(t, c, "box1")
	pressKey(c, "enter")
	c.TakeHostCheckRequest()
	c.SetHostCheckResults("box1", []daemon.HostCheckResult{{
		Name:      "box1",
		Probed:    true,
		Reachable: true,
		Complete:  true,
		Problems: []string{
			"git is not on PATH or its version could not be read — install git so af can create and refresh worktrees",
			"tmux answered `tmux -V` with an old version — sessions need a newer one for the control mode af drives",
		},
		Warnings: []string{
			"gh answered but is not authenticated — run gh auth login on the host before sessions that open pull requests",
		},
	}}, nil)

	// The row's render cannot hold the whole verdict in this window. ("requests"
	// is the verdict tail's last word — asserted as a single token so a wrap
	// cannot split the check.)
	list := c.String()
	if strings.Contains(list, "requests") {
		t.Fatal("the scenario does not exercise the bug — the tail already shows")
	}

	// 'e' opens the reader; scrolling reaches the verdict's last line.
	pressKey(c, "e")
	if !c.explaining {
		t.Fatal("'e' on a host row opens the verdict reader")
	}
	for i := 0; i < 20; i++ {
		pressKey(c, "down")
	}
	view := c.String()
	for _, want := range []string{"Host check", "requests"} {
		if !strings.Contains(view, want) {
			t.Errorf("the scrolled reader shows %q; got %q", want, view)
		}
	}

	// esc returns to the list where the row was left.
	pressKey(c, "esc")
	if c.explaining {
		t.Error("esc closes the reader")
	}
	if row := c.rows[c.selectedIdx].host; row == nil || row.Name != "box1" {
		t.Error("the reader returns to the host row it opened from")
	}
}

// Esc abandons the form with nothing sent.
func TestConfigPaneHostAddEsc(t *testing.T) {
	c := newHostTestPane(t, twoHosts(), "")
	called := false
	c.editHosts = func(edit config.HostsEdit) (*config.SetResult, string, error) {
		called = true
		return nil, "", nil
	}
	c.beginHostAdd()
	typeInto(c, "box3")
	pressKey(c, "esc")
	if c.hosts.adding {
		t.Error("esc leaves the add view")
	}
	if called {
		t.Error("esc must not send the half-typed form")
	}
}
