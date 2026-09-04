package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The Accounts section of the config overlay (#3385) — the owner's ask, in his
// words: "I'd like to be able to do the logging in from the config tab of the
// TUI or the web", and then "can I click a button in config to spawn a tmux
// session with the login instead?".
//
// IT IS NOT CONFIG, AND IT MUST NOT LOOK LIKE IT. #3385 raised this as the
// question to settle before building: there is no account key anywhere in
// config/, and an account is a 0700 directory under <AF_HOME>/accounts/. A row
// that renders like a config row implies a settable key participating in the
// precedence chain, which this is not. So the section carries its own heading
// saying what these are, its rows show a STATE rather than a value, and neither
// of the two verbs here (register, log in) goes anywhere near the config write
// path — they are daemon calls the host makes, exactly as the assistant button
// is (#2453).
//
// The pane holds no roster and no login command of its own. Both come from the
// daemon, which reads them from the one place that has them
// (internal/agentaccount), so a fourth verified agent appears here with no edit
// to this file — the same rule the config rows follow with the manifest.

// AccountRow is one line of the Accounts section: either a registered account,
// or the "register" affordance for an agent.
type AccountRow struct {
	Agent string
	// Name is the account name. Empty exactly when Register is true.
	Name string
	// LoggedIn is whether the AGENT's own credential file is present in the
	// account directory. It is a claim about a file, not about a working
	// identity: af answers it by stat and never opens the credential, so a
	// revoked or expired one still reads as present. The copy says "logged in",
	// never "valid".
	LoggedIn bool
	// RegistrationOnly marks an agent whose accounts cannot yet scope a session.
	RegistrationOnly bool
	// Register makes this the "+ register an account" line for Agent rather than
	// an account of its own.
	Register bool
}

// AccountRequestKind is which verb the user asked for.
type AccountRequestKind int

const (
	// AccountRequestNone is the zero value: nothing was asked for.
	AccountRequestNone AccountRequestKind = iota
	// AccountRequestLogin asks the host to run the agent's login flow for an
	// account and hand it the terminal.
	AccountRequestLogin
	// AccountRequestRegister asks the host to create an account's directory.
	AccountRequestRegister
)

// AccountRequest is what the pane asks the host to do.
//
// The pane cannot do either itself: both are daemon round trips, and a login is
// also a full-screen terminal handover. This is the same division the config
// assistant already uses — the pane records an intent, the app performs it (see
// TakeAssistantRequest) — because a pane that shells out is a pane that owns a
// lifecycle it cannot see the end of.
type AccountRequest struct {
	Kind  AccountRequestKind
	Agent string
	Name  string
}

// accountsSection is the pane's account state: the rows the daemon reported, the
// register field, and the pending request.
type accountsSection struct {
	rows   []AccountRow
	loaded bool
	// unavailable is why the accounts could not be read, rendered in place of the
	// rows. A section that silently shows nothing is indistinguishable from "you
	// have no accounts", and those need different actions from the operator.
	unavailable string

	// registering is the inline name field for a "+ register" row. It is a
	// separate field from the config pane's value input on purpose: sharing one
	// would let an abandoned account name be committed to a config key, or the
	// reverse.
	registering bool
	registerFor string
	input       textinput.Model

	request AccountRequest

	// status is the outcome of the last register or login attempt, shown in the
	// pane's own status line.
	status        string
	statusIsError bool
}

var (
	accountStateOKStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	accountStateOffStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	accountRegisterStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("36"))
	accountAgentNameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
)

// accountsHeading is what the section calls itself. Short, because the pane
// renders headings in caps and a whole sentence shouted is exactly the
// caps-emphasis this repo's copy conventions forbid.
const accountsHeading = "Accounts"

// accountsHeadingNote is the line under it, and it is the answer to #3385's
// placement question rather than decoration: there is no account key anywhere in
// config/, and a row that reads as a config row implies a settable key
// participating in the precedence chain. The note says what these are before the
// first row can suggest otherwise.
const accountsHeadingNote = "agent identities, not config keys · af runs the agent's own login and never reads the credential"

