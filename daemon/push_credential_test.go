package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	// carriers maps a forward's process group to the callback identity its
	// tunnel carries; forwardOf maps a presented token to the process group the
	// request arrives through (0: no forward at all).
	carriers   map[int]string
	forwardOf  map[string]int
	nextPGID   int
	tokenReads atomic.Int32
	// onTokenRead runs inside the token read, so a test can change the world
	// between the two authorizations.
	onTokenRead func()
	tokenErr    error
	// machines is the machine each row's runtime runs on, by title (placeOn).
	machines map[string]session.RuntimeMachine
}

var ownOrigin = pushcred.Origin{Host: "github.com", Path: "owner/repo"}

// box1 is the listed host the fixture's own session runs on, as recorded at
// placement and as the live host list names it.
var box1 = config.SSHConfig{Host: "box1.example"}

// listedOn is a placement on listed host name, recorded at destination, whose
// runtime's callback identity is ref.
func listedOn(name string, destination config.SSHConfig, ref string) session.PushPlacement {
	return session.PushPlacement{ListedHost: name, Destination: destination, CallbackRef: ref,
		Machine: session.RuntimeMachine{Host: destination.Host}}
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	f := newScopeFixture(t)
	pf := &pushFixture{
		scopeFixture: f,
		placements:   map[string]session.PushPlacement{"me": listedOn("box1", box1, "ref-me")},
		origins:      map[string]pushcred.Origin{"me": ownOrigin},
		carriers:     map[int]string{},
		forwardOf:    map[string]int{},
		nextPGID:     41000,
	}
	pf.giveForward(f.secret, "ref-me")
	prevCarrier := callbackCarrierRef
	callbackCarrierRef = func(pgid int) (string, bool) {
		if ref, ok := pf.carriers[pgid]; ok {
			return ref, true
		}
		return prevCarrier(pgid) // the scope fixture's own forwards
	}
	t.Cleanup(func() { callbackCarrierRef = prevCarrier })
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

// giveForward gives token its own forward: a tunnel process group carrying
// callback identity ref, through which token's requests arrive. ref becomes the
// ref the token's credential derives from, as it is for a real runtime.
func (pf *pushFixture) giveForward(token, ref string) int {
	if owner, ok := pf.mgr.sandboxTokens.sessionFor(token); ok {
		pf.mgr.sandboxTokens.setRefForTest(owner, ref)
	}
	pf.nextPGID++
	pf.carriers[pf.nextPGID] = ref
	pf.forwardOf[token] = pf.nextPGID
	return pf.nextPGID
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
	// What the endpoint's ConnContext records for the connection's peer.
	if pgid := pf.forwardOf[token]; pgid > 0 {
		req = req.WithContext(context.WithValue(req.Context(), carrierPGIDKey{}, pgid))
	}
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
	pf.placements["foreign"] = listedOn("box1", box1, "ref-foreign")
	pf.origins["foreign"] = pushcred.Origin{Host: "github.com", Path: "owner/other"}
	other, err := pf.mgr.sandboxTokens.mint(pf.other.ID)
	require.NoError(t, err)
	pf.giveForward(other, "ref-foreign")

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
	pf.placements["me"] = session.PushPlacement{FreeFormSandbox: true, SandboxSSH: "ssh box3", CallbackRef: "ref-me"}
	pf.setConfig(func(c *config.Config) { c.SandboxPushCredentials, c.SandboxSSH = true, "ssh box3" })
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)
}

func TestGitCredential_FreeFormSandboxNeedsItsOwnOptIn(t *testing.T) {
	pf := newPushFixture(t)
	pf.backends["me"] = "sandbox"
	pf.placements["me"] = session.PushPlacement{FreeFormSandbox: true, SandboxSSH: "ssh box3", CallbackRef: "ref-me"}
	pf.setConfig(func(c *config.Config) { c.SandboxSSH = "ssh box3" })

	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)

	pf.setConfig(func(c *config.Config) { c.SandboxPushCredentials = true })
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)

	// A listed host's opt-in does not reach a free-form placement.
	pf.setConfig(func(c *config.Config) { c.SandboxPushCredentials = false })
	pf.placements["me"] = listedOn("box1", box1, "ref-me")
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
		"revoked":  func(pf *pushFixture) { pf.mgr.sandboxTokens.revoke(pf.me.ID) },
		"archived": func(pf *pushFixture) { pf.me.SetStatusForTest(session.Archived) },
		// Codex P1 on #6433: a REPLACEMENT credential minted mid-read leaves the
		// session holding a credential, just not the one this request presented.
		"replaced":    func(pf *pushFixture) { _, _ = pf.mgr.sandboxTokens.mint(pf.me.ID) },
		"licence off": func(pf *pushFixture) { pf.setConfig(func(c *config.Config) { c.Hosts = nil }) },
		"auto push": func(pf *pushFixture) {
			_, _ = pf.mgr.pushCreds.beginAutoPush(pf.me.ID, "ref-me", nil)
		},
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

