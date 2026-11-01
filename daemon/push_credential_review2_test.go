package daemon

import (
	"bytes"
	"context"
	"errors"
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
	_, _, err := pf.mgr.authorizePushCredential(pushAsk{owner: pf.me.ID, grant: pf.secret, carrier: foreign,
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

func TestGitCredential_WithheldWhileASessionTheOperatorDidNotCreateSharesTheMachine(t *testing.T) {
	box1Machine := session.RuntimeMachine{Host: "box1.example", Addr: "198.51.100.7"}
	for name, tc := range map[string]struct {
		data    session.InstanceData
		machine *session.RuntimeMachine
		refused bool
	}{
		"sandbox-created, same machine":        {session.InstanceData{ID: "t1", Title: "t", CreatedBySandbox: "x"}, &box1Machine, true},
		"unknown provenance, same machine":     {session.InstanceData{ID: "t1", Title: "t"}, &box1Machine, true},
		"sandbox-created, same pinned address": {session.InstanceData{ID: "t1", Title: "t", CreatedBySandbox: "x"}, &session.RuntimeMachine{Host: "alias.example", Addr: "198.51.100.7"}, true},
		"sandbox-created, another machine":     {session.InstanceData{ID: "t1", Title: "t", CreatedBySandbox: "x"}, &session.RuntimeMachine{Host: "box9.example"}, false},
		"sandbox-created, no runtime":          {session.InstanceData{ID: "t1", Title: "t", CreatedBySandbox: "x"}, nil, false},
		"operator-created, same machine":       {session.InstanceData{ID: "t1", Title: "t", CreatedByOperator: true}, &box1Machine, false},
		// Fourth review of #6433, 1: an archive does not end a process a hook
		// detached, so an archived row keeps blocking until it is KILLED (this
		// reverses the Codex P2 skip of e17864b).
		"sandbox-created, a preserved archive": {session.InstanceData{ID: "t1", Title: "t", CreatedBySandbox: "x",
			Status: session.Archived, ArchivedWorkspace: &session.ArchivedWorkspaceData{Host: "box1", Dir: "/s"}}, &box1Machine, true},
		// Third review of #6433, D: a repository's own [ssh] target is a placement
		// the repository chose, whoever created the session.
		"operator-created, a repo's [ssh] target on the same machine": {session.InstanceData{ID: "t1", Title: "t", CreatedByOperator: true},
			&session.RuntimeMachine{Host: "box1.example", RepoChosen: true}, true},
		"operator-created, a repo's [ssh] target elsewhere": {session.InstanceData{ID: "t1", Title: "t", CreatedByOperator: true},
			&session.RuntimeMachine{Host: "box9.example", RepoChosen: true}, false},
		// B: a re-provision in flight runs somewhere af can't name yet.
		"sandbox-created, being re-provisioned": {session.InstanceData{ID: "t1", Title: "t", CreatedBySandbox: "x"},
			&session.RuntimeMachine{Unknown: true}, true},
		"operator-created on a listed host, being re-provisioned": {session.InstanceData{ID: "t1", Title: "t", CreatedByOperator: true},
			&session.RuntimeMachine{Unknown: true}, false},
		// C: a machine that can't be shown to differ is the same one.
		"sandbox-created, on a name that does not resolve": {session.InstanceData{ID: "t1", Title: "t", CreatedBySandbox: "x"},
			&session.RuntimeMachine{Host: "nowhere.example"}, true},
		"sandbox-created, on the licensed name with a trailing dot": {session.InstanceData{ID: "t1", Title: "t", CreatedBySandbox: "x"},
			&session.RuntimeMachine{Host: "BOX1.example."}, true},
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPushFixture(t)
			p := pf.placements["me"]
			p.Machine = box1Machine
			pf.placements["me"] = p
			pf.track(t, tc.data)
			if tc.machine != nil {
				pf.placeOn(t, "t", *tc.machine)
			} else {
				pf.placeOn(t, "unrelated", session.RuntimeMachine{})
			}
			code, got, msg := pf.askOwn(t)
			if tc.refused {
				requireRefused(t, code, got, msg, refusePushHostShared)
				assert.Zero(t, pf.tokenReads.Load())
				return
			}
			requireAnswered(t, code, got, msg)
		})
	}
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

func TestCheckHostNamesTheSessionsThatWithholdPushCredentials(t *testing.T) {
	pf := newPushFixture(t)
	child := pf.track(t, session.InstanceData{ID: "child-7", Title: "child", CreatedBySandbox: pf.me.ID})
	legacy := pf.track(t, session.InstanceData{ID: "legacy-3", Title: "legacy"})
	pf.placeOn(t, "child", session.RuntimeMachine{Host: "box1.example"})
	pf.placeOn(t, "legacy", session.RuntimeMachine{SandboxSSH: "ssh freebox"})
	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"box1": {Host: "box1.example", PushCredentials: true},
			"box2": {Host: "box2.example"},
		},
		SandboxSSH: "ssh freebox", SandboxPushCredentials: true,
	}
	resp := &CheckHostResponse{Hosts: []HostCheckResult{{Name: "box1"}, {Name: "box2"}}}
	pf.mgr.fillPushCredentialBlockers(cfg, resp)

	require.Len(t, resp.Hosts[0].PushCredentialBlockers, 1)
	assert.Contains(t, resp.Hosts[0].PushCredentialBlockers[0], `"child"`)
	assert.Contains(t, resp.Hosts[0].PushCredentialBlockers[0], child.ID)
	assert.Contains(t, resp.Hosts[0].PushCredentialBlockers[0], "created by another session's af")
	assert.Empty(t, resp.Hosts[1].PushCredentialBlockers, "a host that is not opted in is not reported")
	require.Len(t, resp.SandboxPushCredentialBlockers, 1)
	assert.Contains(t, resp.SandboxPushCredentialBlockers[0], legacy.ID)
	assert.Contains(t, resp.SandboxPushCredentialBlockers[0], "unknown provenance")
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

// --- third review of #6433: B, an in-flight create ----------------------------

// A create the operator did not place runs its hooks on its machine before its
// row is tracked: from before provisioning until then, that machine is held.
func TestGitCredential_WithheldWhileACreateTheOperatorDidNotPlaceProvisions(t *testing.T) {
	pf := newPushFixture(t)
	planned := session.RuntimeMachine{Host: "box1.example"}
	prev := plannedRuntimeMachine
	plannedRuntimeMachine = func(session.InstanceOptions, string) (session.RuntimeMachine, bool) { return planned, true }
	t.Cleanup(func() { plannedRuntimeMachine = prev })

	release := pf.mgr.holdInflightCreate("child-9", "child", session.InstanceOptions{}, pf.me.Path, true)
	code, got, msg := pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostProvisioning)
	release()
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)

	planned = session.RuntimeMachine{Host: "box1.example", RepoChosen: true}
	release = pf.mgr.holdInflightCreate("repo-1", "repo", session.InstanceOptions{}, pf.me.Path, false)
	code, got, msg = pf.askOwn(t)
	requireRefused(t, code, got, msg, refusePushHostProvisioning)
	release()

	// An operator create onto a placement the operator chose is not held, and one
	// onto another machine holds nothing here.
	planned = session.RuntimeMachine{Host: "box1.example"}
	release = pf.mgr.holdInflightCreate("op-1", "op", session.InstanceOptions{}, pf.me.Path, false)
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)
	release()
	planned = session.RuntimeMachine{Host: "box9.example", RepoChosen: true}
	release = pf.mgr.holdInflightCreate("repo-2", "repo2", session.InstanceOptions{}, pf.me.Path, false)
	code, got, msg = pf.askOwn(t)
	requireAnswered(t, code, got, msg)
	release()
}

