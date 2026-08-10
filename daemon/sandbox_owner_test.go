package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

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

// The owner's OWN row must still not describe the daemon host (#3065 review).
//
// Reflective rather than a list of field assertions, and that is the point: the
// projection is default-deny by construction, so this walks every field of the
// result and fails on any populated one that has not been consciously allowed.
// A field ADDED to InstanceData later is caught the moment it starts arriving —
// which a hand-written "assert Path is empty" would never do.
func TestSandboxSafeInstanceData_WithholdsEverythingNotExplicitlyAllowed(t *testing.T) {
	allowed := map[string]bool{
		"ID": true, "TaskID": true, "Title": true, "Branch": true, "Status": true,
		"Liveness": true, "InFlightOp": true, "LifecycleAction": true,
		"CurrentAgent": true, "Program": true, "BackendType": true,
		"TaskRunActive": true, "LimitResetAt": true, "CreatedAt": true, "UpdatedAt": true,
		"IdleReason": true, "LastPromptAttemptAt": true,
		"LastPromptDeliveryStatus": true, "LastPaneChurnAt": true,
	}

	// Populate EVERY field with a non-zero value, so anything that survives the
	// projection is visible. Strings and bools are enough to make the point; the
	// struct/slice fields are set to non-nil so they too show up as populated.
	full := session.InstanceData{
		ID: "sess-a", TaskID: "task-1", Title: "mine", Branch: "af/mine",
		Status: session.Ready, CurrentAgent: "claude", Program: "claude",
		BackendType: "ssh", TaskRunActive: true, CanKill: true, CanHandoff: true,
		IsRoot: true, UserKilled: true, StartupStateUnknown: true,
		Height: 40, Width: 120,
		IdleReason:               session.IdleReasonDeliveryUnconfirmed,
		LastPromptAttemptAt:      time.Unix(100, 0).UTC(),
		LastPromptDeliveryStatus: session.PromptCouldNotConfirm,
		LastPaneChurnAt:          time.Unix(90, 0).UTC(),
		Path:                     "/home/operator/private/repo",
		TmuxName:                 "af-mine",
		Prompt:                   "the operator's prompt text",
		PendingHandoffMission:    "mission",
		Tabs:                     []session.TabData{{Name: "shell"}},
	}
	full.Worktree.RepoPath = "/home/operator/private/repo"
	full.Worktree.WorktreePath = "/home/operator/private/repo/.af/worktrees/mine"

	got := sandboxSafeInstanceData(full)

	v := reflect.ValueOf(got)
	typ := v.Type()
	require.Greater(t, typ.NumField(), 20, "InstanceData shrank unexpectedly; this guard assumes the wide struct it is protecting")
	var leaked []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if allowed[name] {
			continue
		}
		if !v.Field(i).IsZero() {
			leaked = append(leaked, name)
		}
	}
	assert.Emptyf(t, leaked,
		"sandboxSafeInstanceData passed through %v, which is not in the allowed set. If a new field is genuinely safe "+
			"for a sandbox to learn about its own session, add it to the projection AND to this test's allowed set — "+
			"deliberately. Fields describing the daemon HOST (Path, Worktree, TmuxName) must never be added.", leaked)

	// And the allowed ones actually survive, or the projection would "pass" by
	// returning an empty struct.
	assert.Equal(t, "sess-a", got.ID)
	assert.Equal(t, "mine", got.Title)
	assert.Equal(t, "af/mine", got.Branch)
	assert.Equal(t, session.Ready, got.Status)
	assert.Equal(t, session.IdleReasonDeliveryUnconfirmed, got.IdleReason)
	assert.Equal(t, time.Unix(100, 0).UTC(), got.LastPromptAttemptAt)
	assert.Equal(t, session.PromptCouldNotConfirm, got.LastPromptDeliveryStatus)
	assert.Equal(t, time.Unix(90, 0).UTC(), got.LastPaneChurnAt)
	assert.Empty(t, got.Path, "the host's absolute repo root is the field this projection exists for")
	assert.Empty(t, got.Worktree.WorktreePath, "the worktree carries host paths too, so the whole struct is withheld")
}

// An invalidation anywhere — listener OR auth posture — must fence a mint in
// flight (#3065 review). The listener generation this replaced could not see an
// auth-only change.
func TestSandboxTokenRegistry_InvalidationCountFencesEveryGlobalRevoke(t *testing.T) {
	var r sandboxTokenRegistry
	start := r.invalidationCount()

	// Advances even with an EMPTY registry: a sweep that finds nothing still means
	// an invalidating event happened, and the mint racing it has not registered its
	// token yet — exactly the case a count gated on "something was dropped" misses.
	require.Equal(t, 0, r.revokeAll())
	assert.Equal(t, start+1, r.invalidationCount(),
		"an invalidation with nothing outstanding must still advance the fence, or a mint racing it survives")

	_, err := r.mint("sess-a")
	require.NoError(t, err)
	afterMint := r.invalidationCount()
	assert.Equal(t, start+1, afterMint, "minting is not an invalidation and must not move the fence")

	require.Equal(t, 1, r.revokeAll())
	assert.Equal(t, start+2, r.invalidationCount())

	// A TARGETED revoke is not a global invalidation: one session ending must not
	// fence every other session's in-flight create.
	_, err = r.mint("sess-b")
	require.NoError(t, err)
	before := r.invalidationCount()
	r.revoke("sess-b")
	assert.Equal(t, before, r.invalidationCount(),
		"revoking one session must not advance the global fence, or every teardown would fail unrelated creates")
}
