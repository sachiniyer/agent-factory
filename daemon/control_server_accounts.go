package daemon

// The accounts control plane (#3384/#3385). Split out of control_server.go to
// sit alongside its request/response types (control_types_accounts.go) so the
// three handlers that act on the registered-account roster read as one surface.
// The config-agent pair (SpawnConfigAgent/ReapConfigAgent) is a different
// surface — its types stayed in control_types.go — so it stays put too.

import "context"

// ListAccounts reports the registered accounts on this host with their
// logged-in state. A read, so no mutation admission gate.
func (s *controlServer) ListAccounts(req ListAccountsRequest, resp *ListAccountsResponse) error {
	out, err := s.manager.ListAccounts(req)
	if err != nil {
		return err
	}
	*resp = out
	return nil
}

// RegisterAccount creates an account's credential directory without logging in.
// It writes to the daemon host's agent-factory home, so it is behind the same
// mutation admission gate as every other write.
func (s *controlServer) RegisterAccount(req RegisterAccountRequest, resp *RegisterAccountResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.RegisterAccount(req)
	if err != nil {
		return err
	}
	*resp = out
	return nil
}

// AccountLogin opens an agent's own login flow in a bare tmux session scoped to
// one account, and returns what a client needs to attach to it.
//
// No event is published: a login pane is not a session, so nothing on the events
// plane models it — the same reason SpawnConfigAgent publishes none.
//
// It is behind requireStateMutationAdmission because it MUTATES the host: it
// registers an account directory and spawns a process. It is deliberately NOT
// sandboxAllowed for the same reason CreateSession is not — starting a process
// on the daemon host is the plainest form of the host authority that credential
// withholds (see httproutes.go's rule).
func (s *controlServer) AccountLogin(req AccountLoginRequest, resp *AccountLoginResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	// net/rpc gives no per-call context; the spawn is bounded by tmux's own start
	// timeout, and the interactive flow it opens is unbounded by design.
	out, err := s.manager.AccountLogin(context.Background(), req)
	if err != nil {
		return err
	}
	*resp = out
	return nil
}