// The final authorization's one blocking step is the origin lookup (a git run in
// production). Whatever changes DURING it is still caught, because every state
// check runs again after it and nothing blocks between that and the reply (Codex
// P1 on #6433).
func TestGitCredential_ChangesDuringTheFinalOriginLookupAreCaught(t *testing.T) {
	for name, change := range map[string]func(pf *pushFixture){
		"revoked":     func(pf *pushFixture) { pf.mgr.sandboxTokens.revoke(pf.me.ID) },
		"replaced":    func(pf *pushFixture) { _, _ = pf.mgr.sandboxTokens.mint(pf.me.ID) },
		"archived":    func(pf *pushFixture) { pf.me.SetStatusForTest(session.Archived) },
		"licence off": func(pf *pushFixture) { pf.setConfig(func(c *config.Config) { c.Hosts = nil }) },
		"auto push": func(pf *pushFixture) {
			_, _ = pf.mgr.pushCreds.beginAutoPush(pf.me.ID, "ref-me", nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPushFixture(t)
			var lookups atomic.Int32
			lookup := instancePushOrigin
			instancePushOrigin = func(i *session.Instance) (pushcred.Origin, bool) {
				// The second pass's lookup: the token has been read.
				if lookups.Add(1) == 2 {
					change(pf)
				}
				return lookup(i)
			}
			code, got, msg := pf.askOwn(t)
			assert.NotEqualf(t, http.StatusOK, code, "answered after %s during the final origin lookup", name)
			assert.Empty(t, got.Password)
			assert.NotEmpty(t, msg)
			assert.Equal(t, int32(2), lookups.Load(), "anti-vacuous: the change landed in the final pass's lookup")
		})
	}
}

// W7.8 changes no af-initiated push (the owner's answer to the plan's Q4): while
// the daemon runs an automatic preserve-push for a row, that row gets nothing.
func TestGitCredential_WithheldDuringAnAutomaticPreservePush(t *testing.T) {
	pf := newPushFixture(t)
	end, err := pf.mgr.pushCreds.beginAutoPush(pf.me.ID, "ref-me", nil)
	require.NoError(t, err)
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushAutoPreserve)

	// Another session is unaffected by this row's fence.
	pf.placements["foreign"] = listedOn("box1", box1, "ref-foreign")
	pf.origins["foreign"] = pushcred.Origin{Host: "github.com", Path: "owner/other"}
	other, err := pf.mgr.sandboxTokens.mint(pf.other.ID)
	require.NoError(t, err)
	pf.giveForward(other, "ref-foreign")
	code, got, msg = pf.ask(t, other, "https", "github.com", "owner/other.git")
	requireAnswered(t, code, got, msg)

	end(true)
	end(true) // idempotent
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)
}

