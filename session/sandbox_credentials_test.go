package session

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/unixurl"
)

// The credential subscribes to the RUNTIME's lifetime (#3068): provisioning a
// sandbox mints, reaping one revokes, and both happen in one place each.

type fakeCreds struct {
	mints    int
	revokes  int
	mintErr  error
	invalid  error
	lastCred SandboxCredential
}

func (f *fakeCreds) Mint() (SandboxCredential, error) {
	f.mints++
	if f.mintErr != nil {
		return SandboxCredential{}, f.mintErr
	}
	f.lastCred = SandboxCredential{
		URL:        "http://10.0.0.5:8443",
		Token:      "secret",
		Socket:     "/af/sandbox-callback.sock",
		Revalidate: func() error { return f.invalid },
	}
	return f.lastCred, nil
}

func (f *fakeCreds) Revoke() { f.revokes++ }

func TestMintForProvision_OnlyForKindsThatInjectACallback(t *testing.T) {
	creds := &fakeCreds{}

	// A local kind must not mint: minting unconditionally would make every local
	// create inherit a sandbox-only refusal.
	var spec ProvisionSpec
	cred := mintForProvision(&spec, BackendLocal, creds)
	assert.False(t, cred.Granted())
	assert.Empty(t, cred.UnavailableReason)
	assert.Equal(t, 0, creds.mints)
	assert.Empty(t, spec.CallbackToken)

	// An injecting kind mints and populates the spec.
	cred = mintForProvision(&spec, BackendSSH, creds)
	assert.True(t, cred.Granted())
	assert.Empty(t, cred.UnavailableReason)
	assert.Equal(t, 1, creds.mints)
	assert.Equal(t, "http://10.0.0.5:8443", spec.CallbackURL)
	assert.Equal(t, "secret", spec.CallbackToken)
	assert.Equal(t, "/af/sandbox-callback.sock", spec.CallbackSocket, "the carrier rides the spec with the credential")

	// No minter at all (a local session, a test, the in-sandbox agent-server) is a
	// no-op rather than an error — but the ABSENCE is still named, so a kind that
	// would take a credential cannot lose one silently (#5571 W3.1).
	var bare ProvisionSpec
	cred = mintForProvision(&bare, BackendSSH, nil)
	assert.False(t, cred.Granted())
	assert.NotEmpty(t, cred.UnavailableReason, "a kind that injects a callback must explain not getting one")
	assert.Empty(t, bare.CallbackToken)
}

// A refusal must DEGRADE the provision, not fail it: the sandbox still
// provisions — a callback-less session is more useful than none — but the
// refusal text is carried out loud so the surfaces can say why the agent cannot
// call af (#5571 W3.1). The mint refuses exactly when a credential could not be
// enforced or could not be dialled.
func TestMintForProvision_DegradesARefusal(t *testing.T) {
	refusal := errors.New("refusing to give this sandbox a callback credential: require_token is false")
	creds := &fakeCreds{mintErr: refusal}
	var spec ProvisionSpec
	cred := mintForProvision(&spec, BackendSSH, creds)
	assert.False(t, cred.Granted())
	assert.Equal(t, refusal.Error(), cred.UnavailableReason,
		"the degraded reason must be the refusal's own text — surfaces render it verbatim")
	assert.Empty(t, spec.CallbackToken, "a refused mint must leave the spec without a credential")
	assert.Empty(t, spec.CallbackURL)
}

// failingRuntime is a Runtime whose Provision always errors — the shape behind
// "the sandbox never came up" for the credential-lifetime test below.
type failingRuntime struct{ err error }

func (r failingRuntime) Provision(ProvisionSpec) (ProvisionResult, error) {
	return ProvisionResult{}, r.err
}

// A minted credential must not outlive a sandbox that never came up: when
// Provision fails the grant dies with it — the same fail-closed rule the
// replacement path's replacementLive defer enforces (#5571 W3.1).
func TestDefaultBackendFactory_ProvisionFailureRevokesTheMint(t *testing.T) {
	rt := failingRuntime{err: errors.New("provision blew up")}
	restoreRuntime := SetRuntimeForTest(BackendSandbox, func() Runtime { return rt })
	defer restoreRuntime()

	creds := &fakeCreds{}
	_, err := defaultBackendFactoryForKind(InstanceOptions{
		Title:              "s",
		Program:            "claude",
		SandboxCredentials: creds,
	}, t.TempDir(), BackendSandbox)

	require.Error(t, err)
	assert.Equal(t, 1, creds.mints, "the mint ran before provisioning")
	assert.Equal(t, 1, creds.revokes,
		"the grant must die with the sandbox that never existed — not linger against a session that will not be created")
}

