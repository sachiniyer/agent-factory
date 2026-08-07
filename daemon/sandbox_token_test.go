package daemon

import (
	"net/http"
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sandbox callback credential's two non-negotiables (#2999): a sandbox never
// holds the operator's authority, and af refuses to hand out a credential for a
// listener that does not ask for one.

func TestSandboxTokenRegistry_MintLookupRevoke(t *testing.T) {
	var registry sandboxTokenRegistry

	secret, err := registry.mint("sess-a")
	require.NoError(t, err)
	require.NotEmpty(t, secret)

	owner, ok := registry.sessionFor(secret)
	require.True(t, ok)
	assert.Equal(t, "sess-a", owner)

	// Revocation is what makes this per-session rather than a second master key.
	registry.revoke("sess-a")
	_, ok = registry.sessionFor(secret)
	assert.False(t, ok, "a revoked credential must stop authenticating immediately")

	// Idempotent: every teardown path calls this and none should fail for it.
	registry.revoke("sess-a")
	registry.revoke("never-existed")

	// An empty presented credential must never match, or a caller sending no
	// token at all would authenticate as whatever the zero value looked up.
	_, ok = registry.sessionFor("")
	assert.False(t, ok)
}

func TestSandboxTokenRegistry_ReMintRevokesThePrevious(t *testing.T) {
	var registry sandboxTokenRegistry

	first, err := registry.mint("sess-a")
	require.NoError(t, err)
	second, err := registry.mint("sess-a")
	require.NoError(t, err)
	require.NotEqual(t, first, second, "each mint must be a fresh secret")

	_, ok := registry.sessionFor(first)
	assert.False(t, ok, "re-provisioning a session must invalidate its old credential, not leave two live")
	_, ok = registry.sessionFor(second)
	assert.True(t, ok)
}

func TestSandboxTokenRegistry_SessionsDoNotShareCredentials(t *testing.T) {
	var registry sandboxTokenRegistry
	a, err := registry.mint("sess-a")
	require.NoError(t, err)
	b, err := registry.mint("sess-b")
	require.NoError(t, err)
	require.NotEqual(t, a, b)

	// Revoking one must not disturb the other: teardown is per session.
	registry.revoke("sess-a")
	_, ok := registry.sessionFor(b)
	assert.True(t, ok, "one session's teardown must not revoke another's credential")
}

// The scope is what stops a compromised sandbox owning the host, so the denials
// are asserted by name rather than by counting.
func TestSandboxAllowedPath_DeniesTheOperatorOnlyVerbs(t *testing.T) {
	// What the scope IS: capability discovery — what this daemon could offer.
	// None of these names a session, starts anything, or reveals a host path.
	for _, path := range []string{"/v1/ListBackends", "/v1/ListPrograms", "/v1/SuggestSessionName"} {
		assert.Truef(t, sandboxAllowedPath(path), "%s only describes what the daemon offers", path)
	}
	for _, path := range []string{
		// DeliverPrompt runs instructions through every agent on the machine.
		"/v1/DeliverPrompt",
		// CreateSession names NO session and reaches the same authority anyway, by
		// creating the target itself: repo_path, backend, program and prompt are
		// all caller-supplied, so the default/local backend starts an attacker's
		// agent on the HOST (#3012 review). This one is why the rule is "cannot
		// gain host authority" rather than "cannot name a session".
		"/v1/CreateSession",
		// These four NAME a session, so admitting them without binding the
		// credential to its owner reproduces DeliverPrompt one agent at a time:
		// SendPrompt and CreateTab take a session id directly, AddTask reaches one
		// through Task.TargetSession, and Snapshot is the enumeration that makes
		// any of them aimable.
		"/v1/SendPrompt",
		"/v1/CreateTab",
		"/v1/AddTask",
		"/v1/Snapshot",
		// Read-only, and still host reconnaissance: ListProjects returns the
		// operator's absolute project roots and ListDirectory walks the daemon's
		// filesystem at an arbitrary path. The Add-project picker that consumes
		// ListDirectory (#2788) is the operator's own client on the operator's own
		// token, so denying it here costs nothing that works today.
		"/v1/ListProjects",
		"/v1/ListDirectory",
		"/v1/SetConfigValue",
		"/v1/DeleteProject",
		"/v1/KillSession",
	} {
		assert.Falsef(t, sandboxAllowedPath(path), "%s must never be reachable with a sandbox credential", path)
	}
	// Default deny: a path that is not in the table at all — including the WS
	// planes and anything added later without opting in — is refused.
	assert.False(t, sandboxAllowedPath("/v1/events"))
	assert.False(t, sandboxAllowedPath("/v1/NotARoute"))
}

func TestAuthGate_SandboxCredentialIsScopedAndRevocable(t *testing.T) {
	var registry sandboxTokenRegistry
	secret, err := registry.mint("sess-a")
	require.NoError(t, err)

	gate := &authGate{
		expectedToken: func() (string, error) { return "operator-token", nil },
		sandboxTokens: &registry,
	}
	request := func(path, token string) *http.Request {
		r := mustRequest(t, http.MethodPost, path)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}

	assert.True(t, gate.authorize(request("/v1/ListBackends", secret)),
		"a sandbox credential must work for a route its scope admits")
	assert.False(t, gate.authorize(request("/v1/DeliverPrompt", secret)),
		"a sandbox credential must NOT reach DeliverPrompt")
	assert.False(t, gate.authorize(request("/v1/SendPrompt", secret)),
		"nor SendPrompt, which names a session and so reaches the same authority one agent at a time")
	assert.False(t, gate.authorize(request("/v1/CreateSession", secret)),
		"nor CreateSession, which names no session and starts one on the host anyway")

	// The operator's own token is unaffected by any of this.
	assert.True(t, gate.authorize(request("/v1/DeliverPrompt", "operator-token")))

	// And revocation reaches the gate, not just the registry.
	registry.revoke("sess-a")
	assert.False(t, gate.authorize(request("/v1/CreateSession", secret)),
		"a revoked credential must stop authorizing at the gate")

	// A gate with no registry (the agent-server, the preview origin) accepts no
	// sandbox credential at all.
	bare := &authGate{expectedToken: func() (string, error) { return "operator-token", nil }}
	assert.False(t, bare.authorize(request("/v1/ListBackends", secret)))
}

func TestMintSandboxCallback_RefusesWithoutRequireToken(t *testing.T) {
	m := &Manager{}

	_, _, err := m.mintSandboxCallback(daemonTestConfig(false, "10.0.0.5:8443"), "sess-a")
	require.Error(t, err, "a credential against a listener that demands none enforces nothing")
	assert.Contains(t, err.Error(), requireTokenFixHint,
		"the refusal must name the one-line fix, or the operator is left guessing")

	// Refused BEFORE minting: nothing may be left in the registry for a provision
	// that did not happen.
	_, ok := m.sandboxTokens.sessionFor("")
	assert.False(t, ok)
	assert.Empty(t, m.sandboxTokens.bySession, "a refused mint must leave no credential behind")

	// An empty listen_addr has nothing to call back to, and is refused separately
	// so the message can say which of the two is wrong.
	_, _, err = m.mintSandboxCallback(daemonTestConfig(true, ""), "sess-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen_addr")

	// A loopback listener with require_loopback_token off is exempted by the gate
	// BEFORE any token is examined, so a credential there enforces nothing.
	loopback := daemonTestConfig(true, "127.0.0.1:8443")
	_, _, err = m.mintSandboxCallback(loopback, "sess-a")
	require.Error(t, err, "the gate exempts loopback, so the scope would not be enforced")
	assert.Contains(t, err.Error(), requireLoopbackTokenFixHint)

	// A BIND address is not a dialable one: from inside a sandbox a wildcard names
	// the sandbox, and port 0 names nothing fixed at all.
	for _, addr := range []string{"0.0.0.0:8443", ":8443", "10.0.0.5:0"} {
		_, _, err = m.mintSandboxCallback(daemonTestConfig(true, addr), "sess-a")
		require.Errorf(t, err, "listen_addr %q is not dialable from a sandbox", addr)
	}

	url, token, err := m.mintSandboxCallback(daemonTestConfig(true, "10.0.0.5:8443"), "sess-a")
	require.NoError(t, err)
	assert.Equal(t, "http://10.0.0.5:8443", url)
	assert.NotEmpty(t, token)
	owner, ok := m.sandboxTokens.sessionFor(token)
	require.True(t, ok)
	assert.Equal(t, "sess-a", owner)
}

// daemonTestConfig is the minimal config the refusal reads.
func daemonTestConfig(requireToken bool, listenAddr string) *config.Config {
	return &config.Config{RequireToken: requireToken, ListenAddr: listenAddr}
}
