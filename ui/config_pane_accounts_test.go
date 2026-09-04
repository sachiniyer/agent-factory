package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
)

// The Accounts section of the config overlay (#3385). What these pin is the part
// a screenshot cannot: that an account row is not a config row — it does not
// open the value editor, it does not go near the config write path — and that
// the two verbs leave the pane as requests for the host rather than as work the
// pane did itself.

// accountsPane builds a pane with one config key and a populated Accounts
// section, sized so rendering is not degenerate.
func accountsPane(t *testing.T, accounts []AccountRow, agents []string) *ConfigPane {
	t.Helper()
	pane := NewConfigPane()
	pane.SetSize(100, 40)
	pane.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Purpose: "the agent a new session runs", Tier: 1,
	}}, "/tmp/config.toml")
	pane.SetAccounts(accounts, agents, nil)
	pane.SetFocus(true)
	return pane
}

// selectAccount moves the cursor onto the first row matching agent/name (or the
// register row for agent when name is empty), failing if there is none.
func selectAccount(t *testing.T, pane *ConfigPane, agent, name string) {
	t.Helper()
	for i, row := range pane.rows {
		if row.account == nil || row.account.Agent != agent {
			continue
		}
		if name == "" && row.account.Register {
			pane.selectedIdx = i
			return
		}
		if !row.account.Register && row.account.Name == name {
			pane.selectedIdx = i
			return
		}
	}
	t.Fatalf("no account row for %s/%q; rows: %v", agent, name, accountRowLabels(pane))
}

func accountRowLabels(pane *ConfigPane) []string {
	var out []string
	for _, row := range pane.rows {
		if row.account != nil {
			if row.account.Register {
				out = append(out, row.account.Agent+"/+register")
			} else {
				out = append(out, row.account.Agent+"/"+row.account.Name)
			}
		}
	}
	return out
}