func TestRevalidateAfterProvision_RunsOnlyWhenSomethingWasGranted(t *testing.T) {
	assert.NoError(t, revalidateAfterProvision(SandboxCredential{}),
		"nothing was granted, so there is nothing to invalidate")

	invalidated := errors.New("invalidated")
	assert.ErrorIs(t, revalidateAfterProvision(SandboxCredential{
		Token: "secret", Revalidate: func() error { return invalidated },
	}), invalidated)

	assert.NoError(t, revalidateAfterProvision(SandboxCredential{
		Token: "secret", Revalidate: func() error { return nil },
	}))
}

// THE lifetime rule: a conclusively reaped runtime takes its credential with it,
// in ONE place, so kill/archive/restore/recover do not each have to remember.
//
// This is the half that stops the worse failure mode — a token minted for one
// runtime surviving into its replacement.
func TestResetRemoteRuntime_RevokesTheCredential(t *testing.T) {
	creds := &fakeCreds{}
	i := &Instance{Title: "worker"}
	i.SetSandboxCredentials(creds)

	i.resetRemoteRuntime()
	assert.Equal(t, 1, creds.revokes, "reaping a runtime must revoke the credential minted for it")

	// Idempotent: every teardown path reaches this and none should fail for it.
	i.resetRemoteRuntime()
	assert.Equal(t, 2, creds.revokes)

	// And an instance with no minter must not panic — that is every local session.
	bare := &Instance{Title: "local"}
	assert.NotPanics(t, func() { bare.resetRemoteRuntime() })
}

// A KNOWN teardown failure still means the runtime is gone, so the credential
// goes with it — while the runtime WIRING is deliberately left for the retry
// path (#3065 review).
//
// remoteAgentServer.Kill tears the in-sandbox workspace down over REST and then
// reaps the sandbox, and returns the REST error even when the reap succeeded. The
// success path's revocation is therefore not reachable on that branch, and
// leaving the credential live means a copied token keeps authenticating until
// some later recovery happens to replace it.
func TestRevokeSandboxCredential_IsSeparableFromResettingTheRuntime(t *testing.T) {
	creds := &fakeCreds{}
	i := &Instance{Title: "worker"}
	i.SetSandboxCredentials(creds)

	// Revoking alone must not disturb the wiring the retry path still needs.
	i.runtimeTeardown = func() error { return nil }
	i.revokeSandboxCredential()
	assert.Equal(t, 1, creds.revokes)
	assert.NotNil(t, i.runtimeTeardown, "settling the credential must not clear the wiring the abort-to-Lost retry owns")

	// And the full reset still revokes, so the two agree on the common path.
	i.resetRemoteRuntime()
	assert.Equal(t, 2, creds.revokes)
	assert.Nil(t, i.runtimeTeardown)

	bare := &Instance{Title: "local"}
	assert.NotPanics(t, func() { bare.revokeSandboxCredential() })
}

// The mid-provision recheck (#5571 W3.1, PR #5577 review): a grant that dies
// inside the clone/binary window must be caught BEFORE the env file commits —
// after launch the server's own process env is fixed and a daemon-side degrade
// would leave the inner agent being taught af verbs that all fail.
func TestRevalidateCallbackBeforeEnv_ClearsThePairOnLoss(t *testing.T) {
	lost := errors.New("the listener moved while the sandbox was provisioning")
	w := &sandboxWorkspace{spec: ProvisionSpec{
		CallbackURL:   "http://10.0.0.5:8443",
		CallbackToken: "secret",
		CallbackRevalidate: func() error {
			return lost
		},
	}}
	assert.Equal(t, lost.Error(), w.revalidateCallbackBeforeEnv())
	assert.Empty(t, w.spec.CallbackToken, "a lost grant must not be committed to the host")
	assert.Empty(t, w.spec.CallbackURL)
}

func TestRevalidateCallbackBeforeEnv_KeepsALiveGrant(t *testing.T) {
	checked := false
	w := &sandboxWorkspace{spec: ProvisionSpec{
		CallbackURL:   "http://10.0.0.5:8443",
		CallbackToken: "secret",
		CallbackRevalidate: func() error {
			checked = true
			return nil
		},
	}}
	assert.Empty(t, w.revalidateCallbackBeforeEnv())
	assert.True(t, checked)
	assert.Equal(t, "secret", w.spec.CallbackToken)
}

