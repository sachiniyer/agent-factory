package app

import (
	"fmt"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/ui"
)

// The app-side wiring for the config overlay's Accounts section (#3385).
//
// The pane records an intent and this performs it, which is the same division
// handleConfigAgent uses and for the same two reasons: both verbs are daemon
// round trips, and a login is a full-screen terminal handover whose end the pane
// cannot see. ui/ stays a renderer.
//
// A login opens the AGENT's own flow in a bare tmux session the daemon owns, and
// the TUI hands it the terminal with `tmux attach-session` — the config agent's
// mechanism, for the config agent's reason: a login pane has no Instance, so the
// WS PTY route (which resolves its byte source through the daemon's instance
// map) cannot reach it, and being reachable that way would mean being a row in
// the session list.
//
// Nothing here touches a credential. af sets one directory and hands over the
// terminal; the state these functions read back is the presence of the agent's
// own credential file, by stat.

// The daemon seams, as vars so the TUI tests can drive every branch without a
// daemon. Same shape as spawnConfigAgent/reapConfigAgent above.
var (
	listAccountsForPane = daemon.ListAccounts
	registerAccount     = daemon.RegisterAccount
	startAccountLogin   = daemon.AccountLogin
)

// SetAccountSeamsForTest swaps the three daemon calls behind the Accounts
// section and returns a restore func, matching SetConfigAgentSpawnerForTest.
func SetAccountSeamsForTest(
	list func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error),
	register func(daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error),
	login func(daemon.AccountLoginRequest) (daemon.AccountLoginResponse, error),
) func() {
	prevList, prevRegister, prevLogin := listAccountsForPane, registerAccount, startAccountLogin
	listAccountsForPane, registerAccount, startAccountLogin = list, register, login
	return func() {
		listAccountsForPane, registerAccount, startAccountLogin = prevList, prevRegister, prevLogin
	}
}

// loadAccountsIntoPane fills the config overlay's Accounts section.
//
// It is called on every open, like the config read beside it, so the section
// shows the accounts as they are NOW — including one registered from the CLI or
// logged in on another surface since the TUI started.
//
// A failure becomes the section's own message rather than blocking the overlay:
// the config editor is still useful when the accounts cannot be read, and an
// operator who came to change a key should not be turned away because a daemon
// call failed.
func (m *home) loadAccountsIntoPane() {
	resp, err := listAccountsForPane(daemon.ListAccountsRequest{})
	if err != nil {
		log.WarningLog.Printf("accounts: could not read the registered accounts for the config pane: %v", err)
		m.configPane.SetAccounts(nil, nil, err)
		return
	}
	rows := make([]ui.AccountRow, 0, len(resp.Entries))
	for _, entry := range resp.Entries {
		rows = append(rows, ui.AccountRow{
			Agent:            entry.Agent,
			Name:             entry.Name,
			LoggedIn:         entry.LoggedIn,
			RegistrationOnly: entry.RegistrationOnly,
		})
	}
	m.configPane.SetAccounts(rows, resp.Agents, nil)
}

// handleAccountRequest performs whatever the Accounts section asked for. It
// returns nil when there was nothing to do, so the caller can fall through to
// its own routing.
func (m *home) handleAccountRequest(req ui.AccountRequest) tea.Cmd {
	switch req.Kind {
	case ui.AccountRequestRegister:
		return m.handleAccountRegister(req.Agent, req.Name)
	case ui.AccountRequestLogin:
		return m.handleAccountLogin(req.Agent, req.Name)
	default:
		return nil
	}
}

// handleAccountRegister creates an account's directory and refreshes the
// section in place. The overlay stays open: registering is a step on the way to
// logging in, and closing the surface the user is working in would make them
// reopen it to do the next thing.
//
// It runs INLINE rather than off the event loop, unlike the login below. A
// register is one mkdir plus a stat on the daemon host — there is no readiness
// wait and no process to start — so the round trip is milliseconds, and an async
// hop would buy a spinner nobody sees at the cost of a message type.
func (m *home) handleAccountRegister(agent, name string) tea.Cmd {
	resp, err := registerAccount(daemon.RegisterAccountRequest{Agent: agent, Name: name})
	if err != nil {
		// The daemon's own refusal, verbatim: it holds the one name rule
		// (agentaccount.ValidateName), the case-collision rule and the roster, and
		// each of those messages already says what to do about it.
		m.configPane.SetAccountStatus(err.Error(), true)
		return nil
	}
	m.loadAccountsIntoPane()
	status := fmt.Sprintf("Registered %s account %q · %s", resp.Entry.Agent, resp.Entry.Name, resp.Entry.Dir)
	// The preconditions ride along on the same line: this is the moment the
	// operator is about to put a real credential somewhere, which is when what the
	// agent's variable relocates has to be said (#3384).
	for _, notice := range resp.Notices {
		status += " · " + notice
	}
	m.configPane.SetAccountStatus(status, false)
	return nil
}

// accountLoginStartedMsg reports the outcome of an async login spawn.
type accountLoginStartedMsg struct {
	err   error
	agent string
	name  string
	// sessionName is the bare tmux session the daemon started, empty when the
	// flow finished before af could hand a terminal over.
	sessionName string
	// socketPath pins the tmux socket for the attach (#2019); empty falls back to
	// the default socket.
	socketPath string
	// finished is true when the agent's login command ran to completion before af
	// could hand over the terminal — there is nothing to attach to, and the
	// outcome is already known.
	finished bool
	loggedIn bool
	notices  []string
	// noticeID identifies the "Starting…" notice this spawn raised, so the
	// handler retracts its OWN notice rather than whatever is on screen by then.
	noticeID uint64
}

