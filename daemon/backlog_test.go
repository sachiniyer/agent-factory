package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/internal/testguard"

	"github.com/stretchr/testify/require"
)

func newBacklogTestServer(t *testing.T) (*controlServer, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	ready := make(chan struct{})
	close(ready)
	m := &Manager{ready: ready, events: newEventsHub()}
	// A metering pass runs on a background goroutine and resolves the AF home
	// when it writes. Join it before this test's AGENT_FACTORY_HOME is restored
	// (cleanups run last-in-first-out, and the Setenv above registered first), so
	// no pass outlives the test that started it and writes into the next test's
	// home (#6380). Joining, not waiting out a budget, is what makes this hold.
	t.Cleanup(m.stopAndWaitBackgroundMutationsForShutdown)
	return &controlServer{manager: m}, setupControlRepo(t)
}

func nextBacklogChanged(t *testing.T, ch <-chan agentproto.Event) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != agentproto.EventBacklogChanged {
				continue
			}
			var payload map[string]any
			require.NoError(t, json.Unmarshal(ev.Data, &payload))
			return payload
		case <-deadline:
			t.Fatal("no backlog.changed event")
		}
	}
}

func TestBacklogRoutes_AddOverHTTPPublishesChanged(t *testing.T) {
	cs, repoPath := newBacklogTestServer(t)
	id, ch, _ := cs.manager.events.subscribeWithOverflow()
	defer cs.manager.events.unsubscribe(id)

	code, env := postProposalRoute(t, cs, "AddBacklogItems", AddBacklogItemsRequest{
		RepoPath: repoPath,
		Items:    []backlog.ItemInput{{Key: "a", Title: "First"}, {Key: "b", Title: "Second", WaitsOn: []backlog.Ref{{Key: "a"}}}},
	})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, map[string]any{"a": float64(1), "b": float64(2)}, env["data"].(map[string]any)["ids"])

	payload := nextBacklogChanged(t, ch)
	require.Len(t, payload, 2, "backlog.changed carries exactly repo_id and ids")
	require.NotEmpty(t, payload["repo_id"])
	require.Equal(t, []any{float64(1), float64(2)}, payload["ids"])

	code, env = postProposalRoute(t, cs, "GetBacklog", GetBacklogRequest{RepoID: payload["repo_id"].(string)})
	require.Equal(t, http.StatusOK, code, env)
	data := env["data"].(map[string]any)
	require.Len(t, data["backlog"].(map[string]any)["items"], 2)
	require.Equal(t, "draft", data["states"].(map[string]any)["1"].(map[string]any)["state"])
	require.Equal(t, "off", data["runner"].(map[string]any)["state"])
}

func TestBacklogRoutes_NotFoundNamesTheProject(t *testing.T) {
	cs, repoPath := newBacklogTestServer(t)
	code, env := postProposalRoute(t, cs, "SetBacklogItemStatus", SetBacklogItemStatusRequest{
		RepoPath: repoPath, ID: 12, Status: backlog.StatusQueued,
	})
	require.NotEqual(t, http.StatusOK, code, env)
	want := fmt.Sprintf(`B-12 not found in project %s — run "af backlog list --all"`, filepath.Base(repoPath))
	require.Equal(t, want, env["error"].(map[string]any)["message"])
}

func TestBacklogRoutes_SandboxCredentialRefused(t *testing.T) {
	var registry sandboxTokenRegistry
	secret, err := registry.mint("sess-a")
	require.NoError(t, err)
	gate := &authGate{
		expectedToken: func() (string, error) { return "operator-token", nil },
		sandboxTokens: &registry,
	}
	for verb := range backlogMethods {
		path := "/v1/" + verb
		require.Falsef(t, sandboxAllowedPath(path), "%s must not be sandbox-allowed", path)
		r := mustRequest(t, http.MethodPost, path)
		r.Header.Set("Authorization", "Bearer "+secret)
		require.Falsef(t, admits(gate, r), "a sandbox credential must not reach %s", path)
		r = mustRequest(t, http.MethodPost, path)
		r.Header.Set("Authorization", "Bearer operator-token")
		require.Truef(t, admits(gate, r), "the operator token must reach %s", path)
	}
}

func TestBacklogMethodsMatchRoutes(t *testing.T) {
	served := map[string]bool{}
	for _, r := range HTTPRoutes() {
		served[r.Path] = true
	}
	for verb := range backlogMethods {
		require.Truef(t, served["/v1/"+verb], "%s is in the client allowlist but not served", verb)
	}
}