func accountKey(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// The section renders every registered account with its state, and offers a way
// to add one for EVERY agent on the roster — including an agent with no accounts
// yet, which is the whole first-use case.
func TestAccountsSectionListsAccountsAndOffersRegistrationPerAgent(t *testing.T) {
	pane := accountsPane(t, []AccountRow{
		{Agent: "claude", Name: "work", LoggedIn: true},
		{Agent: "codex", Name: "personal"},
	}, []string{"claude", "codex", "gemini"})

	got := accountRowLabels(pane)
	want := []string{
		"claude/work", "claude/+register",
		"codex/personal", "codex/+register",
		"gemini/+register",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("account rows = %v, want %v", got, want)
	}

	view := pane.String()
	for _, fragment := range []string{"claude · work", "logged in", "codex · personal", "not logged in",
		"+ register a gemini account"} {
		if !strings.Contains(view, fragment) {
			t.Fatalf("the section does not render %q:\n%s", fragment, view)
		}
	}
}

// The heading has to say what these rows ARE. #3385's placement question is
// exactly this: there is no account key anywhere in config/, and a row that
// reads as a config row implies a settable key participating in the precedence
// chain.
func TestAccountsSectionSaysItIsNotConfig(t *testing.T) {
	pane := accountsPane(t, []AccountRow{{Agent: "codex", Name: "work"}}, []string{"codex"})
	view := pane.String()
	if !strings.Contains(view, "not config keys") {
		t.Fatalf("the Accounts heading does not distinguish itself from config:\n%s", view)
	}
}

// Enter on an account is a LOGIN REQUEST, not an edit. The pane must not open
// the config value editor over a row that has no config key — that editor's
// commit path writes to config.
func TestAccountsEnterRequestsALoginAndNeverOpensTheValueEditor(t *testing.T) {
	pane := accountsPane(t, []AccountRow{{Agent: "codex", Name: "work"}}, []string{"codex"})
	selectAccount(t, pane, "codex", "work")
	pane.HandleKeyPress(accountKey("enter"))

	if pane.editing {
		t.Fatal("enter on an account row opened the config value editor")
	}
	req := pane.TakeAccountRequest()
	if req.Kind != AccountRequestLogin || req.Agent != "codex" || req.Name != "work" {
		t.Fatalf("account request = %+v, want a codex/work login", req)
	}
	// The overlay closes as it asks, so the host's terminal handover has a clean
	// screen — the same shape as the assistant button.
	if pane.HasFocus() {
		t.Fatal("the pane kept focus while asking for a full-screen login takeover")
	}
	// Read-and-clear: one keypress is one login.
	if again := pane.TakeAccountRequest(); again.Kind != AccountRequestNone {
		t.Fatalf("the login request fired twice: %+v", again)
	}
}

// Enter on a register row opens an inline name field; enter again asks the host
// to create it. The pane keeps focus throughout — registering is a step on the
// way to logging in, not a reason to close the surface.
func TestAccountsRegisterRowCollectsANameAndRequestsIt(t *testing.T) {
	pane := accountsPane(t, nil, []string{"codex"})
	selectAccount(t, pane, "codex", "")
	pane.HandleKeyPress(accountKey("enter"))
	if !pane.accounts.registering {
		t.Fatal("enter on the register row did not open the name field")
	}
	if !pane.HasFocus() {
		t.Fatal("opening the register field closed the overlay")
	}
	for _, r := range "work" {
		pane.HandleKeyPress(accountKey(string(r)))
	}
	pane.HandleKeyPress(accountKey("enter"))

	req := pane.TakeAccountRequest()
	if req.Kind != AccountRequestRegister || req.Agent != "codex" || req.Name != "work" {
		t.Fatalf("account request = %+v, want a codex/work registration", req)
	}
	if pane.accounts.registering {
		t.Fatal("the name field stayed open after committing")
	}
	if !pane.HasFocus() {
		t.Fatal("registering closed the overlay; the user is still working in it")
	}
}

// While the name field is open every rune is a character. "a" must not toggle
// the advanced tier and "C" must not open the assistant — the same rule the
// config value field already keeps.
func TestAccountsRegisterFieldTakesRunesNotCommands(t *testing.T) {
	pane := accountsPane(t, nil, []string{"claude"})
	selectAccount(t, pane, "claude", "")
	pane.HandleKeyPress(accountKey("enter"))
	for _, r := range "aC" {
		pane.HandleKeyPress(accountKey(string(r)))
	}
	if pane.showAdvanced {
		t.Fatal("typing 'a' into an account name toggled the advanced tier")
	}
	if pane.TakeAssistantRequest() {
		t.Fatal("typing 'C' into an account name asked for the config assistant")
	}
	if got := pane.accounts.input.Value(); got != "aC" {
		t.Fatalf("account name field holds %q, want %q", got, "aC")
	}
	// And the host must know a field is taking runes, or the configured quit key
	// would exit the TUI mid-name (#1727's rule, for this field).
	if !pane.IsEditing() {
		t.Fatal("IsEditing does not report the account name field, so 'q' would quit instead of typing")
	}
}

// Escape abandons the name field without creating anything, and leaves the
// overlay open — the user backed out of one row, not out of config.
func TestAccountsRegisterFieldEscapeCreatesNothing(t *testing.T) {
	pane := accountsPane(t, nil, []string{"codex"})
	selectAccount(t, pane, "codex", "")
	pane.HandleKeyPress(accountKey("enter"))
	pane.HandleKeyPress(accountKey("x"))
	pane.HandleKeyPress(accountKey("esc"))

	if pane.accounts.registering {
		t.Fatal("escape left the name field open")
	}
	if req := pane.TakeAccountRequest(); req.Kind != AccountRequestNone {
		t.Fatalf("escape still asked for %+v", req)
	}
	if !pane.HasFocus() {
		t.Fatal("escape from the name field closed the whole overlay")
	}
}

// An empty name asks for nothing. Committing it would send a request the daemon
// would refuse, and reporting that refusal to a user who pressed enter on an
// empty field is noise where doing nothing is the answer.
func TestAccountsRegisterRefusesAnEmptyNameLocally(t *testing.T) {
	pane := accountsPane(t, nil, []string{"codex"})
	selectAccount(t, pane, "codex", "")
	pane.HandleKeyPress(accountKey("enter"))
	pane.HandleKeyPress(accountKey("enter"))
	if req := pane.TakeAccountRequest(); req.Kind != AccountRequestNone {
		t.Fatalf("an empty name asked for %+v", req)
	}
}

// A section that shows nothing is indistinguishable from "you have no accounts",
// and those need different actions. A read failure says so, in place of the rows.
func TestAccountsSectionReportsAFailedReadRatherThanLookingEmpty(t *testing.T) {
	pane := NewConfigPane()
	pane.SetSize(100, 40)
	pane.SetEntries([]config.ConfigEntry{{Key: "default_program", Value: "claude", Tier: 1}}, "/tmp/config.toml")
	pane.SetAccounts(nil, nil, errors.New("the daemon did not answer"))
	pane.SetFocus(true)

	view := pane.String()
	if !strings.Contains(view, "could not be read") || !strings.Contains(view, "the daemon did not answer") {
		t.Fatalf("a failed account read is not reported:\n%s", view)
	}
	if len(accountRowLabels(pane)) != 0 {
		t.Fatalf("a failed read still produced rows: %v", accountRowLabels(pane))
	}
}

// Before the daemon has answered there is no section at all, rather than an
// empty Accounts heading that reads as "you have none".
func TestAccountsSectionIsAbsentUntilLoaded(t *testing.T) {
	pane := NewConfigPane()
	pane.SetSize(100, 40)
	pane.SetEntries([]config.ConfigEntry{{Key: "default_program", Value: "claude", Tier: 1}}, "/tmp/config.toml")
	pane.SetFocus(true)
	if strings.Contains(pane.String(), accountsHeading) {
		t.Fatalf("the Accounts heading rendered before any account was loaded:\n%s", pane.String())
	}
	if pane.AccountsLoaded() {
		t.Fatal("AccountsLoaded is true before SetAccounts ran")
	}
}

// The selected account explains what enter will do, and says the thing the whole
// feature rests on: af runs the agent's own flow and never reads the credential.
func TestSelectedAccountExplainsThatAfNeverReadsTheCredential(t *testing.T) {
	pane := accountsPane(t, []AccountRow{{Agent: "codex", Name: "work"}}, []string{"codex"})
	selectAccount(t, pane, "codex", "work")
	view := pane.String()
	for _, fragment := range []string{"never reads the credential", "own login"} {
		if !strings.Contains(view, fragment) {
			t.Fatalf("the selected account does not say %q:\n%s", fragment, view)
		}
	}
	if !strings.Contains(view, "↵ log in") {
		t.Fatalf("the hints do not offer the login verb on an account row:\n%s", view)
	}
}

// A logged-in account says the login will REPLACE what is there. Re-running a
// flow that overwrites an identity without saying so is the kind of surprise the
// accounts feature exists to avoid.
func TestSelectedLoggedInAccountSaysTheLoginReplacesIt(t *testing.T) {
	pane := accountsPane(t, []AccountRow{{Agent: "claude", Name: "work", LoggedIn: true}}, []string{"claude"})
	selectAccount(t, pane, "claude", "work")
	if !strings.Contains(pane.String(), "replacing it") {
		t.Fatalf("a logged-in account does not say the login replaces its credential:\n%s", pane.String())
	}
}

// A registration-only agent's rows say so where the user is about to spend a
// real login on one — the same sentence every other surface carries.
func TestRegistrationOnlyAccountSaysASessionCannotUseItYet(t *testing.T) {
	pane := accountsPane(t,
		[]AccountRow{{Agent: "gemini", Name: "work", RegistrationOnly: true}}, []string{"gemini"})
	selectAccount(t, pane, "gemini", "work")
	if !strings.Contains(pane.String(), "cannot be scoped") {
		t.Fatalf("a registration-only account does not warn before a login:\n%s", pane.String())
	}
}

// The status line reports the host's outcome, and an error is rendered as one.
func TestAccountStatusRendersTheHostsOutcome(t *testing.T) {
	pane := accountsPane(t, []AccountRow{{Agent: "codex", Name: "work"}}, []string{"codex"})
	pane.SetAccountStatus("account name \"Work\" collides with existing account \"work\"", true)
	if !strings.Contains(pane.String(), "collides with existing account") {
		t.Fatalf("the daemon's refusal did not reach the pane:\n%s", pane.String())
	}
	// Closing clears it: a refusal from minutes ago must not greet the next open,
	// which is the same reset the config echo goes through.
	pane.SetFocus(false)
	pane.SetFocus(true)
	if strings.Contains(pane.String(), "collides with existing account") {
		t.Fatalf("a stale account status survived a close:\n%s", pane.String())
	}
}

// Navigation crosses the boundary between config rows and account rows without
// stopping, and the config keys keep working after the section exists.
func TestConfigRowsStillEditWithAccountsPresent(t *testing.T) {
	pane := accountsPane(t, []AccountRow{{Agent: "codex", Name: "work"}}, []string{"codex"})
	// The first selectable row is the config key.
	pane.selectedIdx = 0
	pane.clampSelection()
	if pane.selectedEntry() == nil {
		t.Fatal("the config key is not selectable with an Accounts section present")
	}
	pane.HandleKeyPress(accountKey("enter"))
	if !pane.editing {
		t.Fatal("enter on a config row no longer opens the value editor")
	}
	if req := pane.TakeAccountRequest(); req.Kind != AccountRequestNone {
		t.Fatalf("editing a config key produced an account request: %+v", req)
	}
}