// And CreateSession holds it across provisioning: a credential asked for while
// the backend is being provisioned is refused, and answered once the create is
// over.
func TestCreateSessionHoldsItsMachineWhileItProvisions(t *testing.T) {
	pf := newPushFixture(t)
	prev := plannedRuntimeMachine
	plannedRuntimeMachine = func(session.InstanceOptions, string) (session.RuntimeMachine, bool) {
		return session.RuntimeMachine{Host: "box1.example", RepoChosen: true}, true
	}
	t.Cleanup(func() { plannedRuntimeMachine = prev })
	var during string
	var provisioned bool
	t.Cleanup(session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		provisioned = true
		_, _, during = pf.askOwn(t)
		return nil, errors.New("provisioning stopped by the test")
	}))
	_, err := pf.mgr.CreateSession(context.Background(), CreateSessionRequest{Title: "repo-target", RepoPath: pf.me.Path, Program: "claude"})
	require.Error(t, err)
	require.True(t, provisioned, "the create reached provisioning")
	assert.Contains(t, during, refusePushHostProvisioning, "a credential asked for mid-provision")
	assert.Empty(t, pf.mgr.pushCreds.inflightCreates(), "released once the create is over")
	code, got, msg := pf.askOwn(t)
	requireAnswered(t, code, got, msg)
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