// preserveSandboxBeforeReap holds the fence across the in-sandbox archive (where
// the push runs). A CONFIRMED success lifts it. Anything else may leave that
// archive running (it takes no context and its git steps have no deadline), so
// the row stays fenced for as long as the credential it held — elapsed time is
// never read as completion (Codex P2 on #6433).
func TestPreserveSandboxBeforeReapFencesPushCredentials(t *testing.T) {
	for name, tc := range map[string]struct {
		fail       bool
		wantFenced bool
	}{
		"confirmed success lifts the fence": {fail: false, wantFenced: false},
		"a failure keeps it for the grant":  {fail: true, wantFenced: true},
	} {
		t.Run(name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			var inst *session.Instance
			const ref = "ref-fenced"
			placeWithCallbackRef(t, "fenced", ref)
			var fencedDuringArchive atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/agent/archive" {
					fencedDuringArchive.Store(manager.pushCreds.autoPushFenced(inst.ID, ref))
					if tc.fail {
						http.Error(w, "push rejected by origin", http.StatusInternalServerError)
						return
					}
					writeSandboxProbeEnvelope(w, map[string]any{"branch": "af/fenced"})
					return
				}
				http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			}))
			t.Cleanup(srv.Close)
			inst, _ = registerStartedRemote(t, manager, repoID, repoPath, "fenced", srv.URL, session.Ready)
			// Review finding 4: the daemon holds NO token for this row (none was
			// minted, as for a row not yet reattached after a restart). The fence
			// keys on the runtime's callback identity, so it holds anyway.
			manager.sandboxTokens.mu.RLock()
			_, registered := manager.sandboxTokens.bySession[inst.ID]
			manager.sandboxTokens.mu.RUnlock()
			require.False(t, registered, "precondition: no registered token")

			require.False(t, manager.pushCreds.autoPushFenced(inst.ID, ref), "precondition: no fence")
			_ = manager.preserveSandboxBeforeReap(repoID, daemonInstanceKey(repoID, "fenced"), inst, "")
			assert.True(t, fencedDuringArchive.Load(), "the push ran while push credentials were still answerable for its row")
			assert.Equal(t, tc.wantFenced, manager.pushCreds.autoPushFenced(inst.ID, ref))
			assert.Equal(t, tc.wantFenced, (&pushCredentialState{}).autoPushFenced(inst.ID, ref),
				"the fence is on disk for the next daemon too")

			// A replacement runtime's new callback identity is not fenced by the old
			// one's abandoned archive.
			assert.False(t, manager.pushCreds.autoPushFenced(inst.ID, "ref-replacement"), "the fence follows the runtime, not the session forever")
		})
	}
}

// An abandoned automatic push fences the route for exactly the credential that
// was live then; the session's next credential (a replacement runtime) is
// answered.
func TestGitCredential_AbandonedAutoPushFencesOnlyThatCredential(t *testing.T) {
	pf := newPushFixture(t)
	end, err := pf.mgr.pushCreds.beginAutoPush(pf.me.ID, "ref-me", nil)
	require.NoError(t, err)
	end(false) // timed out, lost, refused: not confirmed
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushAutoPreserve)

	// The SAME runtime re-derives the same credential after a restart: still
	// fenced.
	again, err := pf.mgr.sandboxTokens.mint(pf.me.ID)
	require.NoError(t, err)
	pf.giveForward(again, "ref-me")
	code, got, msg = pf.ask(t, again, "https", "github.com", "owner/repo.git")
	requireRefused(t, code, got, msg, refusePushAutoPreserve)

	// A replacement runtime has a new callback identity and is answered.
	pf.placements["me"] = listedOn("box1", box1, "ref-me-2")
	fresh, err := pf.mgr.sandboxTokens.mint(pf.me.ID)
	require.NoError(t, err)
	pf.giveForward(fresh, "ref-me-2")
	code, got, msg = pf.ask(t, fresh, "https", "github.com", "owner/repo.git")
	requireAnswered(t, code, got, msg)
}

// A daemon restart re-derives the very same callback credential (W3.6), so a
// fence held only in memory would lift while an abandoned archive ran on (Codex
// P2 on #6433). The fence is persisted before the push starts and read back by
// the next process; a confirmed success removes it from the record too.
func TestPushFencesSurviveADaemonRestart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), pushFenceFileName)
	const grant = "ref-runtime-0001" // the runtime's callback identity: not a secret

	first := &pushCredentialState{fencePath: path}
	end, err := first.beginAutoPush("sess-a", grant, nil)
	require.NoError(t, err)
	// The process dies mid-push: end never runs.
	_ = end
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), grant, "the record names the runtime's callback identity (a non-secret ref)")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	second := &pushCredentialState{fencePath: path}
	assert.True(t, second.autoPushFenced("sess-a", grant), "the restarted daemon must still fence the re-derived credential")
	assert.False(t, second.autoPushFenced("sess-a", "ref-runtime-0002"), "a replacement runtime is not fenced")
	assert.False(t, second.autoPushFenced("sess-b", grant))

	// A later automatic push that is CONFIRMED lifts it, durably.
	end, err = second.beginAutoPush("sess-a", grant, nil)
	require.NoError(t, err)
	end(true)
	third := &pushCredentialState{fencePath: path}
	assert.False(t, third.autoPushFenced("sess-a", grant))
}

