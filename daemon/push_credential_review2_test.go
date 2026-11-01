package daemon

import (
	"bytes"
	"context"
	"fmt"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The second independent security review of #6433, of 6272bfd2e4: one test per
// property it asked for, each written to fail on that head.

// --- finding 2: every sandbox route, only through the owner's own forward -------

// A leaked credential for eligible E, sent through ANOTHER session's forward or
// through none (a listener with no forward records no carrier), is refused by the
// gate on every route a sandbox credential may call — CreateTab, SendPrompt and
// AddTask included, which would otherwise run code inside E. Through E's own
// forward the same requests pass the gate.
func TestEverySandboxRouteAdmitsACredentialOnlyThroughItsOwnersForward(t *testing.T) {
	pf := newPushFixture(t)
	otherToken, err := pf.mgr.sandboxTokens.mint(pf.other.ID)
	require.NoError(t, err)
	foreign := pf.giveForward(otherToken, "ref-foreign")

	send := func(path, body string, pgid int) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+pf.secret)
		req.Header.Set("Content-Type", "application/json")
		if pgid != 0 {
			req = withCarrier(req, pgid)
		}
		rec := httptest.NewRecorder()
		pf.endpoint.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	paths := make([]string, 0, len(sandboxAllowedPaths))
	for p := range sandboxAllowedPaths {
		paths = append(paths, p)
	}
	require.Contains(t, paths, "/v1/CreateTab")
	for _, path := range paths {
		for name, pgid := range map[string]int{"another session's forward": foreign, "no forward": 0, "an unknown group": 99999} {
			code, body := send(path, `{}`, pgid)
			assert.Equalf(t, http.StatusUnauthorized, code, "%s through %s: %s", path, name, body)
			assert.NotContains(t, body, testPushToken)
		}
	}
	assert.Zero(t, pf.tokenReads.Load())

	// Anti-vacuous: through E's own forward the gate admits each of them (the
	// handler may still refuse the body, but not as unauthorized).
	own := pf.forwardOf[pf.secret]
	// Empty bodies: the handler refuses them, so nothing is started.
	for path, body := range map[string]string{
		"/v1/CreateTab":  `{}`,
		"/v1/SendPrompt": `{}`,
		"/v1/AddTask":    `{}`,
	} {
		code, body := send(path, body, own)
		assert.NotEqualf(t, http.StatusUnauthorized, code, "%s through the owner's own forward: %s", path, body)
	}
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg)
}

// The route keeps its own forward check beside the gate's, against the row's
// runtime identity rather than the registry's.
func TestGitCredential_RouteAlsoBindsTheForward(t *testing.T) {
	pf := newPushFixture(t)
	foreign := pf.giveForward("not-a-token", "ref-foreign")
	_, _, _, err := pf.mgr.authorizePushCredential(pushAsk{owner: pf.me.ID, grant: pf.secret, carrier: foreign,
		req: GitCredentialRequest{Protocol: "https", Host: "github.com", Path: "owner/repo.git"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), refusePushWrongForward)
}

// --- finding 1: while a host is licensed, no code the operator didn't place -----

// placeOn records title's runtime as running on machine m.
func (pf *pushFixture) placeOn(t *testing.T, title string, m session.RuntimeMachine) {
	t.Helper()
	if pf.machines == nil {
		pf.machines = map[string]session.RuntimeMachine{}
		prev := instanceRuntimeMachine
		instanceRuntimeMachine = func(i *session.Instance) (session.RuntimeMachine, bool) {
			m, ok := pf.machines[i.Title]
			return m, ok
		}
		t.Cleanup(func() { instanceRuntimeMachine = prev })
	}
	pf.machines[title] = m
}

// track adds a row to the owner's project from data (so its provenance is
// exactly what the data says). A row whose data is Archived stays an archive;
// any other is a started, ready row.
func (pf *pushFixture) track(t *testing.T, data session.InstanceData) *session.Instance {
	t.Helper()
	archived := data.Status == session.Archived
	// The fixture's backend seam, not the row's own type, decides the backend.
	data.Path = pf.me.Path
	if !archived {
		data.Status = session.Ready
	}
	if data.BackendType == "" {
		data.BackendType = "local"
	}
	data.Worktree = session.GitWorktreeData{RepoPath: pf.me.Path, WorktreePath: pf.me.Path}
	inst, err := session.FromInstanceData(data)
	require.NoError(t, err)
	if !archived {
		inst.SetStartedForTest(true)
		inst.SetStatusForTest(session.Ready)
	}
	pf.backends[data.Title] = "ssh"
	pf.mgr.mu.Lock()
	pf.mgr.instances[daemonInstanceKey(pf.repoA, data.Title)] = inst
	pf.mgr.mu.Unlock()
	return inst
}

func TestSandboxCannotCreateOnAPushLicensedMachine(t *testing.T) {
	pf := newPushFixture(t)
	pf.setConfig(func(c *config.Config) {
		c.Hosts = map[string]config.HostConfig{
			"box1":       {Host: "box1.example", PushCredentials: true},
			"box1-alias": {Host: "BOX1.example"},
			"by-address": {Host: "198.51.100.7"},
			"box9":       {Host: "box9.example"},
		}
	})
	// A live session on box1 recorded the address its ssh runtime pinned.
	p := pf.placements["me"]
	p.Machine = session.RuntimeMachine{Host: "box1.example", Addr: "198.51.100.7"}
	pf.placements["me"] = p

	var created []string
	cs := &controlServer{manager: pf.mgr}
	cs.createSessionFn = func(_ context.Context, req CreateSessionRequest, resp *CreateSessionResponse) error {
		created = append(created, req.Host)
		resp.Instance = session.InstanceData{ID: "c-" + req.Host, Title: req.Title}
		return nil
	}
	gated := withAuth(newHTTPMux(cs), &authGate{
		expectedToken: func() (string, error) { return "operator-token", nil },
		sandboxTokens: &pf.mgr.sandboxTokens,
	}, nil)
	create := func(host string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/CreateSession", strings.NewReader(`{"title":"x","host":"`+host+`"}`))
		req.Header.Set("Authorization", "Bearer "+pf.secret)
		rec := httptest.NewRecorder()
		gated.ServeHTTP(rec, pf.forwards.carry(req))
		return rec.Code, rec.Body.String()
	}
	for _, host := range []string{"box1", " box1 ", "box1-alias", "by-address"} {
		code, body := create(host)
		assert.NotEqualf(t, http.StatusOK, code, "create on %q", host)
		assert.Containsf(t, body, refuseSandboxCreateHost, "create on %q", host)
	}
	assert.Empty(t, created, "nothing reached the create")

	// Third review of #6433: the refusal must not tell a sandbox WHICH names are
	// licensed, so a licensed name answers exactly as a name that is not listed.
	_, licensed := create("box1")
	_, unlisted := create("not-a-listed-host")
	assert.Equal(t, licensed, unlisted, "a licensed host and an unlisted one must be refused alike")
	assert.NotContains(t, licensed, "push", "nor name the reason")

	code, body := create("box9")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"box9"}, created, "an unlicensed machine still takes a sandbox's create")

	// The free-form target, opted in, names box9 too: then box9 is licensed.
	pf.setConfig(func(c *config.Config) { c.SandboxSSH, c.SandboxPushCredentials = "ssh dev@box9.example", true })
	code, body = create("box9")
	assert.NotEqual(t, http.StatusOK, code)
	assert.Contains(t, body, refuseSandboxCreateHost)
}