// SetAccounts loads the section from what the daemon reported.
//
// agents is the roster an account can be registered for, and it is separate from
// the accounts themselves for a reason: a fresh install has none, and the
// register affordance still has to be offered. An error replaces the rows rather
// than emptying them silently.
func (c *ConfigPane) SetAccounts(accounts []AccountRow, agents []string, err error) {
	c.accounts.rows = nil
	c.accounts.unavailable = ""
	c.accounts.loaded = true
	if err != nil {
		c.accounts.unavailable = err.Error()
		c.rebuildRows()
		return
	}
	// Grouped by agent, each agent's accounts followed by its register line, so
	// the way to add one is where you looked for the one that is missing.
	byAgent := make(map[string][]AccountRow, len(agents))
	for _, account := range accounts {
		byAgent[account.Agent] = append(byAgent[account.Agent], account)
	}
	for _, agent := range agents {
		c.accounts.rows = append(c.accounts.rows, byAgent[agent]...)
		registrationOnly := false
		if existing := byAgent[agent]; len(existing) > 0 {
			registrationOnly = existing[0].RegistrationOnly
		}
		c.accounts.rows = append(c.accounts.rows, AccountRow{
			Agent: agent, Register: true, RegistrationOnly: registrationOnly,
		})
	}
	c.rebuildRows()
}

// AccountsLoaded reports whether SetAccounts has run. The host uses it to decide
// whether to render the section at all — an overlay opened before the daemon
// answered shows the config rows it already has rather than an empty Accounts
// heading that looks like "you have none".
func (c *ConfigPane) AccountsLoaded() bool { return c.accounts.loaded }

// TakeAccountRequest reports what the user asked for since the last call,
// clearing it. Read-and-clear, like TakeAssistantRequest, so one keypress
// produces exactly one login.
func (c *ConfigPane) TakeAccountRequest() AccountRequest {
	req := c.accounts.request
	c.accounts.request = AccountRequest{}
	return req
}

// SetAccountStatus reports the outcome of a request the host performed. A
// failure stays visible in the pane the user is still looking at, rather than
// behind an overlay that has already closed.
func (c *ConfigPane) SetAccountStatus(text string, isError bool) {
	c.accounts.status = text
	c.accounts.statusIsError = isError
}

// accountRows renders the section into the pane's flattened row list. It is
// appended AFTER the config tiers: the config keys are what the overlay is for,
// and a credential section above them would push them down the screen.
func (c *ConfigPane) appendAccountRows() {
	if !c.accounts.loaded {
		return
	}
	c.rows = append(c.rows, configRow{heading: accountsHeading})
	for i := range c.accounts.rows {
		account := c.accounts.rows[i]
		c.rows = append(c.rows, configRow{account: &account})
	}
}

// selectedAccount returns the account row under the cursor, or nil.
func (c *ConfigPane) selectedAccount() *AccountRow {
	if c.selectedIdx < 0 || c.selectedIdx >= len(c.rows) {
		return nil
	}
	return c.rows[c.selectedIdx].account
}

// handleAccountKey routes a key while an account row is selected. It returns
// false when the key is not the section's, so the pane's own handling runs.
func (c *ConfigPane) handleAccountKey(msg tea.KeyMsg) bool {
	account := c.selectedAccount()
	if account == nil {
		return false
	}
	switch msg.String() {
	case "enter":
		if account.Register {
			c.beginRegister(account.Agent)
			return true
		}
		// The login is a full-screen terminal handover the host performs, so the
		// pane closes as it asks — the same shape as the assistant button. Focus is
		// dropped FIRST, because SetFocus(false) clears a pending request as part of
		// its reset, and then the request is recorded so closing does not wipe the
		// intent that is closing it.
		c.SetFocus(false)
		c.accounts.request = AccountRequest{
			Kind: AccountRequestLogin, Agent: account.Agent, Name: account.Name,
		}
		return true
	}
	return false
}

// beginRegister opens the inline name field for an agent.
func (c *ConfigPane) beginRegister(agent string) {
	c.accounts.registering = true
	c.accounts.registerFor = agent
	c.accounts.status = ""
	c.accounts.statusIsError = false
	c.clearStatus()
	c.accounts.input = textinput.New()
	c.accounts.input.Placeholder = "account name"
	c.accounts.input.Width = c.accountInputWidth()
	c.accounts.input.Focus()
}

