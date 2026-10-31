package daemon

import (
	"net/http"
	"testing"

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
	// The exact scope table is pinned by TestSandboxScopeTableIsPinned
	// (sandbox_scope_test.go, W3.5); this keeps the by-name denials of the verbs
	// whose authority a sandbox must never hold, whatever else joins the scope.
	assert.True(t, sandboxAllowedPath("/v1/Snapshot"),
		"Snapshot is admitted because its handler constrains to the caller's project; if it is being "+
			"withdrawn, remove its entry from sandboxConstrainedRoutes too")
	for _, path := range []string{
		// DeliverPrompt runs instructions through every agent on the machine.
		"/v1/DeliverPrompt",
		// Read-only, and still host reconnaissance: ListProjects returns the
		// operator's absolute project roots and ListDirectory walks the daemon's
		// filesystem at an arbitrary path.
		"/v1/ListProjects",
		"/v1/ListDirectory",
		// The same oracle, quieter: both take a caller-supplied repo_path and
		// answer through config.RepoFromPath, which wraps git's own stderr.
		"/v1/ListBackends",
		"/v1/ListPrograms",
		// Parameterless, and still an oracle across ALL repos' live titles.
		"/v1/SuggestSessionName",
		"/v1/SetConfigValue",
		"/v1/UnsetConfigValue",
		"/v1/DeleteProject",
		"/v1/KillSession",
		// W3.5: a task patch, trigger or restart can turn a prompt-delivery task
		// into one that runs on the daemon host.
		"/v1/UpdateTask",
		"/v1/TriggerTask",
		"/v1/RestartTask",
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

	// A route the table admits is exercised here through a temporary entry too,
	// so the revocation assertion below stays on a route that is admitted for
	// this test alone, not on one whose scope may change.
	sandboxAllowedPaths["/v1/TestOnlyAdmitted"] = true
	t.Cleanup(func() { delete(sandboxAllowedPaths, "/v1/TestOnlyAdmitted") })

	assert.True(t, admits(gate, request("/v1/TestOnlyAdmitted", secret)),
		"a sandbox credential must authorize a route its scope admits")
	assert.False(t, admits(gate, request("/v1/SuggestSessionName", secret)),
		"and must not authorize the route withdrawn as an activity oracle")
	assert.False(t, admits(gate, request("/v1/DeliverPrompt", secret)),
		"a sandbox credential must NOT reach DeliverPrompt")
	// W3.5 admits SendPrompt and CreateSession at the gate; the handlers bound
	// them to the owner's project (daemon/sandbox_scope.go), pinned by
	// sandbox_scope_test.go.
	assert.True(t, admits(gate, request("/v1/SendPrompt", secret)),
		"SendPrompt is admitted at the gate and scoped to the project by its handler")
	assert.True(t, admits(gate, request("/v1/CreateSession", secret)),
		"CreateSession is admitted at the gate and bounded to a listed host by its handler")
	assert.False(t, admits(gate, request("/v1/ListBackends", secret)),
		"nor ListBackends, whose caller-supplied repo_path is a host path oracle")

	// The operator's own token is unaffected by any of this.
	assert.True(t, admits(gate, request("/v1/DeliverPrompt", "operator-token")))

	// And revocation reaches the gate, not just the registry. Asserted against the
	// one ADMITTED route: checking a denied route here would pass whether or not
	// revocation did anything.
	registry.revoke("sess-a")
	assert.False(t, admits(gate, request("/v1/TestOnlyAdmitted", secret)),
		"a revoked credential must stop authorizing at the gate")

	// A gate with no registry (the agent-server, the preview origin) accepts no
	// sandbox credential at all.
	bare := &authGate{expectedToken: func() (string, error) { return "operator-token", nil }}
	assert.False(t, admits(bare, request("/v1/TestOnlyAdmitted", secret)))
}

// The mint no longer reads network.listen_addr or network.require_token
// (#5571 W3.4): the callback rides the supervised connection's -R to the
// dedicated sandbox endpoint, so its only precondition is that the endpoint is
// listening. A default-config daemon (loopback listener, token not required) must
// therefore mint.
func TestMintSandboxCallbackFenced_NeedsOnlyTheEndpoint(t *testing.T) {
	m := &Manager{}

	// Endpoint down: refused BEFORE minting, leaving nothing in the registry.
	_, err := m.mintSandboxCallbackFenced(m.sandboxTokens.invalidationCount(), "sess-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox callback endpoint is not running")
	assert.Empty(t, m.sandboxTokens.bySession, "a refused mint must leave no credential behind")
	assert.NotEmpty(t, m.callbackUnavailableReason(), "the picker must say what the create would")

	m.setSandboxEndpointSocket("/tmp/af-home/sandbox-callback.sock")
	assert.Empty(t, m.callbackUnavailableReason())

	grant, err := m.mintSandboxCallbackFenced(m.sandboxTokens.invalidationCount(), "sess-a")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/af-home/sandbox-callback.sock", grant.Socket)
	assert.NotEmpty(t, grant.Token)
	owner, ok := m.sandboxTokens.sessionFor(grant.Token)
	require.True(t, ok)
	assert.Equal(t, "sess-a", owner)

	// Clearing the endpoint (daemon shutdown) refuses again.
	m.setSandboxEndpointSocket("")
	_, err = m.mintSandboxCallbackFenced(m.sandboxTokens.invalidationCount(), "sess-b")
	require.Error(t, err)

	// A fence that moved during the window voids the mint.
	m.setSandboxEndpointSocket("/tmp/af-home/sandbox-callback.sock")
	stale := m.sandboxTokens.invalidationCount()
	m.sandboxTokens.revokeAll()
	_, err = m.mintSandboxCallbackFenced(stale, "sess-c")
	require.Error(t, err)
	assert.Empty(t, m.sandboxTokens.bySession["sess-c"])
}

// A credential does not outlive the listener it was minted against (#3012 review).
//
// The callback URL is written into the sandbox's environment file at provision
// time and nothing rewrites it, so a live listen_addr change leaves every running
// sandbox calling a closed address. Leaving the token valid buys nothing — the
// sandbox cannot reach the daemon to use it — and hides the fact that the
// capability is gone.
func TestSandboxTokenRegistry_RevokeAllDropsEveryCredential(t *testing.T) {
	var registry sandboxTokenRegistry
	a, err := registry.mint("sess-a")
	require.NoError(t, err)
	b, err := registry.mint("sess-b")
	require.NoError(t, err)

	assert.Equal(t, 2, registry.revokeAll(), "the count is what the rebind log reports; a wrong one misreports the blast radius")
	for _, secret := range []string{a, b} {
		_, ok := registry.sessionFor(secret)
		assert.False(t, ok, "a credential minted against the previous listener must not survive the rebind")
	}

	// Idempotent, and the registry must still be usable afterwards — the daemon
	// keeps running and the next provision mints against the NEW listener.
	assert.Equal(t, 0, registry.revokeAll())
	fresh, err := registry.mint("sess-c")
	require.NoError(t, err)
	owner, ok := registry.sessionFor(fresh)
	require.True(t, ok, "revokeAll must clear the registry, not break it")
	assert.Equal(t, "sess-c", owner)
}

// admits reports only the gate's yes/no, for the tests that do not care which
// principal was admitted. Tests that DO care call authorize directly and assert
// the owner — see TestAuthGate_BindsTheCredentialToItsOwningSession.
func admits(g *authGate, r *http.Request) bool {
	_, ok := g.authorize(r)
	return ok
}
