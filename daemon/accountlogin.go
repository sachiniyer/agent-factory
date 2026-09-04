package daemon

import (
	"context"
	"fmt"
	"sort"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/accountlogin"
)

// The daemon half of `af accounts login` (#3384).
//
// The daemon runs the login pane for the same reason it runs config agents: a
// bare tmux session nobody owns is an orphan, and the daemon is the process with
// a lifetime long enough to own one. It is also what makes the verb reachable
// from every surface — the CLI, the TUI's config tab and the web (#3385) all ask
// for the same session and attach to it, rather than each reimplementing a flow.
//
// Everything about WHAT is run lives in session/accountlogin. This file is
// request plumbing: resolve the home and the operator's pass-through list, and
// hand the rest over.

// AccountLogin registers the account if needed and opens the agent's own login
// flow in a bare tmux session scoped to it, returning what a caller needs to
// attach to it and to report the outcome.
//
// It does NOT wait for the flow. The whole point is an interactive OAuth or
// device-code step that only a human can finish, so the RPC returns as soon as
// the pane is running and the terminal is somebody else's to take.
func (m *Manager) AccountLogin(ctx context.Context, req AccountLoginRequest) (AccountLoginResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return AccountLoginResponse{}, fmt.Errorf("account login: cannot resolve the agent-factory home: %w", err)
	}
	// The LIVE config, not the frozen startup one: session_env_passthrough is
	// hot-reloadable, and an operator who has just added DISPLAY to it so their
	// browser can open should not have to restart the daemon before the next
	// login sees it (#2480).
	passthrough := m.Config().SessionEnvPassthrough
	login, err := m.accountLogins.Start(ctx, accountlogin.Request{
		Home:        home,
		Agent:       req.Agent,
		Name:        req.Name,
		Passthrough: passthrough,
	})
	if err != nil {
		return AccountLoginResponse{}, err
	}
	return AccountLoginResponse{
		Agent:       login.Agent,
		Name:        login.Name,
		Dir:         login.Dir,
		Program:     login.Program,
		SessionName: login.TmuxName,
		SocketPath:  login.SocketPath,
		Reused:      login.Reused,
		Finished:    login.Finished,
		LoggedIn:    login.LoggedIn,
		Notices:     login.Notices,
	}, nil
}

// ListAccounts reports the registered accounts on this host with their
// logged-in state, and the roster an account can be created in.
//
// The UIs read this rather than the filesystem, which is the point of it
// existing: the accounts live in the DAEMON's agent-factory home, and a web
// client is usually not on that machine at all. `af accounts list` still reads
// the home directly — it refuses a remote daemon for the same reason — so the
// two answer from the same package and the same AccountEntry shape.
//
// A logged-in probe is a stat per account, so listing is cheap enough to be the
// UI's refresh. It reads no credential.
func (m *Manager) ListAccounts(req ListAccountsRequest) (ListAccountsResponse, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return ListAccountsResponse{}, fmt.Errorf("accounts: cannot resolve the agent-factory home: %w", err)
	}
	agents := sessionenv.AccountAgents()
	if req.Agent != "" {
		if _, ok := sessionenv.SupportsAccounts(req.Agent); !ok {
			return ListAccountsResponse{}, fmt.Errorf("%w: %s (supported: %s)",
				agentaccount.ErrUnsupportedAgent, req.Agent, sessionenv.AccountAgentsSummary())
		}
		agents = []string{req.Agent}
	}
	// Entries is a non-nil empty slice, never nil: it crosses the wire as JSON,
	// and `null` where a client expects a list is a different value from `[]` in
	// every language reading it.
	entries := []AccountEntry{}
	for _, agent := range agents {
		names, err := agentaccount.List(home, agent)
		if err != nil {
			return ListAccountsResponse{}, err
		}
		registrationOnly := sessionenv.AccountRegistrationOnly(agent)
		for _, name := range names {
			entry, err := accountEntryFor(home, agent, name, registrationOnly)
			if err != nil {
				return ListAccountsResponse{}, err
			}
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Agent != entries[j].Agent {
			return entries[i].Agent < entries[j].Agent
		}
		return entries[i].Name < entries[j].Name
	})
	// The FULL roster, not the filtered one: a client renders its register form
	// from this, and narrowing it to the agent that was queried would make the
	// form offer one agent because the list happened to be filtered.
	return ListAccountsResponse{Entries: entries, Agents: sessionenv.AccountAgents()}, nil
}

// RegisterAccount creates an account's credential directory without logging in.
//
// It is the other half of the UI's story. `af accounts login` registers on the
// way past, which is right for a CLI where the two are one gesture, but a form
// that lists accounts needs to be able to ADD one and leave it empty — the
// operator may be creating it now and signing in on the machine with the browser
// later.
func (m *Manager) RegisterAccount(req RegisterAccountRequest) (RegisterAccountResponse, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return RegisterAccountResponse{}, fmt.Errorf("accounts: cannot resolve the agent-factory home: %w", err)
	}
	dir, err := agentaccount.Register(home, req.Agent, req.Name)
	if err != nil {
		return RegisterAccountResponse{}, err
	}
	notices, err := agentaccount.CheckLoginPreconditions(req.Agent, dir)
	if err != nil {
		return RegisterAccountResponse{}, err
	}
	entry, err := accountEntryFor(home, req.Agent, req.Name, sessionenv.AccountRegistrationOnly(req.Agent))
	if err != nil {
		return RegisterAccountResponse{}, err
	}
	return RegisterAccountResponse{Entry: entry, Notices: notices}, nil
}

// accountEntryFor builds one entry, resolving the directory and probing the
// artifact through the same helpers every other surface uses.
func accountEntryFor(home, agent, name string, registrationOnly bool) (AccountEntry, error) {
	dir, err := agentaccount.Dir(home, agent, name)
	if err != nil {
		return AccountEntry{}, err
	}
	loggedIn, err := agentaccount.LoggedIn(home, agent, name)
	if err != nil {
		return AccountEntry{}, err
	}
	return AccountEntry{
		Agent: agent, Name: name, Dir: dir,
		RegistrationOnly: registrationOnly, LoggedIn: loggedIn,
	}, nil
}
