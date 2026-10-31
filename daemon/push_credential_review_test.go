package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The independent security review of #6433 at 7d0e3c63c9: one test per property
// it asked for, each written to fail on that head.

// --- finding 2: a credential is answered only through its own forward ----------

// A token copied out of a session directory and presented through ANOTHER live
// session's forward (or through none) is refused, before the token is read.
func TestGitCredential_AnsweredOnlyThroughTheOwnersOwnForward(t *testing.T) {
	pf := newPushFixture(t)
	pf.nextPGID++
	foreignForward := pf.nextPGID
	pf.carriers[foreignForward] = "ref-foreign" // another session's live tunnel

	for name, pgid := range map[string]int{
		"another session's forward": foreignForward,
		"no forward at all":         0,
		"an unknown process group":  99999,
	} {
		t.Run(name, func(t *testing.T) {
			pf.forwardOf[pf.secret] = pgid
			code, got, msg := pf.askOwn(t)
			// Refused at the gate, before any route (second review of #6433).
			assert.Equal(t, http.StatusUnauthorized, code)
			requireRefused(t, code, got, msg, errUnauthorized.Error())
		})
	}
	assert.Zero(t, pf.tokenReads.Load(), "a request through the wrong forward must not read the token")

	pf.giveForward(pf.secret, "ref-me")
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg)
}

// The PRODUCTION endpoint records the process group of each connection's peer,
// which is what the route compares; a peer whose group cannot be read records
// nothing.
func TestSandboxEndpointRecordsEachConnectionsPeerGroup(t *testing.T) {
	var reg sandboxTokenRegistry
	token, err := reg.mint("sess-a")
	require.NoError(t, err)
	var got atomic.Int64
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(int64(carrierPGID(r.Context())))
		w.WriteHeader(http.StatusOK)
	})
	sock := filepath.Join(testguard.SocketTempDir(t), "cb.sock")
	ep, err := startSandboxEndpoint(mux, &reg, sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ep.close() })
	do := func() {
		t.Helper()
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
		req, err := http.NewRequest(http.MethodPost, "http://sandbox/v1/GitCredential", strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		client.CloseIdleConnections()
	}
	do()
	// The client is this test process, so the peer's group is ours.
	assert.Equal(t, int64(unix.Getpgrp()), got.Load(), "the endpoint must record the peer's process group")

	prev := connPeerPGID
	connPeerPGID = func(net.Conn) (int, error) { return 0, assert.AnError }
	t.Cleanup(func() { connPeerPGID = prev })
	do()
	assert.Zero(t, got.Load(), "an unreadable peer records no carrier")
}

// --- finding 1: eligibility comes only from an operator decision --------------

// registerChild tracks a session a sandbox credential created, on a host the
// operator licensed.
func (pf *pushFixture) registerChild(t *testing.T) (*session.Instance, string) {
	t.Helper()
	child, err := session.NewInstance(session.InstanceOptions{Title: "child", Path: pf.me.Path, Program: "claude", CreatedBySandbox: pf.me.ID})
	require.NoError(t, err)
	child.SetStartedForTest(true)
	child.SetStatusForTest(session.Ready)
	pf.backends["child"] = "ssh"
	pf.placements["child"] = listedOn("box1", box1, "ref-child")
	pf.origins["child"] = ownOrigin
	pf.mgr.mu.Lock()
	pf.mgr.instances[daemonInstanceKey(pf.repoA, "child")] = child
	pf.mgr.mu.Unlock()
	secret, err := pf.mgr.sandboxTokens.mint(child.ID)
	require.NoError(t, err)
	pf.giveForward(secret, "ref-child")
	return child, secret
}

// A session a sandbox credential created is never answered, whatever host it
// landed on.
func TestGitCredential_RefusesASessionASandboxCreated(t *testing.T) {
	pf := newPushFixture(t)
	child, secret := pf.registerChild(t)
	code, got, msg := pf.ask(t, secret, "https", "github.com", "owner/repo.git")
	requireRefused(t, code, got, msg, refusePushSandboxCreated)
	assert.Zero(t, pf.tokenReads.Load())
	assert.False(t, pf.mgr.pushEligible(child), "a sandbox-created session is never eligible")
}