// accountLoginDoneMsg reports that the user has left the login takeover.
type accountLoginDoneMsg struct {
	agent string
	name  string
	err   error
}

// handleAccountLogin opens the agent's own login flow for one account.
//
// Async, like handleConfigAgent and for the same reason: the spawn is a daemon
// round trip that registers a directory and starts a process, and running it
// inline would freeze the TUI for its duration.
func (m *home) handleAccountLogin(agent, name string) tea.Cmd {
	noticeID := m.setTransientNotice(fmt.Errorf("Starting the %s login for %q…", agent, name))
	login := startAccountLogin
	return func() tea.Msg {
		resp, err := login(daemon.AccountLoginRequest{Agent: agent, Name: name})
		return accountLoginStartedMsg{
			err:         err,
			agent:       agent,
			name:        name,
			sessionName: resp.SessionName,
			socketPath:  resp.SocketPath,
			finished:    resp.Finished,
			loggedIn:    resp.LoggedIn,
			notices:     resp.Notices,
			noticeID:    noticeID,
		}
	}
}

// handleAccountLoginStarted finalizes the spawn: hand the terminal over, or
// report why there is nothing to hand it to.
func (m *home) handleAccountLoginStarted(msg accountLoginStartedMsg) (tea.Model, tea.Cmd) {
	// Retract the "Starting…" notice, but ONLY if it is still ours: the spawn ran
	// async, so another action may have posted its own by now and clearing
	// unconditionally would wipe a message the user has not read. Same generation
	// token the config-agent spawn uses.
	if msg.noticeID == m.transientNoticeID {
		m.errBox.Clear()
	}
	if msg.err != nil {
		log.ErrorLog.Printf("accounts: could not start the %s login for %q: %v", msg.agent, msg.name, msg.err)
		return m, m.handleError(msg.err)
	}
	// The flow answered itself — `codex login` against an account that already
	// holds a credential prints and exits. There is no pane; report the outcome.
	if msg.finished || msg.sessionName == "" {
		return m.reopenConfigWithAccountStatus(accountLoginFinishedStatus(msg), !msg.loggedIn)
	}
	return m, m.enterAccountLogin(msg.agent, msg.name, msg.sessionName, msg.socketPath)
}

// accountLoginFinishedStatus is what the pane says about a flow that ended
// before the handover.
func accountLoginFinishedStatus(msg accountLoginStartedMsg) string {
	if !msg.loggedIn {
		return fmt.Sprintf(
			"The %s login for %q ended without leaving a credential, so the account is registered but not "+
				"logged in. Try again, or run `af accounts login %s %s` on the daemon host.",
			msg.agent, msg.name, msg.agent, msg.name)
	}
	status := fmt.Sprintf("Account %q is logged in for %s.", msg.name, msg.agent)
	for _, notice := range msg.notices {
		status += " " + notice
	}
	return status
}

// enterAccountLogin hands the terminal to the login pane and takes it back when
// the flow ends or the user detaches.
//
// tea.ExecProcess is bubbletea's own primitive for this: it pauses the Program,
// releases the terminal, runs the child, and resumes — which is why this needs
// neither the WS PTY stream nor a hand-restore of the terminal modes.
func (m *home) enterAccountLogin(agent, name, sessionName, socketPath string) tea.Cmd {
	cmd := execAccountLoginAttach(sessionName, socketPath)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return accountLoginDoneMsg{agent: agent, name: name, err: err}
	})
}

// execAccountLoginAttach builds the attach command, through the shared builder
// that scrubs $TMUX and pins the socket (see bareTmuxAttachCommand). A var so a
// test can drive the takeover without a tmux server.
var execAccountLoginAttach = func(sessionName, socketPath string) *exec.Cmd {
	return bareTmuxAttachCommand(sessionName, socketPath)
}

// handleAccountLoginDone reports the outcome once the user is back.
//
// The pane is NOT reaped here. A login the operator detached from is still
// waiting for its human — af cannot tell a detach from a completed flow at this
// seam — and killing it would throw away a half-finished OAuth step. The pane
// ends when the agent's own command exits, and the daemon reaps whatever is left
// at shutdown, which is the same contract the CLI verb keeps.
func (m *home) handleAccountLoginDone(msg accountLoginDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		log.ErrorLog.Printf("accounts: the %s login takeover for %q ended with an error: %v",
			msg.agent, msg.name, msg.err)
		return m, m.handleError(fmt.Errorf(
			"the %s login for %q could not take the terminal: %w", msg.agent, msg.name, msg.err))
	}
	// Reopen the overlay onto FRESH state rather than a remembered row: the
	// account's logged-in state is the whole question the user just went to
	// answer, and re-reading is how the section reports the agent's own verdict
	// instead of af's memory of it.
	return m.reopenConfigWithAccountStatus(
		fmt.Sprintf("Back from the %s login for %q.", msg.agent, msg.name), false)
}

// reopenConfigWithAccountStatus reopens the config overlay with re-read config
// and accounts, carrying one status line into it.
//
// A failure to reopen is surfaced rather than swallowed — the same rule
// showConfigEditor keeps — but it leaves the user at the default view rather
// than in a half-opened overlay.
func (m *home) reopenConfigWithAccountStatus(status string, isError bool) (tea.Model, tea.Cmd) {
	model, cmd := m.showConfigEditor()
	if !m.configPane.HasFocus() {
		// showConfigEditor refused (a config that will not load, a remote daemon
		// that will not answer) and has already raised its own error. Do not
		// overwrite that with an account status the user cannot see anyway.
		return model, cmd
	}
	m.configPane.SetAccountStatus(status, isError)
	if isError {
		log.WarningLog.Printf("accounts: %s", status)
	}
	return model, cmd
}
