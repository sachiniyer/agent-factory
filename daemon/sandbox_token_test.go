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
	// The scope was EMPTY throughout #3012 — no route could be admitted while the
	// credential was unbound — and #3056 admits exactly one, Snapshot, because its
	// handler now narrows to the caller's own session. Admission alone is still not
	// authorization: TestEverySandboxRouteEnforcesAnOwnerConstraint is what holds
	// the two together, and this assertion only pins which routes are in the table.
	assert.True(t, sandboxAllowedPath("/v1/Snapshot"),
		"Snapshot is admitted under #3056 because its handler constrains to sandboxOwner(ctx); if it is being "+
			"withdrawn, remove its entry from sandboxConstrainedRoutes too")
	assert.Len(t, sandboxAllowedPaths, 1,
		"the scope grew without this test noticing: every entry needs its own handler-side owner constraint, so a "+
			"new one is a deliberate decision, never a default")
	for _, path := range []string{
		// DeliverPrompt runs instructions through every agent on the machine.
		"/v1/DeliverPrompt",
		// CreateSession names NO session and reaches the same authority anyway, by
		// creating the target itself: repo_path, backend, program and prompt are
		// all caller-supplied, so the default/local backend starts an attacker's
		// agent on the HOST (#3012 review). This one is why the rule is "cannot
		// gain host authority" rather than "cannot name a session".
		"/v1/CreateSession",
		// These three NAME a session, so admitting them without a per-route owner
		// constraint reproduces DeliverPrompt one agent at a time: SendPrompt and
		// CreateTab take a session id directly, and AddTask reaches one through
		// Task.TargetSession. Snapshot used to be listed here as the enumeration
		// that made them aimable; #3056 admitted it precisely because its handler
		// now enforces the constraint these three still lack. Give them one and
		// they can join it — that is the whole point of the contract.
		"/v1/SendPrompt",
		"/v1/CreateTab",
		"/v1/AddTask",
		// Read-only, and still host reconnaissance: ListProjects returns the
		// operator's absolute project roots and ListDirectory walks the daemon's
		// filesystem at an arbitrary path. The Add-project picker that consumes
		// ListDirectory (#2788) is the operator's own client on the operator's own
		// token, so denying it here costs nothing that works today.
		"/v1/ListProjects",
		"/v1/ListDirectory",
		// The same oracle, quieter: both take a caller-supplied repo_path and
		// answer through config.RepoFromPath, which wraps git's own stderr — so a
		// guessed path comes back distinguishable as missing, permission-denied,
		// or not-a-repo. Confirming a path is weaker than enumerating one; it is
		// still the operator's layout, and admitting it would be an exception
		// carved into the rule for convenience.
		"/v1/ListBackends",
		"/v1/ListPrograms",
		// Parameterless, and still an oracle: it avoids live titles across ALL
		// repos, and the sandbox holds the finite wordlist in the af binary af put
		// there, so sampling reveals which combinations are persistently taken.
		"/v1/SuggestSessionName",
		"/v1/SetConfigValue",
		"/v1/UnsetConfigValue",
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

	// The shipped table admits NOTHING, so the gate's admit path is exercised
	// against a temporary entry. Without this the positive branch would be dead and
	// the revocation assertion below would pass for the wrong reason — every route
	// is refused anyway.
	sandboxAllowedPaths["/v1/TestOnlyAdmitted"] = true
	t.Cleanup(func() { delete(sandboxAllowedPaths, "/v1/TestOnlyAdmitted") })

	assert.True(t, admits(gate, request("/v1/TestOnlyAdmitted", secret)),
		"a sandbox credential must authorize a route its scope admits")
	assert.False(t, admits(gate, request("/v1/SuggestSessionName", secret)),
		"and must not authorize the route withdrawn as an activity oracle")
	assert.False(t, admits(gate, request("/v1/DeliverPrompt", secret)),
		"a sandbox credential must NOT reach DeliverPrompt")
	assert.False(t, admits(gate, request("/v1/SendPrompt", secret)),
		"nor SendPrompt, which names a session and so reaches the same authority one agent at a time")
	assert.False(t, admits(gate, request("/v1/CreateSession", secret)),
		"nor CreateSession, which names no session and starts one on the host anyway")
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

func TestMintSandboxCallback_RefusesWithoutRequireToken(t *testing.T) {
	m := &Manager{}

	_, err := m.mintSandboxCallback(daemonTestConfig(false, "10.0.0.5:8443"), "sess-a")
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
	_, err = m.mintSandboxCallback(daemonTestConfig(true, ""), "sess-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen_addr")

	// A BIND address is not a dialable one, and every way it can fail that test is
	// refused (#3012 review). From inside a sandbox a wildcard names the SANDBOX;
	// so does loopback, even more definitively — and nothing forwards a callback
	// the other way, since the ssh runtime tunnels daemon→sandbox only. Port 0
	// names nothing fixed at all.
	// "10.0.0.5:" is the OTHER spelling of a kernel-selected port: SplitHostPort
	// returns port "" with no error, so a check against the literal "0" alone let it
	// through and produced "http://10.0.0.5:" (#3012 review).
	// Every case here is a SPELLING the previous version of this check missed, and
	// they are listed rather than reduced because each one was a separate review
	// finding: an unspecified address and a zero port each have an open-ended set of
	// textual forms, and matching literals recognised whichever ones I happened to
	// think of. The code now parses instead — net.IP.IsUnspecified and
	// net.LookupPort — so this table's job is to keep proving that.
	for _, addr := range []string{
		// Unspecified, four ways. net.Listen binds all of them identically.
		"0.0.0.0:8443", ":8443", "[::]:8443", "[0:0:0:0:0:0:0:0]:8443", "[::ffff:0.0.0.0]:8443",
		// Port zero, four ways. All resolve to 0 and mean "kernel picks".
		"10.0.0.5:0", "10.0.0.5:", "10.0.0.5:00", "10.0.0.5:000",
		// Loopback, three ways — the third is a NAME, and names are the reason the
		// host must now be a literal IP at all.
		"127.0.0.1:8443", "[::1]:8443", "localhost:8443",
		// Loopback aliases: unbounded as a set (any /etc/hosts entry), which is why
		// the rule is "literal IP" rather than a list of names to reject.
		"ip6-localhost:8443", "localhost.localdomain:8443",
		// A hostname is resolved by the SANDBOX, not by this daemon, so af cannot
		// know what it points at over there — refused even when it looks routable.
		"af-host.internal:8443",
		// A zone identifier names an interface on the DAEMON's host, and its raw "%"
		// is an invalid escape to Go's URL parser. net.ParseIP refuses it, so the
		// literal-IP rule covers this too.
		"[fe80::1234%eth0]:8443",
	} {
		_, err = m.mintSandboxCallback(daemonTestConfig(true, addr), "sess-a")
		require.Errorf(t, err, "listen_addr %q is not dialable from a sandbox", addr)
		assert.Emptyf(t, m.sandboxTokens.bySession, "a refused mint must leave no credential behind (%s)", addr)
	}

	// Loopback must be refused for being UNDIALABLE, not for the posture: telling
	// an operator to set require_loopback_token would send them to a key that buys
	// a well-enforced credential their sandbox still cannot use.
	_, err = m.mintSandboxCallback(daemonTestConfig(true, "127.0.0.1:8443"), "sess-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reaches ITSELF")
	assert.NotContains(t, err.Error(), "require_loopback_token")

	grant, err := m.mintSandboxCallback(daemonTestConfig(true, "10.0.0.5:8443"), "sess-a")
	require.NoError(t, err)
	assert.Equal(t, "http://10.0.0.5:8443", grant.URL)

	// A service-name port resolves into the URL as a NUMBER, because an HTTP client
	// inside the sandbox dials a port, not an /etc/services entry.
	namedGrant, nerr := m.mintSandboxCallback(daemonTestConfig(true, "10.0.0.5:http"), "sess-b")
	require.NoError(t, nerr)
	assert.Equal(t, "http://10.0.0.5:80", namedGrant.URL)
	assert.NotEmpty(t, grant.Token)
	owner, ok := m.sandboxTokens.sessionFor(grant.Token)
	require.True(t, ok)
	assert.Equal(t, "sess-a", owner)
}

// daemonTestConfig is the minimal config the refusal reads.
func daemonTestConfig(requireToken bool, listenAddr string) *config.Config {
	return &config.Config{RequireToken: requireToken, ListenAddr: listenAddr}
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