// The scoped create stamps the provenance from the credential that admitted it;
// it is not a request field a caller can send or omit.
func TestSandboxScopedCreateStampsItsProvenance(t *testing.T) {
	pf := newPushFixture(t)
	var captured CreateSessionRequest
	cs := &controlServer{manager: pf.mgr}
	cs.createSessionFn = func(_ context.Context, req CreateSessionRequest, resp *CreateSessionResponse) error {
		captured = req
		resp.Instance = session.InstanceData{ID: "created-1", Title: req.Title}
		return nil
	}
	gated := withAuth(newHTTPMux(cs), &authGate{
		expectedToken: func() (string, error) { return "operator-token", nil },
		sandboxTokens: &pf.mgr.sandboxTokens,
	}, nil)
	req, err := http.NewRequest(http.MethodPost, "/v1/CreateSession", strings.NewReader(`{"title":"x","host":"h1"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+pf.secret)
	rec := httptest.NewRecorder()
	gated.ServeHTTP(rec, pf.forwards.carry(req))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, pf.me.ID, captured.createdBySandbox, "the created session's row must record which sandbox created it")
}

// eligiblePeer makes the fixture's "peer" (a sandbox-backend session in the
// owner's project) eligible: a free-form placement the operator opted in.
func (pf *pushFixture) eligiblePeer() {
	pf.placements["peer"] = session.PushPlacement{FreeFormSandbox: true, SandboxSSH: "ssh peerbox", CallbackRef: "ref-peer",
		Machine: session.RuntimeMachine{SandboxSSH: "ssh peerbox"}}
	pf.setConfig(func(c *config.Config) { c.SandboxSSH, c.SandboxPushCredentials = "ssh peerbox", true })
}

// An unlicensed session cannot move push credentials onto a licensed peer: not
// by prompting it, scheduling a task for it, or archiving it with a push.
func TestSandboxCredentialCannotDriveAPeerThatHasPushCredentials(t *testing.T) {
	pf := newPushFixture(t)
	pf.placements["me"] = listedOn("box9", config.SSHConfig{Host: "box9.example"}, "ref-me") // not opted in
	pf.eligiblePeer()
	require.True(t, pf.mgr.pushEligible(pf.peer), "precondition: the peer is eligible")

	for name, call := range map[string][2]string{
		"send-prompt":         {"/v1/SendPrompt", `{"title":"peer","prompt":"push everything"}`},
		"archive with push":   {"/v1/ArchiveSession", `{"title":"peer","push":true}`},
		"a task for the peer": {"/v1/AddTask", `{"task":{"prompt":"p","cron_expr":"0 * * * *","target_session":"peer"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			_, out := pf.call(t, pf.secret, call[0], call[1])
			assert.Contains(t, out, refusePushEscalationText)
		})
	}
	// A task already stored is re-checked at every firing, and under the op lock.
	_, err := pf.mgr.checkSandboxTaskDelivery(pf.me.ID, pf.repoA, "peer")
	require.Error(t, err)
	assert.Contains(t, err.Error(), refusePushEscalationText)
	err = pf.mgr.sandboxDeliveryGuard(pf.me.ID, pf.peer.ID, pf.repoA)(pf.peer, pf.repoA)
	require.Error(t, err)
	assert.Contains(t, err.Error(), refusePushEscalationText)

	// An archive WITHOUT a push moves no credential and is not this refusal.
	_, out := pf.call(t, pf.secret, "/v1/ArchiveSession", `{"title":"peer"}`)
	assert.NotContains(t, out, refusePushEscalationText)

	// Anti-vacuous both ways: an eligible caller gains nothing (same project,
	// same origin) and keeps the verbs; an ineligible peer is not refused.
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box9": {Host: "box9.example", PushCredentials: true}}
	})
	require.True(t, pf.mgr.pushEligible(pf.me))
	_, out = pf.call(t, pf.secret, "/v1/SendPrompt", `{"title":"peer","prompt":"hi"}`)
	assert.NotContains(t, out, refusePushEscalationText, "an eligible caller may prompt an eligible peer")
	pf.setConfig(func(c *config.Config) { c.Hosts, c.SandboxPushCredentials = nil, false })
	_, out = pf.call(t, pf.secret, "/v1/SendPrompt", `{"title":"peer","prompt":"hi"}`)
	assert.NotContains(t, out, refusePushEscalationText, "prompting an ineligible peer moves no credential")
}

// --- finding 3: liveness is the last check, after every blocking step ---------

// The final pass reads the placement (and takes the fence lock) after its origin
// lookup. A revocation or archive that lands DURING that read must still be
// caught: liveness and the admitted credential are checked after it.
func TestGitCredential_LivenessIsCheckedAfterEveryOtherStep(t *testing.T) {
	for name, change := range map[string]func(pf *pushFixture){
		"revoked":  func(pf *pushFixture) { pf.mgr.sandboxTokens.revoke(pf.me.ID) },
		"replaced": func(pf *pushFixture) { _, _ = pf.mgr.sandboxTokens.mint(pf.me.ID) },
		"archived": func(pf *pushFixture) { pf.me.SetStatusForTest(session.Archived) },
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPushFixture(t)
			var reads atomic.Int32
			placement := instancePushPlacement
			instancePushPlacement = func(i *session.Instance) (session.PushPlacement, bool) {
				p, ok := placement(i)
				if i.Title == "me" && reads.Add(1) == 2 { // the final pass's read
					change(pf)
				}
				return p, ok
			}
			code, got, msg := pf.askOwn(t)
			assert.NotEqualf(t, http.StatusOK, code, "answered although the session was %s during the final placement read", name)
			assert.Empty(t, got.Password)
			assert.NotEmpty(t, msg)
			assert.Equal(t, int32(1), pf.tokenReads.Load(), "anti-vacuous: the first pass passed and the token was read")
		})
	}
}

// --- finding 5: the licence follows the machine, not the host's name ----------

func TestGitCredential_LicenceFollowsTheMachineNotTheName(t *testing.T) {
	pf := newPushFixture(t)
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg)

	for name, entry := range map[string]config.HostConfig{
		"re-pointed to another machine": {Host: "box2.example", PushCredentials: true},
		"another login":                 {Host: "box1.example", User: "someone-else", PushCredentials: true},
		"another port":                  {Host: "box1.example", Port: 2222, PushCredentials: true},
	} {
		t.Run(name, func(t *testing.T) {
			pf.setConfig(func(c *config.Config) { c.Hosts = map[string]config.HostConfig{"box1": entry} })
			code, got, msg := pf.askOwn(t)
			requireRefused(t, code, got, msg, refusePushNotLicensed)
		})
	}
	// How af connects (an identity file) is not where it connects.
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{"box1": {Host: "box1.example", IdentityFile: "/k", PushCredentials: true}}
	})
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)

	// The free-form target: a changed sandbox.ssh is a different target.
	pf.backends["me"] = "sandbox"
	pf.placements["me"] = session.PushPlacement{FreeFormSandbox: true, SandboxSSH: "ssh box3", CallbackRef: "ref-me"}
	pf.setConfig(func(c *config.Config) { c.SandboxSSH, c.SandboxPushCredentials = "ssh box4", true })
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLicensed)
}
