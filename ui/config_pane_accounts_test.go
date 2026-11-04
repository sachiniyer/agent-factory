package ui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
func TestAccountsSectionOffersAgentLogin(t *testing.T) {
	pane := accountsPane(t, []AccountRow{{Agent: "codex", Name: "work"}}, []string{"codex"})
	view := pane.String()
	if !strings.Contains(view, "login flow") {
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
func TestSelectedAccountExplainsHowToLogIn(t *testing.T) {
	pane := accountsPane(t, []AccountRow{{Agent: "codex", Name: "work"}}, []string{"codex"})
	selectAccount(t, pane, "codex", "work")
	view := pane.String()
	for _, fragment := range []string{"Log in to codex", "URL and device code"} {
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
	if !strings.Contains(pane.String(), "replace it") {
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

// Remote login has no terminal transport. Its refusal belongs to the account,
// survives reopening, and never prevents registration on the same daemon.
func TestAccountsRemoteLoginRefusalStaysInSection(t *testing.T) {
	pane := accountsPane(t, []AccountRow{{Agent: "codex", Name: "work"}}, []string{"codex"})
	const refusal = "Run af accounts login on the daemon host (http://buildbox:8443)."
	pane.SetAccountLoginRefusal(refusal)
	selectAccount(t, pane, "codex", "work")
	for i := 0; i < 2; i++ {
		pane.HandleKeyPress(accountKey("enter"))
		if !pane.HasFocus() || pane.editing {
			t.Fatal("refused login must retain the Accounts section")
		}
		if req := pane.TakeAccountRequest(); req.Kind != AccountRequestNone {
			t.Fatalf("refused login requested %+v", req)
		}
		if view := pane.String(); !strings.Contains(view, refusal) || strings.Contains(view, "↵ log in") {
			t.Fatalf("refusal must replace the login offer:\n%s", view)
		}
		pane.SetFocus(false)
		pane.SetFocus(true)
	}
	selectAccount(t, pane, "codex", "")
	pane.HandleKeyPress(accountKey("enter"))
	pane.HandleKeyPress(accountKey("new-work"))
	pane.HandleKeyPress(accountKey("enter"))
	if req := pane.TakeAccountRequest(); req.Kind != AccountRequestRegister || req.Name != "new-work" {
		t.Fatalf("remote registration request = %+v", req)
	}
	pane.SetAccountLoginRefusal("")
	selectAccount(t, pane, "codex", "work")
	pane.HandleKeyPress(accountKey("enter"))
	if req := pane.TakeAccountRequest(); req.Kind != AccountRequestLogin {
		t.Fatalf("clearing remote policy did not restore local login: %+v", req)
	}
}

// openRegisterFieldAt builds a pane at the given size with the register field
// open for agent and name typed into it — the state the width bug lives in.
// The pane carries one config key so the Accounts section renders below the
// tiers, exactly as it does in production.
func openRegisterFieldAt(t *testing.T, w, h int, agent, name string) *ConfigPane {
	t.Helper()
	c := NewConfigPane()
	c.SetSize(w, h)
	c.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Purpose: "the agent a new session runs", Tier: 1,
	}}, "/tmp/config.toml")
	c.SetAccounts(nil, []string{agent}, nil)
	c.SetFocus(true)
	selectAccount(t, c, agent, "")
	c.HandleKeyPress(accountKey("enter"))
	if !c.accounts.registering {
		t.Fatal("enter on the register row did not open the name field")
	}
	for _, r := range name {
		c.HandleKeyPress(accountKey(string(r)))
	}
	return c
}

// registerRowFromView finds the selected register row in the rendered view by
// its selection cursor, failing if it is absent.
func registerRowFromView(t *testing.T, c *ConfigPane) string {
	t.Helper()
	for _, line := range strings.Split(c.String(), "\n") {
		if strings.Contains(line, "› ") {
			return line
		}
	}
	t.Fatal("the register row did not render at all")
	return ""
}

// TestAccountsRegisterFieldFitsItsRowWithoutClipping tests accountRowSplit's
// arithmetic directly, WITHOUT the fitPaneLine backstop in the way — the same
// assertion TestConfigPaneEditFieldFitsItsRowWithoutClipping makes for the value
// editor.
//
// A field one cell too wide still leaves the pane fitting its box — the backstop
// clips the row — so the width sweep passes while the user loses the tail of the
// name they are typing. This catches exactly that: the composed register row
// (marker + label + gap + input view) must fit within the pane at every width,
// without relying on fitPaneLine to clip it.
func TestAccountsRegisterFieldFitsItsRowWithoutClipping(t *testing.T) {
	for _, w := range configPaneSweepWidths {
		t.Run(fmt.Sprintf("w=%d", w), func(t *testing.T) {
			c := openRegisterFieldAt(t, w, paneHeight, "claude", strings.Repeat("x", 45))
			labelBudget, _ := c.accountRowSplit("claude")
			row := entryRowChromeWidth + labelBudget + lipgloss.Width(c.accounts.input.View())
			if row > w {
				t.Errorf("w=%d: the open register field composes a %d-cell row — the pane clips it, so the name loses its tail (labelBudget=%d, input.Width=%d, view=%d)",
					w, row, labelBudget, c.accounts.input.Width, lipgloss.Width(c.accounts.input.View()))
			}
			if c.accounts.input.Width < minEditFieldWidth {
				t.Errorf("w=%d: the register field was squeezed to %d cells, below the %d-cell floor",
					w, c.accounts.input.Width, minEditFieldWidth)
			}
		})
	}
}

// TestAccountsRegisterRowNeverWiderThanPane is the width invariant for the
// accounts register row — the same property TestConfigPaneNeverRendersALineWider
// ThanThePane guards for config entry rows, extended to the register field that
// the original sweep never exercised (it calls SetEntries, never SetAccounts).
//
// The register row is the one inline input the pane's width tests did not cover:
// it was sized independently of the row it renders into, so the composed row was
// always wider than the pane and fitPaneLine silently clipped the tail. This
// sweeps every width with the field open and a long name typed, demanding no
// rendered line exceed the pane.
func TestAccountsRegisterRowNeverWiderThanPane(t *testing.T) {
	for _, w := range configPaneSweepWidths {
		t.Run(fmt.Sprintf("w=%d", w), func(t *testing.T) {
			c := openRegisterFieldAt(t, w, paneHeight, "claude", strings.Repeat("x", 45))
			for _, line := range strings.Split(c.String(), "\n") {
				if got := lipgloss.Width(line); got > w {
					t.Fatalf("w=%d: rendered a %d-cell line into a %d-cell pane — the frame wraps it, which makes the height window's line count a lie.\n  line: %q",
						w, got, w, line)
				}
			}
		})
	}
}

// TestAccountsRegisterFieldSurvivesTheTightestRow is the accounts analogue of
// TestConfigPaneEditFieldSurvivesTheTightestRow: at af's supported minimum (a
// 40-column terminal, which app/render.go turns into 34 content cells), the
// register field must still be visible — its prompt and the typed value — even
// if the label yields to make room for it.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: at width 34 the entire typed name is
// invisible from the first character; only the "> " prompt survives the clip.
func TestAccountsRegisterFieldSurvivesTheTightestRow(t *testing.T) {
	const (
		w     = 34 // a 40-column terminal, af's supported minimum
		agent = "claude"
	)
	c := openRegisterFieldAt(t, w, paneHeight, agent, "work")

	row := registerRowFromView(t, c)
	if got := lipgloss.Width(row); got > w {
		t.Errorf("the register row is %d cells in a %d-cell pane: %q", got, w, row)
	}
	// The field must be there: its prompt, and the value it was filled with.
	if !strings.Contains(row, c.accounts.input.Prompt) {
		t.Errorf("the register field's prompt was clipped away — the user cannot see what they are editing: %q", row)
	}
	if !strings.Contains(row, "work") {
		t.Errorf("the typed name was clipped away, so the field is invisible while focused: %q", row)
	}
}

// TestAccountsRegisterFieldReflowsOnResize is the accounts analogue of
// TestConfigPaneEditFieldReflowsOnResize: narrowing the terminal
// mid-registration must reflow the field's viewport, not leave it describing the
// old width.
//
// textinput recomputes its horizontal viewport in handleOverflow, which runs
// from SetValue and SetCursor and NOT from a bare Width assignment. So without
// sizeAccountField's CursorEnd/SetCursor reflow dance, narrowing 72 -> 38
// updated Width while offset/offsetRight still described the old width: View()
// kept rendering the old, wider slice, fitPaneLine clipped it from the right,
// and the name's tail and the cursor vanished until the next keystroke.
//
// The assertion is on the FIELD, not the row: the row fits either way because the
// backstop clips it, which is exactly why this needs its own test.
func TestAccountsRegisterFieldReflowsOnResize(t *testing.T) {
	long := strings.Repeat("very-long-", 5) + "name" // 50 chars

	for _, tc := range []struct {
		name  string
		posOf func(n int) int
	}{
		{name: "cursor at the start", posOf: func(int) int { return 0 }},
		{name: "cursor in the interior", posOf: func(n int) int { return n / 2 }},
		{name: "cursor at the end", posOf: func(n int) int { return n }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openRegisterFieldAt(t, 72, paneHeight, "claude", long)
			pos := tc.posOf(len(c.accounts.input.Value()))
			c.accounts.input.SetCursor(pos)

			// The terminal narrows while the registration is open. SetSize is
			// what a resize calls.
			c.SetSize(38, paneHeight)

			budget := c.accounts.input.Width + lipgloss.Width(c.accounts.input.Prompt) + editFieldCursorWidth
			if got := lipgloss.Width(c.accounts.input.View()); got > budget {
				t.Fatalf("after narrowing to 38 with the %s, the field renders %d cells for a %d-cell budget (Width=%d) — the viewport still describes the old width, so the row gets clipped and the name's tail and cursor disappear",
					tc.name, got, budget, c.accounts.input.Width)
			}
			if c.accounts.input.Position() != pos {
				t.Errorf("the reflow moved the cursor: position %d, want %d", c.accounts.input.Position(), pos)
			}
		})
	}
}

// TestAccountsRegisterFieldUnboundedWhileUnsized is the accounts analogue of
// TestConfigPaneEditFieldUnboundedWhileUnsized: an unsized pane constrains
// nothing, and a pane that loses its size goes back to unbounded rather than
// keeping a stale width.
func TestAccountsRegisterFieldUnboundedWhileUnsized(t *testing.T) {
	c := NewConfigPane()
	c.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Tier: 1,
	}}, "/tmp/config.toml")
	c.SetAccounts(nil, []string{"claude"}, nil)
	c.SetFocus(true)
	selectAccount(t, c, "claude", "")
	c.HandleKeyPress(accountKey("enter"))
	if !c.accounts.registering {
		t.Fatal("enter did not open the register field")
	}
	if c.accounts.input.Width != 0 {
		t.Errorf("an unsized pane bounded its register field to %d cells", c.accounts.input.Width)
	}

	// A sized pane bounds the field.
	c.SetSize(72, paneHeight)
	if c.accounts.input.Width == 0 {
		t.Fatal("a sized pane left the register field unbounded")
	}

	// And a pane that loses its size goes back to unbounded rather than keeping
	// a stale width.
	c.SetSize(0, 0)
	if c.accounts.input.Width != 0 {
		t.Errorf("a pane resized to nothing kept a %d-cell register field width", c.accounts.input.Width)
	}
}

