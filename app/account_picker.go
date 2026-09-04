package app

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// The naming form's account field (#3844).
//
// The daemon has accepted an account on create since #3051
// (CreateSessionRequest.Account, the CLI's `--account`), and #3385 gave the TUI
// config tab a section that registers and logs into one. What no UI could do was
// USE one: the walk-through ended with "now drop to a terminal and run
// `af sessions create --account`". This is that half.
//
// Four rules this file exists to hold, three of them borrowed straight from the
// backend field beside it (app/backend_picker.go) because a second, differently
// behaved picker on the same form is how two fields start disagreeing:
//
//  1. THE TUI KNOWS NO ACCOUNT OR AGENT NAMES. Every row is built from the
//     daemon's ListAccounts answer, so an account registered on the daemon host a
//     second ago — or an agent added to the account roster server-side — is
//     offered here with no change to this file.
//  2. AN ACCOUNT BELONGS TO AN AGENT. claude's "work" and codex's "work" are
//     different identities in different registries, so the list follows the
//     program the form currently has selected. A picker that showed every account
//     would be offering guaranteed failures.
//  3. AN UNUSABLE ROW IS LISTED WITH ITS REASON, NOT HIDDEN. A registration-only
//     account is shown and refused on pick, exactly as an unavailable backend is;
//     a not-logged-in one is shown, LABELLED, and selectable, because hiding it
//     would hide the account the user registered thirty seconds ago and is on
//     their way to logging into.
//  4. NO CREDENTIAL MATERIAL, IN EITHER DIRECTION. An account is a directory
//     name. Nothing on this path reads, transports or displays a secret.

// ambientAccount is the sentinel for the picker's first row — the identity every
// session ran as before this field existed. It is the EMPTY STRING on purpose: it
// is exactly what the create request omits on, so choosing it sends no account
// and the agent's own ambient credential decides. Any other sentinel would
// eventually be transmitted as a literal account name.
const ambientAccount = ""

// accountChoice is one row of the picker.
type accountChoice struct {
	// value is what goes on CreateSessionRequest.Account: ambientAccount, or an
	// account name to send verbatim.
	value string
	// label is the row's presentation text — the account name as the daemon
	// spelled it, or the ambient row's own wording.
	label string
	// agent is the agent whose registry this account lives in. Carried per row
	// rather than read back off the form, so a refusal names the agent the DAEMON
	// filed the account under even if the form has moved on.
	agent string
	// registrationOnly is the daemon's answer for this account's agent: registered
	// and loggable-into, but not yet able to scope a session. Refused on pick.
	registrationOnly bool
	// loggedIn is whether the agent's own credential file is present in the
	// account directory, by stat — never by reading it. False is a legitimate
	// choice, not a failure.
	loggedIn bool
}

// accountChoicesFrom turns the daemon's registry into the picker's rows: the
// ambient-identity row first, then every account the daemon listed FOR THIS
// AGENT, in the daemon's own order.
//
// The agent filter lives HERE, over each entry's own Agent field, rather than in
// the request — see openAccountPicker for why the whole registry is fetched. That
// makes the narrowing a property of the picker rather than a promise about the
// response, which matters because the failure it prevents is the identity kind: a
// codex account offered to a claude session is a create that fails, or worse, one
// that quietly does not.
func accountChoicesFrom(resp daemon.ListAccountsResponse, agent string) []accountChoice {
	choices := []accountChoice{{
		value: ambientAccount,
		label: "Ambient identity (the agent's own login)",
		agent: agent,
	}}
	for _, entry := range resp.Entries {
		if entry.Agent != agent {
			continue
		}
		choices = append(choices, accountChoice{
			value:            entry.Name,
			label:            entry.Name,
			agent:            entry.Agent,
			registrationOnly: entry.RegistrationOnly,
			loggedIn:         entry.LoggedIn,
		})
	}
	return choices
}

// item is the row as the selection overlay renders it: the label plus what the
// daemon said about it.
//
// Both markers are on the LIST, before any keypress, for the reason the backend
// picker marks an unusable backend: a bare account name among usable rows is a
// promise. "registration only" is a refusal the user will hit on pick; "not
// logged in" is not a refusal at all — the account works, it simply has no
// credential in it yet — and saying so is the whole of constraint 3.
func (c accountChoice) item() string {
	var marks []string
	if c.registrationOnly {
		marks = append(marks, "registration only")
	}
	if c.value != ambientAccount && !c.loggedIn {
		marks = append(marks, "not logged in")
	}
	if len(marks) == 0 {
		return c.label
	}
	out := c.label + " — " + marks[0]
	for _, mark := range marks[1:] {
		out += " · " + mark
	}
	return out
}