func (c *ConfigPane) accountInputWidth() int {
	width := c.width - 24
	if width < 12 {
		width = 12
	}
	return width
}

// cancelRegister abandons the name field without creating anything.
func (c *ConfigPane) cancelRegister() {
	c.accounts.registering = false
	c.accounts.registerFor = ""
	c.accounts.input.SetValue("")
	c.accounts.input.Blur()
}

// handleRegisterKey drives the name field. Enter asks the host to register;
// esc abandons. Everything else is a rune for the field.
//
// The pane does NOT validate the name. agentaccount.ValidateName is the one
// rule, and it refuses on the daemon where the directory is created; a second
// copy here is how a UI comes to accept a name the writer rejects — the same
// argument commitEdit makes about the config validator.
func (c *ConfigPane) handleRegisterKey(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEsc:
		c.cancelRegister()
		return true
	case tea.KeyEnter:
		name := strings.TrimSpace(c.accounts.input.Value())
		agent := c.accounts.registerFor
		c.cancelRegister()
		if name == "" {
			return true
		}
		c.accounts.request = AccountRequest{
			Kind: AccountRequestRegister, Agent: agent, Name: name,
		}
		return true
	default:
		var cmd tea.Cmd
		c.accounts.input, cmd = c.accounts.input.Update(msg)
		_ = cmd
		return true
	}
}

// renderAccountRow renders one account line: cursor, agent · name, and its
// state — or the register affordance, with its field when it is open.
func (c *ConfigPane) renderAccountRow(i int, account AccountRow) string {
	var b strings.Builder
	selected := i == c.selectedIdx

	cursor := "  "
	if selected {
		cursor = configSelectedStyle.Render("› ")
	}
	b.WriteString(cursor)

	if account.Register {
		label := "+ register a " + account.Agent + " account"
		if selected {
			b.WriteString(configSelectedStyle.Render(label))
		} else {
			b.WriteString(accountRegisterStyle.Render(label))
		}
		if selected && c.accounts.registering && c.accounts.registerFor == account.Agent {
			b.WriteString("  ")
			b.WriteString(c.accounts.input.View())
		}
		return c.fitPaneLine(b.String()) + "\n"
	}

	label := account.Agent + " · " + account.Name
	if selected {
		b.WriteString(configSelectedStyle.Render(label))
	} else {
		b.WriteString(accountAgentNameStyle.Render(label))
	}
	b.WriteString("  ")
	if account.LoggedIn {
		b.WriteString(accountStateOKStyle.Render("logged in"))
	} else {
		b.WriteString(accountStateOffStyle.Render("not logged in"))
	}
	line := c.fitPaneLine(b.String()) + "\n"

	if !selected {
		return line
	}
	var out strings.Builder
	out.WriteString(line)
	// The selected row explains itself, in the same place a config row's purpose
	// goes. What it must convey is that af runs the AGENT's flow and never sees
	// the credential — the property the whole feature rests on.
	out.WriteString(c.wrapIndented(accountRowPurpose(account), configPurposeStyle))
	if account.RegistrationOnly {
		out.WriteString(c.wrapIndented(
			"A session cannot be scoped to a "+account.Agent+" account yet; registering and logging in work.",
			configHintStyle))
	}
	return out.String()
}

// accountRowPurpose is the one sentence under the selected account.
func accountRowPurpose(account AccountRow) string {
	if account.LoggedIn {
		return fmt.Sprintf(
			"Holds a %s credential · ↵ runs %s's own login again in a tmux session scoped to this account, "+
				"replacing it. af never reads the credential.", account.Agent, account.Agent)
	}
	return fmt.Sprintf(
		"No %s credential yet · ↵ runs %s's own login in a tmux session scoped to this account and hands you "+
			"the terminal. af never reads the credential.", account.Agent, account.Agent)
}

// renderAccountsUnavailable renders the section's failure line in place of rows.
func (c *ConfigPane) renderAccountsUnavailable() string {
	return c.wrapIndented("Accounts could not be read: "+c.accounts.unavailable, configErrorStyle)
}