// An unreadable record may be hiding a fence: every request is refused, and no
// automatic push starts, until it can be read — and repairing it restores
// service with no restart (Codex P2 on #6433: the first failure used to stick).
func TestPushFencesFailClosedOnAnUnreadableRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), pushFenceFileName)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	s := &pushCredentialState{fencePath: path}
	assert.True(t, s.autoPushFenced("any", "grant"))
	_, err := s.beginAutoPush("any", "grant", nil)
	assert.Error(t, err)
	assert.True(t, s.autoPushFenced("any", "grant"), "still failing closed on the next request")

	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	assert.False(t, s.autoPushFenced("any", "grant"), "a repaired record is read on the next use")
	end, err := s.beginAutoPush("any", "grant", nil)
	require.NoError(t, err)
	end(true)

	nullRecord := filepath.Join(t.TempDir(), pushFenceFileName)
	require.NoError(t, os.WriteFile(nullRecord, []byte("null"), 0o600))
	n := &pushCredentialState{fencePath: nullRecord}
	assert.False(t, n.autoPushFenced("any", "grant"))
	end, err = n.beginAutoPush("any", "grant", nil)
	require.NoError(t, err, "a null record reads as empty, not as a panic")
	end(true)
}

// A fence is never released by age: the abandoned archive's git steps have no
// deadline, so a record from long ago still fences the credential it names
// (Codex P2 on #6433).
func TestPushFencesDoNotExpireByAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), pushFenceFileName)
	old := map[string]pushFence{"sess-a": {CallbackRef: "g1", At: time.Now().Add(-60 * 24 * time.Hour)}}
	raw, err := json.Marshal(old)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	s := &pushCredentialState{fencePath: path}
	assert.True(t, s.autoPushFenced("sess-a", "g1"), "a 60-day-old fence still holds")
	// Writing the record for another session keeps it: the snapshot names no
	// runtime for sess-a, which proves nothing about whether it can return.
	end, err := s.beginAutoPush("sess-b", "gb", nil)
	require.NoError(t, err)
	end(false)
	reread := &pushCredentialState{fencePath: path}
	assert.True(t, reread.autoPushFenced("sess-a", "g1"))
}

// The one thing that makes a fence moot: its session now holds a DIFFERENT
// credential (a replacement runtime), so the fenced one can never be re-derived.
func TestPushFencesArePrunedOnlyWhenTheCredentialWasReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), pushFenceFileName)
	s := &pushCredentialState{fencePath: path}
	end, err := s.beginAutoPush("sess-a", "g1", nil)
	require.NoError(t, err)
	end(false)

	end, err = s.beginAutoPush("sess-b", "gb", liveRefs(s, map[string]string{"sess-a": "g1"}))
	require.NoError(t, err)
	end(false)
	assert.True(t, (&pushCredentialState{fencePath: path}).autoPushFenced("sess-a", "g1"), "the same credential is still live: kept")

	// Replaced: the session's runtime now has a different callback identity.
	end, err = s.beginAutoPush("sess-c", "gc", liveRefs(s, map[string]string{"sess-a": "g2"}))
	require.NoError(t, err)
	end(false)
	reread := &pushCredentialState{fencePath: path}
	assert.False(t, reread.autoPushFenced("sess-a", "g1"), "a replaced credential's fence is dropped")
	assert.True(t, reread.autoPushFenced("sess-b", "gb"), "other fences survive the prune")
}