// TestAccountsRegisterFieldShowsTypedValueAtWidePane checks the specific
// scenario from the bug report: at width 72 with agent "claude", a name longer
// than the old field's visible budget (38 characters) is accepted into the field
// but must be SHOWN — not silently clipped by fitPaneLine while the field
// holds the full value.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: a 45-character name at width 72 was
// accepted (input.Value() holds all 45) but only 38 characters survived the
// row clip; enter submitted a name the user could not fully see.
func TestAccountsRegisterFieldShowsTypedValueAtWidePane(t *testing.T) {
	const w = 72
	name := strings.Repeat("x", 45) // 45 chars; the old field at width 72 showed only 38
	c := openRegisterFieldAt(t, w, paneHeight, "claude", name)

	// The field holds the full value — it always did, because textinput has no
	// CharLimit (the same reason the value editor preserves long values).
	if got := c.accounts.input.Value(); got != name {
		t.Fatalf("the field holds %q, want %q — the field must not truncate what it holds", got, name)
	}

	// The rendered row must not be wider than the pane — the field is sized to
	// fit the row, not clipped by fitPaneLine.
	row := registerRowFromView(t, c)
	if got := lipgloss.Width(row); got > w {
		t.Errorf("the register row is %d cells in a %d-cell pane: %q", got, w, row)
	}

	// And the field's own View() must fit within its budget — the viewport
	// scrolled to follow the cursor rather than rendering a stale-wide slice.
	budget := c.accounts.input.Width + lipgloss.Width(c.accounts.input.Prompt) + editFieldCursorWidth
	if got := lipgloss.Width(c.accounts.input.View()); got > budget {
		t.Errorf("the field's View() is %d cells for a %d-cell budget (Width=%d) — the viewport did not scroll to fit",
			got, budget, c.accounts.input.Width)
	}
}

