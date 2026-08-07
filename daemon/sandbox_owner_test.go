package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The owner binding (#3056): a sandbox callback credential authorizes only what
// belongs to the session that owns it.

// THE STRUCTURAL GUARD. Every route in the sandbox scope must have a handler-side
// owner constraint, and every registered constraint must name a route that is
// actually in the scope.
//
// This is the check that replaces a rule written in prose. #3012's scope was
// governed by exactly such a rule and drifted from it four separate times —
// CreateSession, the repo-path oracles, the path enumerations, the parameterless
// collision oracle — each time because a route was judged against the rule by a
// reader rather than by a test.
func TestEverySandboxRouteEnforcesAnOwnerConstraint(t *testing.T) {
	require.NotEmpty(t, sandboxAllowedPaths,
		"an empty scope makes this test vacuous; if the scope is deliberately empty again, delete the routes from sandboxConstrainedRoutes too and say so here")

	for path := range sandboxAllowedPaths {
		constraint, ok := sandboxConstrainedRoutes[path]
		assert.Truef(t, ok,
			"%s is admitted to the sandbox scope but no handler-side owner constraint is registered for it. "+
				"Admission is not authorization: give the handler a constraint that narrows to sandboxOwner(ctx), "+
				"then register it in sandboxConstrainedRoutes. A route admitted without one is a boundary that "+
				"reads as enforced and is not.", path)
		assert.NotEmptyf(t, constraint, "%s registers an empty constraint description", path)
	}
	for path := range sandboxConstrainedRoutes {
		assert.Truef(t, sandboxAllowedPaths[path],
			"%s registers an owner constraint but is not in the sandbox scope. Either the route was withdrawn and "+
				"this entry is stale, or the opt-in was dropped by accident and the constraint is now dead code.", path)
	}
}

func TestSandboxOwner_AbsentMeansOperatorNotEmptySandbox(t *testing.T) {
	// A bare context is the operator (unix socket, or the operator's own token):
	// unconstrained, and the bool is what says so.
	_, isSandbox := sandboxOwner(context.Background())
	assert.False(t, isSandbox, "a request with no sandbox owner carries operator authority and must not be narrowed")

	owner, isSandbox := sandboxOwner(withSandboxOwner(context.Background(), "sess-a"))
	assert.True(t, isSandbox)
	assert.Equal(t, "sess-a", owner)

	// An empty id must never read as a sandbox: the whole binding treats "no owner"
	// as the operator, so an empty one would widen rather than narrow.
	_, isSandbox = sandboxOwner(withSandboxOwner(context.Background(), ""))
	assert.False(t, isSandbox, "an empty owner must not present as a constrained sandbox caller")
}

func TestAuthGate_BindsTheCredentialToItsOwningSession(t *testing.T) {
	var registry sandboxTokenRegistry
	secretA, err := registry.mint("sess-a")
	require.NoError(t, err)
	secretB, err := registry.mint("sess-b")
	require.NoError(t, err)

	gate := &authGate{
		expectedToken: func() (string, error) { return "operator-token", nil },
		sandboxTokens: &registry,
	}
	request := func(token string) *http.Request {
		r := mustRequest(t, http.MethodPost, "/v1/Snapshot")
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}

	owner, ok := gate.authorize(request(secretA))
	require.True(t, ok)
	assert.Equal(t, "sess-a", owner, "the gate must report WHICH session's credential admitted the request")

	owner, ok = gate.authorize(request(secretB))
	require.True(t, ok)
	assert.Equal(t, "sess-b", owner, "two sandboxes must not resolve to the same owner")

	// The operator's token authorizes with NO owner — that absence is what keeps
	// the operator unconstrained downstream.
	owner, ok = gate.authorize(request("operator-token"))
	require.True(t, ok)
	assert.Empty(t, owner, "the operator's own token must carry no sandbox owner, or it would be narrowed like one")
}

// THE ACCEPTANCE TEST for #3056, driven through the real mux rather than by
// calling the handler: a credential minted for session A must not be able to see
// session B.
//
// It goes through newHTTPMux + the real gate on purpose. The binding is a chain —
// registry to gate to request context to handler — and every link is somewhere a
// regression would silently restore full visibility while every unit test still
// passed. Calling s.snapshot with a hand-built context would prove the filter and
// none of the wiring.
func TestSnapshot_SandboxCredentialSeesOnlyItsOwnSession(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	mk := func(title string) *session.Instance {
		inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
		require.NoError(t, err)
		inst.SetStartedForTest(true)
		inst.SetStatusForTest(session.Ready)
		seedDiskInstance(t, repoID, title, repoPath)
		manager.mu.Lock()
		manager.instances[daemonInstanceKey(repoID, title)] = inst
		manager.mu.Unlock()
		return inst
	}
	mine := mk("mine")
	theirs := mk("theirs")
	require.NotEqual(t, mine.ID, theirs.ID)

	secret, err := manager.sandboxTokens.mint(mine.ID)
	require.NoError(t, err)

	mux := newHTTPMux(&controlServer{manager: manager})
	gated := withAuth(mux, &authGate{
		expectedToken: func() (string, error) { return "operator-token", nil },
		sandboxTokens: &manager.sandboxTokens,
	}, nil)

	snapshotAs := func(token string) SnapshotResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/Snapshot", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		gated.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var env struct {
			Data SnapshotResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "body: %s", rec.Body.String())
		return env.Data
	}

	// The operator sees everything. Asserted FIRST and not as a courtesy: it is what
	// proves the filter below narrowed something, rather than the fixture being
	// empty or the mux answering an error the assertion would not notice.
	all := snapshotAs("operator-token")
	titles := map[string]bool{}
	for _, d := range all.Instances {
		titles[d.Title] = true
	}
	require.True(t, titles["mine"] && titles["theirs"],
		"the operator must see both sessions, or this test cannot show the sandbox saw fewer; got %v", titles)

	// The sandbox sees exactly its own.
	own := snapshotAs(secret)
	require.Len(t, own.Instances, 1,
		"a sandbox credential must see ONLY its own session; it saw %d", len(own.Instances))
	assert.Equal(t, mine.ID, own.Instances[0].ID)
	assert.Equal(t, "mine", own.Instances[0].Title)
	for _, d := range own.Instances {
		assert.NotEqual(t, theirs.ID, d.ID, "session B leaked into session A's snapshot")
	}
	assert.Empty(t, own.DeliveryAlarms,
		"delivery alarms name their target by TITLE, so they are withheld from a sandbox entirely rather than filtered by an id-to-title mapping")
}