// A session with nothing granted, or nothing able to check it, must not even
// reach for the check — and an unset token must skip it entirely.
func TestRevalidateCallbackBeforeEnv_SkipsNothingToLose(t *testing.T) {
	var noHook sandboxWorkspace
	assert.Empty(t, noHook.revalidateCallbackBeforeEnv())

	called := false
	noToken := &sandboxWorkspace{spec: ProvisionSpec{
		CallbackRevalidate: func() error { called = true; return errors.New("lost") },
	}}
	assert.Empty(t, noToken.revalidateCallbackBeforeEnv(),
		"no token means nothing was minted — do not spend a check on it")
	assert.False(t, called)

	withToken := &sandboxWorkspace{spec: ProvisionSpec{CallbackToken: "secret"}}
	assert.Empty(t, withToken.revalidateCallbackBeforeEnv(),
		"a credential with no revalidate hook is left alone")
}

// mintForProvision must hand the runtime the SAME check the post-provision
// revalidation runs — a spec carrying a credential but no way to re-check it
// would resurrect the drift this field exists to close.
func TestMintForProvision_WiresTheRevalidateHook(t *testing.T) {
	creds := &fakeCreds{}
	var spec ProvisionSpec
	cred := mintForProvision(&spec, BackendSSH, creds)
	require.True(t, cred.Granted())
	require.NotNil(t, spec.CallbackRevalidate, "the runtime must be able to re-check before committing the env")

	// The wired hook is the credential's own revalidation.
	creds.invalid = errors.New("posture changed")
	assert.EqualError(t, spec.CallbackRevalidate(), "posture changed")

	// A refused mint grants nothing and wires nothing.
	creds2 := &fakeCreds{mintErr: errors.New("no listener")}
	var spec2 ProvisionSpec
	mintForProvision(&spec2, BackendSSH, creds2)
	assert.Nil(t, spec2.CallbackRevalidate)
}

// The -R spec is built only from af's own values: a unix socket inside the
// session's 0700 directory, carried to the daemon's endpoint socket. No TCP port
// exists on the sandbox host for another uid to bind (#5799). Anything unusable
// clears the grant and says why, so the agent is never handed a URL that nothing
// answers.
func TestCallbackReverseForward(t *testing.T) {
	const sessionDir = "/home/u/.af-sessions/s.AbC123"
	granted := func(dir, sock string) *sandboxWorkspace {
		return &sandboxWorkspace{SessionDir: dir, spec: ProvisionSpec{CallbackToken: "secret", CallbackSocket: sock}}
	}

	w := granted(sessionDir, "/home/d/.agent-factory/sandbox-callback.sock")
	fwd, lost := w.callbackReverseForward()
	assert.Equal(t, sessionDir+"/af-callback.sock:/home/d/.agent-factory/sandbox-callback.sock", fwd)
	assert.Empty(t, lost)
	assert.Equal(t, "unix://"+sessionDir+"/af-callback.sock", w.spec.CallbackURL,
		"the in-sandbox client dials the session-dir socket, not a TCP address")
	assert.NotContains(t, fwd, "127.0.0.1", "no TCP bind address anywhere in the carrier")
	assert.True(t, strings.HasPrefix(w.CallbackSocketPath(), sessionDir+"/"),
		"the socket must live inside the session's own directory, which mktemp -d makes 0700")
	assert.Equal(t, "secret", w.spec.CallbackToken)

	// No credential: no forward and no reason (a local-style spec).
	fwd, lost = (&sandboxWorkspace{}).callbackReverseForward()
	assert.Empty(t, fwd)
	assert.Empty(t, lost)

	for name, c := range map[string]struct{ dir, sock string }{
		"no session dir":                {"", "/af/cb.sock"},
		"relative session dir":          {"rel/dir", "/af/cb.sock"},
		"colon in session dir":          {"/h/a:b", "/af/cb.sock"},
		"path too long":                 {"/" + strings.Repeat("d", 120), "/af/cb.sock"},
		"no daemon socket":              {sessionDir, ""},
		"relative daemon sock":          {sessionDir, "cb.sock"},
		"colon in daemon sock":          {sessionDir, "/af/a:b/cb.sock"},
		"doubled slash in session dir":  {"/srv/work//s.AbC123", "/af/cb.sock"},
		"dot component in session dir":  {"/srv/work/./s.AbC123", "/af/cb.sock"},
		"trailing slash on session dir": {"/srv/work/s.AbC123/", "/af/cb.sock"},
	} {
		w := granted(c.dir, c.sock)
		fwd, lost := w.callbackReverseForward()
		assert.Emptyf(t, fwd, "%s: no forward may be built", name)
		assert.NotEmptyf(t, lost, "%s: the loss must be reported", name)
		assert.Emptyf(t, w.spec.CallbackToken, "%s: a grant with no carrier must not be committed to the host", name)
		assert.Emptyf(t, w.spec.CallbackURL, "%s: and no URL either", name)
	}
}

