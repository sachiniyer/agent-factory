package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pushcred"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// Push credentials on listed hosts (#5571 W7.8), the daemon half: every rule the
// approved plan names, driven through the REAL dedicated sandbox endpoint gate
// (markSandboxEndpoint + sandboxOnlyGate) and, for the refusals that depend on
// it, through the main-listener gate the scope fixture uses.

// testPushToken is the daemon box's GitHub token in these tests. Distinctive, so
// a scan of a body or a log for it is meaningful.
const testPushToken = "gho_W78canaryTOKEN0000000000000000000"

type pushFixture struct {
	*scopeFixture
	endpoint   http.Handler
	placements map[string]session.PushPlacement // by title
	origins    map[string]pushcred.Origin       // by title
	tokenReads atomic.Int32
	// onTokenRead runs inside the token read, so a test can change the world
	// between the two authorizations.
	onTokenRead func()
	tokenErr    error
}

var ownOrigin = pushcred.Origin{Host: "github.com", Path: "owner/repo"}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	f := newScopeFixture(t)
	pf := &pushFixture{
		scopeFixture: f,
		placements:   map[string]session.PushPlacement{"me": {ListedHost: "box1"}},
		origins:      map[string]pushcred.Origin{"me": ownOrigin},
	}
	prevPlacement, prevOrigin := instancePushPlacement, instancePushOrigin
	instancePushPlacement = func(i *session.Instance) (session.PushPlacement, bool) {
		p, ok := pf.placements[i.Title]
		return p, ok
	}
	instancePushOrigin = func(i *session.Instance) (pushcred.Origin, bool) {
		o, ok := pf.origins[i.Title]
		return o, ok
	}
	t.Cleanup(func() { instancePushPlacement, instancePushOrigin = prevPlacement, prevOrigin })

	f.mgr.pushCreds.tokenFor = func(context.Context, pushcred.Origin) (string, error) {
		pf.tokenReads.Add(1)
		if pf.onTokenRead != nil {
			pf.onTokenRead()
		}
		if pf.tokenErr != nil {
			return "", pf.tokenErr
		}
		return testPushToken, nil
	}
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example", PushCredentials: true}}
	})
	pf.endpoint = markSandboxEndpoint(withAuth(newHTTPMux(&controlServer{manager: f.mgr}), sandboxOnlyGate(&f.mgr.sandboxTokens), nil))
	return pf
}

// setConfig publishes a modified copy of the live config, as an applied
// `af config set` would.
func (pf *pushFixture) setConfig(edit func(*config.Config)) {
	cfg := *pf.mgr.Config()
	edit(&cfg)
	pf.mgr.storeLivePosture(&cfg)
}

