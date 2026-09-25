package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rebindConflictFixture registers one project and returns the control server,
// its id, the root it was registered at, and two replacement checkouts.
func rebindConflictFixture(t *testing.T) (cs *controlServer, id, start string, targets [2]string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	start = setupControlRepo(t)
	base := testguard.CanonicalTempDir(t)
	for i, name := range []string{"left", "right"} {
		targets[i] = filepath.Join(base, name)
		cloneRepoTemplate(t, targets[i])
	}
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs = &controlServer{manager: manager}
	var reg RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: start}, &reg))
	return cs, reg.Project.ID, reg.Project.Root, targets
}

func registryRoot(t *testing.T, id string) string {
	t.Helper()
	projects, err := config.ListProjects()
	require.NoError(t, err)
	for _, p := range projects {
		if p.ID == id {
			return p.Root
		}
	}
	t.Fatalf("project %s is not registered", id)
	return ""
}

// TestControlServer_RebindProject_ConcurrentRebindsFromOneRootExactlyOneWins
// pins #4822: two clients that both saw the project at the same root rebind it
// at once. Exactly one applies; the other is refused as rebound elsewhere,
// naming the winner's root, instead of silently overwriting it.
func TestControlServer_RebindProject_ConcurrentRebindsFromOneRootExactlyOneWins(t *testing.T) {
	cs, id, start, targets := rebindConflictFixture(t)

	var wg sync.WaitGroup
	errs := make([]error, len(targets))
	gate := make(chan struct{})
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			var resp RebindProjectResponse
			errs[i] = cs.RebindProject(RebindProjectRequest{ID: id, Path: target, ExpectedRoot: start}, &resp)
		}()
	}
	close(gate)
	wg.Wait()

	winner, loser := -1, -1
	for i, err := range errs {
		if err == nil {
			winner = i
		} else {
			loser = i
		}
	}
	require.NotEqual(t, -1, winner, "one rebind must apply: %v", errs)
	require.NotEqual(t, -1, loser, "exactly one rebind may apply from the same observed root")
	var rebound *projectReboundError
	require.True(t, errors.As(errs[loser], &rebound), "the loser is refused as rebound elsewhere, got %v", errs[loser])
	assert.Contains(t, errs[loser].Error(), filepath.Clean(targets[winner]), "the refusal names the root the project is bound to now")
	assert.Equal(t, filepath.Clean(targets[winner]), registryRoot(t, id))
}

// TestControlServer_RebindProject_RefusesStaleExpectedRoot: a client whose
// view predates another rebind is refused and the registry is left alone.
func TestControlServer_RebindProject_RefusesStaleExpectedRoot(t *testing.T) {
	cs, id, start, targets := rebindConflictFixture(t)
	var resp RebindProjectResponse
	require.NoError(t, cs.RebindProject(RebindProjectRequest{ID: id, Path: targets[0]}, &resp))

	_, ch := cs.manager.events.subscribe()
	err := cs.RebindProject(RebindProjectRequest{ID: id, Path: targets[1], ExpectedRoot: start}, &resp)

	var rebound *projectReboundError
	require.True(t, errors.As(err, &rebound), "a stale client must be refused, got %v", err)
	assert.Equal(t, filepath.Clean(targets[0]), registryRoot(t, id), "a refused rebind writes nothing")
	assertNoEvent(t, ch, agentproto.EventProjectsChanged)
}

// TestControlServer_RebindProject_OmittedExpectedRootStillApplies: a client
// that predates expected_root keeps last-writer-wins — absence is never a
// refusal.
func TestControlServer_RebindProject_OmittedExpectedRootStillApplies(t *testing.T) {
	cs, id, _, targets := rebindConflictFixture(t)
	var resp RebindProjectResponse
	require.NoError(t, cs.RebindProject(RebindProjectRequest{ID: id, Path: targets[0]}, &resp))
	require.NoError(t, cs.RebindProject(RebindProjectRequest{ID: id, Path: targets[1]}, &resp))
	assert.Equal(t, filepath.Clean(targets[1]), registryRoot(t, id))
}

// TestHTTPRebindProject_ConflictIs409WithCode pins the public wire shape of the
// refusal: 409, error code project_rebound, marked as a daemon rejection — and
// that a body with no expected_root at all (an old client) still applies.
func TestHTTPRebindProject_ConflictIs409WithCode(t *testing.T) {
	cs, id, start, targets := rebindConflictFixture(t)
	handler := rpcHandler(cs.RebindProject)
	post := func(body map[string]string) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/v1/RebindProject", strings.NewReader(string(raw))))
		return rec
	}

	legacy := post(map[string]string{"id": id, "path": targets[0]})
	require.Equal(t, http.StatusOK, legacy.Code, "an omitted expected_root is applied: %s", legacy.Body.String())

	stale := post(map[string]string{"id": id, "path": targets[1], "expected_root": start})
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	var env struct {
		Data  any                    `json:"data"`
		Error apiproto.EnvelopeError `json:"error"`
	}
	require.NoError(t, json.Unmarshal(stale.Body.Bytes(), &env))
	assert.Equal(t, apiproto.ErrorCodeProjectRebound, env.Error.Code)
	assert.True(t, env.Error.DaemonRejected, "a conflict is a definitive daemon refusal, not an unknown outcome")
	assert.Contains(t, env.Error.Message, "refresh and retry")
	assert.Equal(t, filepath.Clean(targets[0]), registryRoot(t, id))
}