// Codex P1 on #6433 at 3abfd87: a create that finishes in the middle of the
// blocker scan must not vanish from both reads. A repo-target create starts while
// the token is read; between the second pass's two reads it commits its row and
// releases its hold. The second pass must still see it.
func TestGitCredential_ACreateThatFinishesMidScanIsStillSeen(t *testing.T) {
	pf := newPushFixture(t)
	repoTarget := session.RuntimeMachine{Host: "box1.example", RepoChosen: true}
	prevPlan := plannedRuntimeMachine
	plannedRuntimeMachine = func(session.InstanceOptions, string) (session.RuntimeMachine, bool) { return repoTarget, true }
	t.Cleanup(func() { plannedRuntimeMachine = prevPlan })

	var release func()
	pf.onTokenRead = func() {
		release = pf.mgr.holdInflightCreate("repo-c1", "repo", session.InstanceOptions{}, pf.me.Path, false)
	}
	committed := false
	prevHook := testHookBetweenBlockerReads
	testHookBetweenBlockerReads = func() {
		if release == nil || committed {
			return
		}
		committed = true
		pf.track(t, session.InstanceData{ID: "repo-c1", Title: "repo", CreatedByOperator: true})
		pf.placeOn(t, "repo", repoTarget)
		release() // the create's deferred release, after its row is committed
	}
	t.Cleanup(func() { testHookBetweenBlockerReads = prevHook })

	code, got, msg := pf.askOwn(t)
	require.True(t, committed, "the create finished in the middle of the second pass's scan")
	requireRefused(t, code, got, msg, refusePushHostShared)
}

// --- fourth review of #6433, 3: every refusal and report names the real cause ---

func TestPushRefusalsAndHostCheckNameTheRealCause(t *testing.T) {
	t.Run("the licensed host's own name doesn't resolve", func(t *testing.T) {
		pf := newPushFixture(t)
		p := pf.placements["me"]
		p.Machine = session.RuntimeMachine{Host: "nowhere.example"}
		pf.placements["me"] = p
		code, got, msg := pf.askOwn(t)
		requireRefused(t, code, got, msg, refusePushHostUnproven)

		resp := &CheckHostResponse{Hosts: []HostCheckResult{{Name: "box1"}}}
		pf.mgr.fillPushCredentialBlockers(&config.Config{Hosts: map[string]config.HostConfig{
			"box1": {Host: "nowhere.example", PushCredentials: true}}}, resp)
		require.Len(t, resp.Hosts[0].PushCredentialBlockers, 1)
		assert.Contains(t, resp.Hosts[0].PushCredentialBlockers[0], "can't establish which machine this is")
		assert.Contains(t, resp.Hosts[0].PushCredentialBlockers[0], "nowhere.example doesn't resolve")
		assert.NotContains(t, resp.Hosts[0].PushCredentialBlockers[0], "sessions kill", "no session is to blame")
	})
	for name, tc := range map[string]struct {
		machine     session.RuntimeMachine
		refusal     string
		cause       string
		remedy      string
		mustNotTell string
	}{
		"a row on a name that doesn't resolve": {session.RuntimeMachine{Host: "nowhere.example"}, refusePushHostShared,
			"nowhere.example doesn't resolve", "make its host name resolve", ""},
		"a row being re-provisioned": {session.InFlightRuntimeMachineForTest(false), refusePushHostProvisioning,
			"being re-provisioned", "wait for it to finish", "sessions kill"},
		"a row on the machine": {session.RuntimeMachine{Host: "box1.example"}, refusePushHostShared,
			"created by another session's af", "af sessions kill", ""},
	} {
		t.Run(name, func(t *testing.T) {
			pf := newPushFixture(t)
			pf.track(t, session.InstanceData{ID: "t1", Title: "t", CreatedBySandbox: "x"})
			pf.placeOn(t, "t", tc.machine)
			code, got, msg := pf.askOwn(t)
			requireRefused(t, code, got, msg, tc.refusal)

			resp := &CheckHostResponse{Hosts: []HostCheckResult{{Name: "box1"}}}
			pf.mgr.fillPushCredentialBlockers(pf.mgr.Config(), resp)
			require.Len(t, resp.Hosts[0].PushCredentialBlockers, 1)
			text := resp.Hosts[0].PushCredentialBlockers[0]
			assert.Contains(t, text, tc.cause)
			assert.Contains(t, text, tc.remedy)
			if tc.mustNotTell != "" {
				assert.NotContains(t, text, tc.mustNotTell, "don't tell the operator to kill a row that isn't the problem")
			}
		})
	}
	t.Run("a create still provisioning", func(t *testing.T) {
		pf := newPushFixture(t)
		prev := plannedRuntimeMachine
		plannedRuntimeMachine = func(session.InstanceOptions, string) (session.RuntimeMachine, bool) {
			return session.RuntimeMachine{Host: "box1.example", RepoChosen: true}, true
		}
		t.Cleanup(func() { plannedRuntimeMachine = prev })
		release := pf.mgr.holdInflightCreate("c1", "repo", session.InstanceOptions{}, pf.me.Path, false)
		defer release()
		resp := &CheckHostResponse{Hosts: []HostCheckResult{{Name: "box1"}}}
		pf.mgr.fillPushCredentialBlockers(pf.mgr.Config(), resp)
		require.Len(t, resp.Hosts[0].PushCredentialBlockers, 1)
		assert.Contains(t, resp.Hosts[0].PushCredentialBlockers[0], "wait for it to finish")
		assert.NotContains(t, resp.Hosts[0].PushCredentialBlockers[0], "sessions kill")
	})
}