// accountRegistryMsg carries a fetched account registry back onto the event loop.
// naming is the instance whose form asked for it and agent is the agent it was
// asked for: the fetch is async, so by the time it lands the user may have
// submitted, cancelled, started naming a different session, or — the case unique
// to this field — changed the program, which changes which registry the answer
// belongs to.
type accountRegistryMsg struct {
	naming *session.Instance
	agent  string
	resp   daemon.ListAccountsResponse
	err    error
}

// listAccountsThroughDaemon is the fetch seam, mirroring listBackendsThroughDaemon:
// a package var so the unit suite can answer with a fixture instead of a daemon.
//
// It goes through the HTTP client rather than daemon.ListAccounts (the gob
// control client the config pane's Accounts section uses), because this answer
// has to come from the daemon the create will go to — which for a remote target
// is another machine, and the one whose home holds the account directories.
var listAccountsThroughDaemon = func(agent string) (daemon.ListAccountsResponse, error) {
	var resp daemon.ListAccountsResponse
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		resp, e = c.ListAccounts(agent)
		return e
	})
	if err != nil {
		return daemon.ListAccountsResponse{}, err
	}
	return resp, nil
}

// SetAccountListerForTest swaps the registry seam so a test can answer with a
// fixture — including account names no list in this process knows about, which is
// how "the TUI knows no account names" is provable rather than asserted.
func SetAccountListerForTest(f func(agent string) (daemon.ListAccountsResponse, error)) func() {
	prev := listAccountsThroughDaemon
	listAccountsThroughDaemon = f
	return func() { listAccountsThroughDaemon = prev }
}

// openAccountPicker starts the round trip that opens the account field. Fetched
// on demand rather than prefetched with every `n`, for the backend field's
// reasons plus one of its own: an account can be registered or logged into from
// another surface while this form is open, and the logged-in column is exactly
// the thing that changes.
//
// The whole registry is requested (an EMPTY agent) rather than the form's agent
// alone, and accountChoicesFrom narrows it. One round trip either way, and this
// shape is the one that keeps the roster: ListAccounts asked about an agent it
// does not support answers with an ERROR, and for an ordinary non-account agent
// like aider that is not an error at all — it is the plain fact that there is
// nothing to pick, which accountRosterNotice states from the same response's
// Agents list rather than from anything local.
func (m *home) openAccountPicker() (tea.Model, tea.Cmd) {
	naming := m.namingInstance
	if naming == nil {
		return m, nil
	}
	// The agent the FORM currently names, resolved through the same helper
	// `af sessions create --account` validates against (api/sessions.go), so the
	// picker and the create cannot disagree about which registry a program uses.
	agent := sessionenv.AgentForCommand(m.pendingProgram)
	if agent == "" {
		return m, m.handleNotice(fmt.Errorf(
			"af cannot tell which agent %q runs as, so it has no account registry to offer for it",
			m.pendingProgram))
	}
	fetch := listAccountsThroughDaemon
	return m, func() tea.Msg {
		resp, err := fetch("")
		return accountRegistryMsg{naming: naming, agent: agent, resp: resp, err: err}
	}
}