// A re-dial removes the stale carrier first so it binds the SAME path (the
// in-sandbox URL never changes), and only that session's own socket.
func TestClearStaleCallbackSocket(t *testing.T) {
	p := &sandboxProvisioner{}
	var ran []string
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		ran = append(ran, script)
		return nil, nil
	}
	p.clearStaleCallbackSocket()
	assert.Empty(t, ran, "no carrier, nothing to clear")
	p.callbackSocket = "/home/u/.af-sessions/s.X/af-callback.sock"
	p.clearStaleCallbackSocket()
	assert.Equal(t, []string{"rm -f -- '/home/u/.af-sessions/s.X/af-callback.sock'"}, ran)
}

// The URL the in-sandbox client receives names EXACTLY the socket the carrier
// forwards, for a session directory containing characters that are special in a
// URL (#5870). Before, "#", "?" and "%" silently truncated or rewrote the path
// while the ssh -R forward targeted the correct socket.
func TestCallbackURLRoundTripsTheSocketPath(t *testing.T) {
	for _, dir := range []string{
		"/home/u/af#work/s.AbC123",
		"/home/u/af?x/s.AbC123",
		"/home/u/100%/s.AbC123",
		"/home/ユーザー/s.AbC123",
	} {
		w := &sandboxWorkspace{SessionDir: dir, spec: ProvisionSpec{CallbackToken: "secret", CallbackSocket: "/home/d/cb.sock"}}
		fwd, lost := w.callbackReverseForward()
		require.Empty(t, lost, dir)
		require.NotEmpty(t, fwd, dir)
		got, ok, err := unixurl.Parse(w.spec.CallbackURL)
		require.NoError(t, err, dir)
		require.True(t, ok)
		assert.Equal(t, w.CallbackSocketPath(), got, "the client dials the socket the provisioner created")
	}
}

// The provisioner guard and unixurl.Parse must refuse the SAME non-clean socket
// paths (#5870). Build accepts any path but Parse rejects anything
// filepath.Clean(path) != path, so before the guard mirrored that check an
// unclean spelling reached mktemp via an operator workspace_root, the guard let
// it through, Build persisted it into AF_DAEMON_URL, and every in-sandbox daemon
// callback then failed at parse time for the session's lifetime. The guard now
// refuses the unclean form (degrading to callback-unavailable at provision
// time) while the clean spelling of the same session still round-trips.
func TestCallbackReverseForward_RefusesNonCleanSocketPathThatParseRefuses(t *testing.T) {
	const daemonSock = "/home/d/.agent-factory/sandbox-callback.sock"
	cases := []struct{ name, uncleanDir, cleanDir string }{
		{"doubled slash", "/srv/work//s.AbC123", "/srv/work/s.AbC123"},
		{"dot component", "/srv/work/./s.AbC123", "/srv/work/s.AbC123"},
		{"trailing slash", "/srv/work/s.AbC123/", "/srv/work/s.AbC123"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &sandboxWorkspace{SessionDir: c.uncleanDir, spec: ProvisionSpec{CallbackToken: "secret", CallbackSocket: daemonSock}}

			_, ok, err := unixurl.Parse(unixurl.Build(w.CallbackSocketPath()))
			require.True(t, ok, "a unix URL is recognised; what fails is the clean-path check")
			require.Error(t, err, "the unclean socket path survives Build but Parse refuses it — the asymmetry the guard must close")

			fwd, lost := w.callbackReverseForward()
			assert.Empty(t, fwd, "no forward may be built from a socket path Parse would refuse")
			assert.NotEmpty(t, lost, "an unclean socket path must degrade to callback-unavailable, not be persisted")
			assert.Empty(t, w.spec.CallbackURL, "the unclean URL must not be written into AF_DAEMON_URL")
			assert.Empty(t, w.spec.CallbackToken, "a grant with no usable carrier must be cleared before the env file")

			clean := &sandboxWorkspace{SessionDir: c.cleanDir, spec: ProvisionSpec{CallbackToken: "secret", CallbackSocket: daemonSock}}
			fwd, lost = clean.callbackReverseForward()
			require.Empty(t, lost, "the cleaned spelling of the same session is usable")
			require.NotEmpty(t, fwd)
			got, ok, err := unixurl.Parse(clean.spec.CallbackURL)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, clean.CallbackSocketPath(), got, "the clean session's URL round-trips to its socket path — no regression")
		})
	}
}