// --- N1: unknown provenance is not eligible ----------------------------------------

func TestGitCredential_RefusesUnknownProvenance(t *testing.T) {
	pf := newPushFixture(t)
	legacy := pf.track(t, session.InstanceData{ID: "legacy-1", Title: "legacy"}) // neither stamp
	pf.placements["legacy"] = listedOn("box1", box1, "ref-legacy")
	pf.origins["legacy"] = ownOrigin
	secret, err := pf.mgr.sandboxTokens.mint(legacy.ID)
	require.NoError(t, err)
	pf.giveForward(secret, "ref-legacy")

	code, got, msg := pf.ask(t, secret, "https", "github.com", "owner/repo.git")
	requireRefused(t, code, got, msg, refusePushSandboxCreated)
	assert.False(t, pf.mgr.pushEligible(legacy), "and the escalation guard treats it as ineligible")

	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)
}

// --- N3: the audit line records the outcome, after the final check ------------

func TestGitCredential_AuditLineFollowsTheFinalCheck(t *testing.T) {
	pf := newPushFixture(t)
	var buf bytes.Buffer
	pf.mgr.infoLog = stdlog.New(&buf, "", 0)
	prev := testHookPushBeforeFinalCheck
	testHookPushBeforeFinalCheck = func() { pf.mgr.sandboxTokens.revoke(pf.me.ID) }
	t.Cleanup(func() { testHookPushBeforeFinalCheck = prev })

	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushNotLive)
	assert.NotContains(t, buf.String(), "push credential: answered", "a request refused at the final check was logged as answered")
	assert.Contains(t, buf.String(), "refused session "+pf.me.ID)
}

// --- third review of #6433: the cross-forward refusal log is rate-limited -------

func TestCrossForwardRefusalLogIsRateLimited(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var lines []string
	l := &crossForwardLog{now: func() time.Time { return now }, lastLogged: map[string]time.Time{},
		logf: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }}
	for i := 0; i < 50; i++ {
		l.record("sess-a", "a listener with no ssh forward")
	}
	require.Len(t, lines, 1, "one line per session per interval, however often it is refused")
	for i := 0; i < 30; i++ {
		l.record(fmt.Sprintf("sess-%d", i), "a listener with no ssh forward")
	}
	assert.Len(t, lines, crossForwardLogBurst, "at most a burst a minute across all sessions")

	now = now.Add(crossForwardLogInterval)
	l.record("sess-a", "a listener with no ssh forward")
	require.Len(t, lines, crossForwardLogBurst+1)
	assert.Contains(t, lines[len(lines)-1], "further refusals not logged", "the suppressed count is carried into the next line")
}
