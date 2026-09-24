package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The daemon RegisterAccount route is the web/TUI "add account" form. Its own
// doc says it creates the directory WITHOUT logging in, so it must enforce
// notice-only registration messaging — never the login-time refusal that
// CheckLoginPreconditions applies to a keyring-backed codex account. The CLI
// `af accounts add` path already calls RegistrationNotices and has
// TestAccountsAddKeyringStillSucceeds codifying the contract; these tests pin
// the same contract on the daemon route that #3889 missed when it split
// RegistrationNotices out of CheckLoginPreconditions.

// registerHome gives a freshly-built Manager an isolated, socket-safe
// AGENT_FACTORY_HOME and a sandboxed HOME so no agent store on the host leaks
// in (and no ambient ~/.codex/config.toml seeds the registered account).
func registerHome(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	t.Setenv("HOME", t.TempDir())
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	return m
}

// TestRegisterAccountKeyringAccountSucceeds is the regression: re-registering a
// keyring-configured codex account through the daemon route must succeed like
// the CLI `af accounts add` does, while login still refuses.
func TestRegisterAccountKeyringAccountSucceeds(t *testing.T) {
	m := registerHome(t)

	resp, err := m.RegisterAccount(RegisterAccountRequest{Agent: "codex", Name: "work"})
	require.NoError(t, err, "first RegisterAccount")
	require.Equal(t, "codex", resp.Entry.Agent)
	require.Equal(t, "work", resp.Entry.Name)
	require.NotEmpty(t, resp.Entry.Dir)

	configPath := filepath.Join(resp.Entry.Dir, "config.toml")
	const keyringConfig = "cli_auth_credentials_store = 'keyring'\n"
	require.NoError(t, os.WriteFile(configPath, []byte(keyringConfig), 0o600))

	// Re-register: before the fix this returned the keyring refusal; it now
	// succeeds and reports the same directory, matching the CLI add path.
	resp2, err := m.RegisterAccount(RegisterAccountRequest{Agent: "codex", Name: "work"})
	require.NoError(t, err, "re-register should succeed for a keyring account like the CLI add path does")
	require.Equal(t, resp.Entry.Dir, resp2.Entry.Dir, "re-registering an existing account reports the same directory")

	// The keyring hand-edit is left exactly as the operator wrote it.
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, keyringConfig, string(data), "Register must not rewrite an operator's per-account config.toml")

	// The route reports registration notices, not a refusal: the codex
	// relocation notice and the settings-seeding notice the CLI prints too.
	require.NotEmpty(t, resp2.Notices, "RegisterAccount should report registration notices for a keyring account")
	joined := strings.Join(resp2.Notices, "\n")
	require.Contains(t, joined, "CODEX_HOME", "the codex relocation notice must reach the UI")
	require.Contains(t, joined, "Registration seeds missing", "the codex settings notice must reach the UI")
	require.NotContains(t, joined, "machine-wide keyring identity", "a registration must not surface the login-time refusal")

	// The login route's refusal is unchanged: a keyring account still cannot
	// be logged in, which is the half of the contract RegisterAccount relies on.
	_, loginErr := agentaccount.CheckLoginPreconditions("codex", resp2.Entry.Dir)
	require.ErrorContains(t, loginErr, "machine-wide keyring identity",
		"login must still refuse a keyring-backed codex account whose identity the agent would ignore")
}

// TestRegisterAccountRouteThroughControlServerSucceedsForKeyring exercises the
// actual net/rpc seam the web/TUI hits (controlServer.RegisterAccount), not just
// the Manager method, so the regression cannot hide behind the delegating wrapper.
func TestRegisterAccountRouteThroughControlServerSucceedsForKeyring(t *testing.T) {
	m := registerHome(t)
	cs := &controlServer{manager: m}

	var resp RegisterAccountResponse
	require.NoError(t, cs.RegisterAccount(RegisterAccountRequest{Agent: "codex", Name: "work"}, &resp), "first register via the RPC seam")
	require.NoError(t, os.WriteFile(
		filepath.Join(resp.Entry.Dir, "config.toml"),
		[]byte("cli_auth_credentials_store = 'keyring'\n"), 0o600))

	var resp2 RegisterAccountResponse
	require.NoError(t, cs.RegisterAccount(RegisterAccountRequest{Agent: "codex", Name: "work"}, &resp2),
		"re-register via the RPC seam should succeed for a keyring account")
	require.Equal(t, resp.Entry.Dir, resp2.Entry.Dir)
	require.NotEmpty(t, resp2.Notices)
}

// TestRegisterAccountReportsRegistrationNoticesForEachAgent pins that the swap
// from CheckLoginPreconditions to RegistrationNotices did not drop the
// per-agent registration notices for the ordinary (non-keyring) case, since the
// UI's setAccountRegisteredStatus displays them. Every supported login agent is
// covered; the notices must match agentaccount.RegistrationNotices verbatim.
func TestRegisterAccountReportsRegistrationNoticesForEachAgent(t *testing.T) {
	for _, agent := range agentaccount.LoginAgents() {
		t.Run(agent, func(t *testing.T) {
			m := registerHome(t)

			resp, err := m.RegisterAccount(RegisterAccountRequest{Agent: agent, Name: "work"})
			require.NoError(t, err, "first RegisterAccount for %s", agent)

			want := agentaccount.RegistrationNotices(agent, resp.Entry.Dir)
			require.Equal(t, want, resp.Notices,
				"RegisterAccount.Notices must be exactly agentaccount.RegistrationNotices, the helper the CLI add path uses")
			require.NotEmpty(t, resp.Notices, "every supported login agent has at least one registration notice")
		})
	}
}