// handleAccountRegistry opens the picker over a delivered registry.
//
// The staleness guard is the whole reason this is a message rather than a direct
// call, and it has one clause the backend field does not need: the PROGRAM must
// still be the one that was asked about. Changing the program mid-fetch changes
// which agent's accounts are the right ones, and opening this list over the new
// program would offer identities from the wrong registry — the exact thing
// constraint 1 exists to prevent.
func (m *home) handleAccountRegistry(msg accountRegistryMsg) (tea.Model, tea.Cmd) {
	if m.state != stateNew || m.namingInstance == nil || m.namingInstance != msg.naming {
		return m, nil
	}
	if sessionenv.AgentForCommand(m.pendingProgram) != msg.agent {
		return m, nil
	}
	if msg.err != nil {
		// Lead with what failed and what it blocks; the daemon's own error text
		// follows. The naming form stays open — the field is optional, so a registry
		// we could not read must not cost the user their half-typed create.
		return m, m.handleError(fmt.Errorf("cannot list accounts for %s: %w", msg.agent, msg.err))
	}
	// An agent that is not on the daemon's account roster at all has no registry to
	// offer, and that is a fact about the agent rather than a failure: say so, name
	// the agents that do, and leave the form alone. The roster is the daemon's —
	// ListAccountsResponse.Agents is always the FULL one, deliberately not narrowed
	// by the request — so this stays true the day a fourth agent is verified.
	if !accountRosterHas(msg.resp.Agents, msg.agent) {
		return m, m.handleNotice(accountRosterNotice(msg.agent, msg.resp.Agents))
	}
	choices := accountChoicesFrom(msg.resp, msg.agent)
	if len(choices) == 1 {
		// Only the ambient row: this agent supports accounts but none is registered
		// on the daemon host yet. An empty-but-for-one-row modal answers nothing, so
		// point at where accounts are made instead.
		return m, m.handleNotice(fmt.Errorf(
			"no %s accounts are registered on the daemon host yet — register one in the config tab (%s) "+
				"or with `af accounts add %s <name>`",
			msg.agent, helpKey(keys.KeyConfigEditor), msg.agent))
	}

	selected := 0
	for i, choice := range choices {
		if choice.value == m.pendingAccount {
			selected = i
			break
		}
	}
	m.accountPickerChoices = choices
	items := make([]string, len(choices))
	for i, choice := range choices {
		items[i] = choice.item()
	}
	// The title names the agent, so the one property a create-time picker owes the
	// user — that this list is scoped to the program on the form — is visible
	// rather than implied.
	m.selectionOverlay = overlay.NewSelectionOverlay(fmt.Sprintf("Select %s account", msg.agent), items)
	m.selectionOverlay.SetSelectedIndex(selected)
	m.layoutSelectionOverlay()
	m.state = stateSelectAccount
	return m, nil
}

// accountRosterHas reports whether an agent is on the daemon's account roster.
func accountRosterHas(roster []string, agent string) bool {
	for _, candidate := range roster {
		if candidate == agent {
			return true
		}
	}
	return false
}

// accountRosterNotice explains that this program's agent has no account registry,
// naming the ones that do — from the DAEMON's roster, never a local list, so a
// client older than the daemon still names the right set.
func accountRosterNotice(agent string, roster []string) error {
	if len(roster) == 0 {
		return fmt.Errorf("the daemon reports no agents that support accounts, so there is nothing to pick for %s", agent)
	}
	return fmt.Errorf("%s sessions cannot be scoped to an account (agents that can: %s)",
		agent, strings.Join(roster, ", "))
}

// handleStateSelectAccount handles key events while the account field is open.
// Mirrors handleStateSelectBackend: the field is part of the create form, so
// submitting or escaping returns to naming rather than abandoning the create.
//
// A registration-only account is REFUSED here, at pick time, with the reason the
// CLI refuses `--account` with. Same trade the backend picker made and for the
// same reason: a modal overlay has nowhere to show a blocking explanation, and
// refusing on choice tells the user what is wrong while their create is still
// open and retryable.
func (m *home) handleStateSelectAccount(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.selectionOverlay == nil {
		m.accountPickerChoices = nil
		m.state = stateNew
		m.menu.SetState(ui.StateNewInstance)
		return m, nil
	}
	if !m.selectionOverlay.HandleKeyPress(msg) {
		return m, nil
	}

	submitted := m.selectionOverlay.IsSubmitted()
	idx := m.selectionOverlay.GetSelectedIndex()
	choices := m.accountPickerChoices

	m.selectionOverlay = nil
	m.accountPickerChoices = nil
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)

	if !submitted || idx < 0 || idx >= len(choices) {
		return m, nil
	}
	choice := choices[idx]
	if choice.registrationOnly {
		// A checked-and-failed precondition, like an unavailable backend: the user
		// can act on it (pick another account, or wait for the agent's launch proof),
		// and filing it as an operation failure would put ordinary user feedback in
		// front of ERROR monitoring.
		return m, m.handleNotice(registrationOnlyRefusal(choice.agent, choice.label))
	}
	m.pendingAccount = choice.value
	m.menu.SetNamingAccount(m.pendingAccount != ambientAccount)
	if m.pendingAccount != ambientAccount && !choice.loggedIn {
		// Selectable, and it says so — the second half of constraint 3. The row was
		// already marked in the list; this is the confirmation for the keypress,
		// which must always say something (#2020), and it is a NOTICE because
		// nothing failed: the session will be created, on an account that has no
		// credential in it yet.
		return m, m.handleNotice(fmt.Errorf(
			"account %q has no %s credential yet — the session will run as it until you log in from the config "+
				"tab (%s) or with `af accounts login %s %s`",
			choice.label, choice.agent, helpKey(keys.KeyConfigEditor), choice.agent, choice.label))
	}
	return m, nil
}