// ask calls GitCredential on the dedicated endpoint with token and the git
// request fields, returning the status, the decoded data and the error message.
func (pf *pushFixture) ask(t *testing.T, token, protocol, host, path string) (int, GitCredentialResponse, string) {
	t.Helper()
	body, err := json.Marshal(GitCredentialRequest{Protocol: protocol, Host: host, Path: path})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/GitCredential", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	pf.endpoint.ServeHTTP(rec, req)
	var env struct {
		Data  *GitCredentialResponse `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	raw := rec.Body.String()
	require.NoErrorf(t, json.Unmarshal([]byte(raw), &env), "body %q", raw)
	if env.Data == nil {
		require.NotContains(t, raw, testPushToken, "a refusal must never carry the token")
	}
	var data GitCredentialResponse
	if env.Data != nil {
		data = *env.Data
	}
	msg := ""
	if env.Error != nil {
		msg = env.Error.Message
	}
	return rec.Code, data, msg
}

func (pf *pushFixture) askOwn(t *testing.T) (int, GitCredentialResponse, string) {
	t.Helper()
	return pf.ask(t, pf.secret, "https", "github.com", "owner/repo.git")
}

func requireAnswered(t *testing.T, code int, got GitCredentialResponse, msg string) {
	t.Helper()
	require.Equalf(t, http.StatusOK, code, "refused: %s", msg)
	assert.Equal(t, pushCredentialUsername, got.Username)
	assert.Equal(t, testPushToken, got.Password, "the token comes back verbatim through the response boundary")
}

func requireRefused(t *testing.T, code int, got GitCredentialResponse, msg, want string) {
	t.Helper()
	assert.NotEqual(t, http.StatusOK, code)
	assert.Empty(t, got.Password)
	assert.Containsf(t, msg, want, "refusal text (status %d)", code)
}

func TestGitCredential_AnswersTheOwnOriginOverTheEndpoint(t *testing.T) {
	pf := newPushFixture(t)
	for _, path := range []string{"owner/repo.git", "owner/repo", "Owner/Repo.git", "/owner/repo.git/", "owner/repo.git/info/lfs"} {
		code, got, msg := pf.ask(t, pf.secret, "https", "github.com", path)
		requireAnswered(t, code, got, msg)
	}
}

// The main web listener admits sandbox credentials to the scope too, so without
// this a leaked af-callback.env would fetch the operator's GitHub token over the
// network. The refusal comes before the token is read.
func TestGitCredential_RefusedOnTheMainListener(t *testing.T) {
	pf := newPushFixture(t)
	code, out := pf.call(t, pf.secret, "/v1/GitCredential", `{"protocol":"https","host":"github.com","path":"owner/repo.git"}`)
	assert.NotEqual(t, http.StatusOK, code)
	assert.Contains(t, out, refusePushOverMainListener)
	assert.NotContains(t, out, testPushToken)
	assert.Zero(t, pf.tokenReads.Load(), "the token must not even be read for a main-listener request")

	// Anti-vacuous: the same credential and body are answered on the endpoint.
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg)
}

// The operator's own authority gets nothing: on the main listener the handler
// refuses it, and the dedicated endpoint has no operator credential at all.
func TestGitCredential_RefusesTheOperator(t *testing.T) {
	pf := newPushFixture(t)
	code, out := pf.call(t, "operator-token", "/v1/GitCredential", `{"protocol":"https","host":"github.com","path":"owner/repo.git"}`)
	assert.NotEqual(t, http.StatusOK, code)
	assert.Contains(t, out, errGitCredentialOperator.Error())
	assert.NotContains(t, out, testPushToken)

	code, got, _ := pf.ask(t, "operator-token", "https", "github.com", "owner/repo.git")
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Empty(t, got.Password)
	assert.Zero(t, pf.tokenReads.Load())
}

func TestGitCredential_OnlyTheSessionsOwnOrigin(t *testing.T) {
	pf := newPushFixture(t)
	for name, req := range map[string][3]string{
		"another repo":  {"https", "github.com", "owner/other.git"},
		"another owner": {"https", "github.com", "attacker/repo.git"},
		"another host":  {"https", "github.example", "owner/repo.git"},
		"plain http":    {"http", "github.com", "owner/repo.git"},
		"no path":       {"https", "github.com", ""},
		"traversal":     {"https", "github.com", "owner/repo.git/../../attacker/x"},
		"a layout path": {"https", "github.com", pf.me.Path},
		"api host":      {"https", "api.github.com", "owner/repo.git"},
		"another port":  {"https", "github.com:8443", "owner/repo.git"},
	} {
		t.Run(name, func(t *testing.T) {
			code, got, msg := pf.ask(t, pf.secret, req[0], req[1], req[2])
			requireRefused(t, code, got, msg, refusePushOtherRepo)
		})
	}
	assert.Zero(t, pf.tokenReads.Load(), "a request for anything but the own origin must not read the token")

	// No https GitHub origin on the daemon checkout: nothing to answer for.
	delete(pf.origins, "me")
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNoOrigin)
}

// A second session — another project, on the same listed host — is answered for
// its OWN origin with its own credential, and never for the first session's.
func TestGitCredential_ASecondSessionNeverGetsTheFirstsOrigin(t *testing.T) {
	pf := newPushFixture(t)
	pf.placements["foreign"] = session.PushPlacement{ListedHost: "box1"}
	pf.origins["foreign"] = pushcred.Origin{Host: "github.com", Path: "owner/other"}
	other, err := pf.mgr.sandboxTokens.mint(pf.other.ID)
	require.NoError(t, err)

	code, got, msg := pf.ask(t, other, "https", "github.com", "owner/repo.git")
	requireRefused(t, code, got, msg, refusePushOtherRepo)
	assert.Zero(t, pf.tokenReads.Load())

	code, got, msg = pf.ask(t, other, "https", "github.com", "owner/other.git")
	requireAnswered(t, code, got, msg)
}

func TestGitCredential_LicenceIsTheOperatorsAndReadLive(t *testing.T) {
	pf := newPushFixture(t)
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg)

	// Off on the host: the next request is refused, with no restart.
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example"}}
	})
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)

	// Back on; then the host leaves the list.
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example", PushCredentials: true}}
	})
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)
	pf.setConfig(func(c *config.Config) { c.Hosts = nil })
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)

	// Another host's opt-in is not this one's.
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example"}, "box2": {Host: "box2.example", PushCredentials: true}}
	})
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)

	// A repository's own [ssh] target records no listed host: never licensed.
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example", PushCredentials: true}}
	})
	delete(pf.placements, "me")
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)

	// The sandbox.ssh switch does not license an ssh row, nor a listed host a
	// free-form one: the placement must agree with the backend.
	pf.placements["me"] = session.PushPlacement{FreeFormSandbox: true}
	pf.setConfig(func(c *config.Config) { c.SandboxPushCredentials = true })
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)
}

func TestGitCredential_FreeFormSandboxNeedsItsOwnOptIn(t *testing.T) {
	pf := newPushFixture(t)
	pf.backends["me"] = "sandbox"
	pf.placements["me"] = session.PushPlacement{FreeFormSandbox: true}

	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)

	pf.setConfig(func(c *config.Config) { c.SandboxPushCredentials = true })
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)

	// A listed host's opt-in does not reach a free-form placement.
	pf.setConfig(func(c *config.Config) { c.SandboxPushCredentials = false })
	pf.placements["me"] = session.PushPlacement{ListedHost: "box1"}
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)
}

func TestGitCredential_RefusedForSessionsThatAreNotLive(t *testing.T) {
	for name, end := range map[string]func(pf *pushFixture){
		"archived": func(pf *pushFixture) { pf.me.SetStatusForTest(session.Archived) },
		"lost":     func(pf *pushFixture) { pf.me.SetStatusForTest(session.Lost) },
		"deleting": func(pf *pushFixture) { pf.me.SetStatusForTest(session.Deleting) },
		"local":    func(pf *pushFixture) { pf.backends["me"] = "local" },
		"docker":   func(pf *pushFixture) { pf.backends["me"] = "docker" },
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPushFixture(t)
			end(pf)
			code, got, msg := pf.askOwn(t)
			assert.NotEqual(t, http.StatusOK, code)
			assert.Empty(t, got.Password)
			assert.True(t, strings.Contains(msg, refusePushNotLive) || strings.Contains(msg, refusePushNotLicensed), "refusal: %q", msg)
			assert.Zero(t, pf.tokenReads.Load())
		})
	}
}

// Every way a session ends revokes its callback credential; after that the
// credential authorizes nothing, this route included.
func TestGitCredential_GoneOnceTheCallbackIsRevoked(t *testing.T) {
	pf := newPushFixture(t)
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg)

	pf.mgr.sandboxTokens.revoke(pf.me.ID)
	code, got, _ = pf.askOwn(t)
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Empty(t, got.Password)
}

// The token read can take seconds. A session archived, revoked or fenced while
// it runs is not answered with the token its own end was meant to stop.
func TestGitCredential_ReauthorizesAfterTheTokenRead(t *testing.T) {
	for name, change := range map[string]func(pf *pushFixture){
		"revoked":      func(pf *pushFixture) { pf.mgr.sandboxTokens.revoke(pf.me.ID) },
		"archived":     func(pf *pushFixture) { pf.me.SetStatusForTest(session.Archived) },
		"licence off":  func(pf *pushFixture) { pf.setConfig(func(c *config.Config) { c.Hosts = nil }) },
		"auto push":    func(pf *pushFixture) { pf.mgr.pushCreds.beginAutoPush(pf.me.ID) },
		"origin moved": func(pf *pushFixture) { pf.origins["me"] = pushcred.Origin{Host: "github.com", Path: "owner/elsewhere"} },
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPushFixture(t)
			pf.onTokenRead = func() { change(pf) }
			code, got, msg := pf.askOwn(t)
			assert.NotEqualf(t, http.StatusOK, code, "answered after %s mid-read", name)
			assert.Empty(t, got.Password)
			assert.NotEmpty(t, msg)
			assert.Equal(t, int32(1), pf.tokenReads.Load(), "anti-vacuous: the first authorization passed and the token was read")
		})
	}
}

// W7.8 changes no af-initiated push (the owner's answer to the plan's Q4): while
// the daemon runs an automatic preserve-push for a row, that row gets nothing.
func TestGitCredential_WithheldDuringAnAutomaticPreservePush(t *testing.T) {
	pf := newPushFixture(t)
	release := pf.mgr.pushCreds.beginAutoPush(pf.me.ID)
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushAutoPreserve)

	// Another session is unaffected by this row's fence.
	pf.placements["foreign"] = session.PushPlacement{ListedHost: "box1"}
	pf.origins["foreign"] = pushcred.Origin{Host: "github.com", Path: "owner/other"}
	other, err := pf.mgr.sandboxTokens.mint(pf.other.ID)
	require.NoError(t, err)
	code, got, msg = pf.ask(t, other, "https", "github.com", "owner/other.git")
	requireAnswered(t, code, got, msg)

	release()
	release() // idempotent
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)
}

// preserveSandboxBeforeReap holds the fence across the in-sandbox archive (where
// the push runs) and beyond its return, since that work outlives the daemon's
// wait.
func TestPreserveSandboxBeforeReapFencesPushCredentials(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	var inst *session.Instance
	var fencedDuringArchive atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/agent/archive" {
			fencedDuringArchive.Store(manager.pushCreds.autoPushInFlight(inst.ID))
			writeSandboxProbeEnvelope(w, map[string]any{"branch": "af/fenced"})
			return
		}
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	inst, _ = registerStartedRemote(t, manager, repoID, repoPath, "fenced", srv.URL, session.Ready)

	require.False(t, manager.pushCreds.autoPushInFlight(inst.ID), "precondition: no fence")
	_ = manager.preserveSandboxBeforeReap(repoID, daemonInstanceKey(repoID, "fenced"), inst, "")
	assert.True(t, fencedDuringArchive.Load(), "the push ran while push credentials were still answerable for its row")
	assert.True(t, manager.pushCreds.autoPushInFlight(inst.ID), "the fence must outlive the call: the in-sandbox archive takes no context")
}

func TestGitCredential_NoLoginOnTheDaemonBoxIsNamed(t *testing.T) {
	pf := newPushFixture(t)
	pf.tokenErr = pushcred.ErrNoLogin
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNoLogin)
}

// Any other token-read failure reaches the sandbox only as the boundary's fixed
// text; the real error (which could name daemon paths) goes to the daemon log.
func TestGitCredential_OtherReadFailuresAreWithheld(t *testing.T) {
	pf := newPushFixture(t)
	pf.tokenErr = assert.AnError
	code, got, msg := pf.askOwn(t)
	assert.NotEqual(t, http.StatusOK, code)
	assert.Empty(t, got.Password)
	assert.Equal(t, sandboxGenericFailureMsg, msg)
}

// The audit line names the session and the origin, never the token, and no
// refusal path logs it either.
func TestGitCredential_TheTokenIsNeverLogged(t *testing.T) {
	info, warn, errs := captureInfo(t), captureWarnings(t), captureErrors(t)
	pf := newPushFixture(t)
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg)
	_, _, _ = pf.ask(t, pf.secret, "https", "github.com", "owner/other.git")
	_, _ = pf.call(t, pf.secret, "/v1/GitCredential", `{"protocol":"https","host":"github.com","path":"owner/repo.git"}`)
	pf.tokenErr = assert.AnError
	_, _, _ = pf.askOwn(t)

	assert.Contains(t, info.String(), "push credential: answered session "+pf.me.ID+" for https://github.com/owner/repo")
	for name, out := range map[string]string{"info": info.String(), "warning": warn.String(), "error": errs.String()} {
		assert.NotContainsf(t, out, testPushToken, "the %s log carries the token", name)
	}
}

// classCredential is returned verbatim, so it is confined to the one route that
// exists to return a credential.
func TestSandboxCredentialClassIsConfinedToGitCredential(t *testing.T) {
	for owner, classes := range sandboxStringClasses {
		for name, spec := range classes {
			if spec.class == classCredential {
				assert.Equalf(t, "GitCredentialResponse", owner.Name(), "%s.%s is classCredential: only the push credential may bypass the scrub", owner.Name(), name)
			}
		}
	}
	assert.Equal(t, classCredential, sandboxStringClasses[reflect.TypeFor[GitCredentialResponse]()]["password"].class)
}

// The marker the route relies on is applied by the PRODUCTION endpoint, not only
// by this file's fixture: a request served by startSandboxEndpoint carries it,
// and one served behind the main listener's gate does not.
func TestSandboxEndpointMarksItsRequestsAndTheMainGateDoesNot(t *testing.T) {
	var reg sandboxTokenRegistry
	token, err := reg.mint("sess-a")
	require.NoError(t, err)
	var marked atomic.Bool
	var reached atomic.Int32
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		marked.Store(viaSandboxEndpoint(r.Context()))
		w.WriteHeader(http.StatusOK)
	})

	sock := filepath.Join(testguard.SocketTempDir(t), "cb.sock")
	ep, err := startSandboxEndpoint(mux, &reg, sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ep.close() })
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequest(http.MethodPost, "http://sandbox/v1/GitCredential", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(1), reached.Load())
	assert.True(t, marked.Load(), "the dedicated endpoint must mark its requests")

	// The main listener's gate admits the same credential to the same route, and
	// must NOT mark it.
	main := withAuth(mux, &authGate{
		expectedToken: func() (string, error) { return "operator-token", nil },
		sandboxTokens: &reg,
	}, nil)
	rec := httptest.NewRecorder()
	mreq := httptest.NewRequest(http.MethodPost, "/v1/GitCredential", strings.NewReader(`{}`))
	mreq.Header.Set("Authorization", "Bearer "+token)
	main.ServeHTTP(rec, mreq)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int32(2), reached.Load(), "anti-vacuous: the main gate admitted the sandbox credential")
	assert.False(t, marked.Load(), "a main-listener request must never read as the dedicated endpoint")
}