// If the fence cannot be recorded, the automatic push does not run.
func TestPreserveSandboxBeforeReapRefusesWhenItsFenceCannotBeRecorded(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	var archived atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archived.Store(true)
		writeSandboxProbeEnvelope(w, map[string]any{"branch": "af/x"})
	}))
	t.Cleanup(srv.Close)
	placeWithCallbackRef(t, "unfenceable", "ref-unfenceable")
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "unfenceable", srv.URL, session.Ready)
	manager.pushCreds.fencePath = filepath.Join(t.TempDir(), "missing-dir", "deeper", pushFenceFileName)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(manager.pushCreds.fencePath))), []byte("a file, not a dir"), 0o600))

	err := manager.preserveSandboxBeforeReap(repoID, daemonInstanceKey(repoID, "unfenceable"), inst, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not be written")
	assert.False(t, archived.Load(), "the automatic push must not start without its fence")
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

// placeWithCallbackRef makes the row titled title report a placement whose
// runtime's callback identity is ref, for tests that drive a row built without a
// real ssh runtime.
func placeWithCallbackRef(t *testing.T, title, ref string) {
	t.Helper()
	prev := instancePushPlacement
	instancePushPlacement = func(i *session.Instance) (session.PushPlacement, bool) {
		if i.Title == title {
			return session.PushPlacement{ListedHost: "box1", Destination: box1, CallbackRef: ref}, true
		}
		return prev(i)
	}
	t.Cleanup(func() { instancePushPlacement = prev })
}

// liveRefs is a snapshot source over refs, a map the test mutates to stand for
// the sessions' runtimes changing: each call reads it, and the write mark, NOW.
func liveRefs(s *pushCredentialState, refs map[string]string) func() *pushRefSnapshot {
	return func() *pushRefSnapshot {
		copied := make(map[string]string, len(refs))
		for k, v := range refs {
			copied[k] = v
		}
		return &pushRefSnapshot{refs: copied, upTo: s.writeMark()}
	}
}

// Codex P2 on #6433 at 6272bfd: two sessions recovering at once. Evidence read
// before A's replacement runtime was fenced must never delete that NEWER fence,
// at B's begin or at B's confirmed end. A fence read back from disk predates
// every snapshot, so a moot one is still pruned after a restart.
func TestPushFencesAreNeverPrunedOnAStaleSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), pushFenceFileName)
	s := &pushCredentialState{fencePath: path}
	stale := &pushRefSnapshot{refs: map[string]string{"sess-a": "ref-a-old"}, upTo: s.writeMark()}
	staleSource := func() *pushRefSnapshot { return stale }

	endA, err := s.beginAutoPush("sess-a", "ref-a-new", liveRefs(s, map[string]string{"sess-a": "ref-a-new"}))
	require.NoError(t, err)
	endA(false) // A's automatic archive timed out: its fence must stay

	endB, err := s.beginAutoPush("sess-b", "ref-b", staleSource)
	require.NoError(t, err)
	assert.True(t, (&pushCredentialState{fencePath: path}).autoPushFenced("sess-a", "ref-a-new"), "B's begin pruned A's newer fence from a stale snapshot")
	endB(true)
	assert.True(t, (&pushCredentialState{fencePath: path}).autoPushFenced("sess-a", "ref-a-new"), "B's confirmed end pruned A's newer fence")
	assert.False(t, (&pushCredentialState{fencePath: path}).autoPushFenced("sess-b", "ref-b"), "B's own fence is lifted")

	// After a restart, A's fence is read back (write number 0): a snapshot that
	// shows A on yet another runtime does make it moot.
	restarted := &pushCredentialState{fencePath: path}
	endC, err := restarted.beginAutoPush("sess-c", "ref-c", liveRefs(restarted, map[string]string{"sess-a": "ref-a-third"}))
	require.NoError(t, err)
	endC(false)
	assert.False(t, (&pushCredentialState{fencePath: path}).autoPushFenced("sess-a", "ref-a-new"), "a fence a replacement made moot is pruned")
	assert.True(t, (&pushCredentialState{fencePath: path}).autoPushFenced("sess-c", "ref-c"))
}

// Second security review of #6433, N2: a confirmed end prunes against the
// identities current AT THE END. A was replaced while B's push ran, so A's old
// fence is moot by then, though it was live when B began.
func TestPushFenceEndPrunesAgainstTheIdentitiesCurrentAtTheEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), pushFenceFileName)
	s := &pushCredentialState{fencePath: path}
	current := map[string]string{"sess-a": "ref-a1"}
	endA, err := s.beginAutoPush("sess-a", "ref-a1", liveRefs(s, current))
	require.NoError(t, err)
	endA(false)

	endB, err := s.beginAutoPush("sess-b", "ref-b", liveRefs(s, current))
	require.NoError(t, err)
	assert.True(t, (&pushCredentialState{fencePath: path}).autoPushFenced("sess-a", "ref-a1"), "A's runtime is still the fenced one")

	current["sess-a"] = "ref-a2" // A is replaced while B's push runs
	endB(true)
	reread := &pushCredentialState{fencePath: path}
	assert.False(t, reread.autoPushFenced("sess-a", "ref-a1"), "the end pruned against the identities read when B began")
	assert.False(t, reread.autoPushFenced("sess-b", "ref-b"))
}