// TestAccountsRegisterFieldWidthMatchesRowChrome verifies that the field width
// is derived from the row's actual chrome (marker + label + gap + prompt +
// cursor), not a flat constant. At a wide pane the field gets the full
// remaining budget; at a narrow pane the field floors at minEditFieldWidth and
// the label yields.
func TestAccountsRegisterFieldWidthMatchesRowChrome(t *testing.T) {
	agent := "claude"
	labelWidth := lipgloss.Width("+ register a " + agent + " account")
	fieldChrome := lipgloss.Width("> ") + editFieldCursorWidth

	// At a wide pane, the field gets everything left after the chrome.
	const wideW = 72
	c := openRegisterFieldAt(t, wideW, paneHeight, agent, "x")
	wantField := wideW - entryRowChromeWidth - labelWidth - fieldChrome
	if c.accounts.input.Width != wantField {
		t.Errorf("w=%d: field width = %d, want %d (pane - chrome - label - fieldChrome)",
			wideW, c.accounts.input.Width, wantField)
	}

	// At a narrow pane, the field floors at minEditFieldWidth and the label
	// yields.
	const narrowW = 34
	c2 := openRegisterFieldAt(t, narrowW, paneHeight, agent, "x")
	if c2.accounts.input.Width < minEditFieldWidth {
		t.Errorf("w=%d: field width = %d, below the %d-cell floor",
			narrowW, c2.accounts.input.Width, minEditFieldWidth)
	}
	// The label must have been truncated: the full label (27 cells) does not
	// fit alongside an 8-cell field in 34 cells.
	labelBudget, _ := c2.accountRowSplit(agent)
	if labelBudget >= labelWidth {
		t.Errorf("w=%d: the label was not truncated (budget=%d, full label=%d) — the field cannot have room",
			narrowW, labelBudget, labelWidth)
	}
}
