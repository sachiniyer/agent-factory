package daemon

import (
	"context"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
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