// clearPendingAccount drops the naming form's account back to the ambient
// identity and takes the status bar's "account ✓" with it. One helper because
// the two halves must move together: a cleared value with a stale hint is a form
// that says a session is scoped to an identity it will not be created with.
func (m *home) clearPendingAccount() {
	m.pendingAccount = ambientAccount
	m.menu.SetNamingAccount(false)
}

// registrationOnlyRefusal is what the picker says about an account whose agent
// cannot scope a session yet.
//
// The DAEMON is the authority on the state — AccountEntry.RegistrationOnly is
// computed by the build that owns the account registry, which may be newer than
// this one — so the flag is trusted and only the WORDING is resolved locally. Two
// arms, because those two facts can disagree:
//
//   - this build agrees the agent is registration-only: use the one sentence every
//     other surface says it with (sessionenv.AccountRegistrationOnlyReason), so the
//     picker cannot describe the state differently from `af sessions create
//     --account`, which refuses with it.
//   - it does not: a daemon newer than this client has named an agent, or a state,
//     this build has never heard of. Say what the daemon reported rather than
//     inventing a reason, and never fall through to no message at all (#2020).
//
// The shared sentence ENDS IN A URL when a follow-up issue exists, so it goes
// LAST and nothing is appended after it: a period fused to a link is one the
// terminal hands over as part of the address.
func registrationOnlyRefusal(agent, name string) error {
	if reason, ok := sessionenv.AccountRegistrationOnlyReason(agent); ok {
		return fmt.Errorf("account %q cannot scope a session: %s", name, reason)
	}
	return fmt.Errorf(
		"account %q cannot scope a session: the daemon reports %s accounts as registration only — one can be "+
			"registered and logged into, but a session cannot run as it yet", name, agent)
}

// accountSkewRefusal compares the account that came BACK on the created session
// with the one the user picked, and reports a mismatch.
//
// This is the version-skew guard (#3844 constraint 5), and it is the same check
// `af sessions create` runs at api/sessions.go: a daemon predating account
// support drops the field silently, so the session runs on the ambient identity
// while every visible signal in the UI says it is running as the account the user
// chose. That is the silent-wrong-identity outcome the whole feature exists to
// prevent, arriving through version skew instead of through the environment — and
// a UI is the surface where it would go unnoticed longest.
//
// The daemon is the authority on what it stored, so this reads what came back
// rather than what was sent. It NAMES BOTH accounts, and it names the session,
// because one now exists that has to be removed rather than used.
func accountSkewRefusal(requested string, started *session.Instance) error {
	if requested == ambientAccount {
		return nil
	}
	if started == nil {
		return fmt.Errorf(
			"session was created for account %q but the daemon returned nothing to check it against — it may be "+
				"running on the ambient identity. Verify with `af sessions list --json` before using it", requested)
	}
	if started.Account == requested {
		return nil
	}
	// Two shapes, two causes, so two remedies. A DROPPED field is the version-skew
	// one this guard was built for and names the upgrade; a field applied as some
	// OTHER account is not skew at all — the daemon knew the field and stored
	// something else — so saying "upgrade the daemon" there would send the user to
	// fix the wrong thing. Both end the same way, because both leave a session
	// running as an identity nobody chose.
	if started.Account == "" {
		return fmt.Errorf(
			"session %q was created but the daemon did not apply account %q — it is running on the ambient "+
				"identity. The running daemon predates account support; upgrade it (af daemon restart after an "+
				"upgrade), then kill this session and create it again",
			started.Title, requested)
	}
	return fmt.Errorf(
		"session %q was created but the daemon applied account %q, not the %q that was picked — it is running as "+
			"an identity you did not choose. Kill this session and create it again",
		started.Title, started.Account, requested)
}
