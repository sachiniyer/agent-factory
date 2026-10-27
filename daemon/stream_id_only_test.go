package daemon

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The by=id stream address (#4760 review). The TUI dials a session it captured
// by stable id. If that session is deleted before the dial — a deferred attach
// whose help modal was still open, a pane reconnecting after a kill — the id is
// stale. The unscoped shape then falls back to a TITLE lookup, and a different
// session whose title spells those bytes would receive the user's keystrokes.
// by=id says the segment is an id and nothing else, so a miss is a 404.

// seedStaleIDDecoy registers a live session whose TITLE is the stale id's bytes,
// with no session anywhere holding that id.
func seedStaleIDDecoy(t *testing.T) (mux *http.ServeMux, staleID, decoyID string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	staleID = "deleted-session-id"
	decoy := startedLocalTabInstance(t, manager, repo.ID, repoPath, staleID, "af_decoy_agent")
	if decoy.ID == "" || decoy.ID == staleID {
		t.Fatalf("precondition: the decoy needs its own stable id, got %q", decoy.ID)
	}
	return newHTTPMux(&controlServer{manager: manager}), staleID, decoy.ID
}

// TestStreamIDOnly_StaleIDIsRefusedNotReadAsTitle is the fail-first. Before the
// fix the by= parameter was ignored, the stale id fell back to the decoy's title,
// and both routes answered for the decoy.
func TestStreamIDOnly_StaleIDIsRefusedNotReadAsTitle(t *testing.T) {
	mux, staleID, _ := seedStaleIDDecoy(t)

	// Control: the legacy unscoped shape (Web's) still reaches the decoy through
	// title fallback. Without this the 404s below could come from a fixture the
	// resolver cannot see at all.
	if rec := streamGet(t, mux, "/v1/sessions/"+staleID+"/stream-info"); rec.Code != http.StatusOK {
		t.Fatalf("control: unscoped stream-info = %d, want 200 via title fallback (body: %s)", rec.Code, rec.Body.String())
	}

	for _, route := range []string{"stream-info", "stream"} {
		rec := streamGet(t, mux, fmt.Sprintf("/v1/sessions/%s/%s?by=id", staleID, route))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s?by=id for a stale id = %d, want 404 — the id must not be reread as another session's title (body: %s)",
				route, rec.Code, rec.Body.String())
		}
	}
}

// TestStreamIDOnly_LiveIDResolvesAndStreamInfoKeepsTheShape proves by=id is a
// narrowing, not a new refusal: the decoy's own id resolves, and the stream URL
// stream-info hands back carries by=id so the follow-up dial keeps the id-only
// contract.
func TestStreamIDOnly_LiveIDResolvesAndStreamInfoKeepsTheShape(t *testing.T) {
	mux, _, decoyID := seedStaleIDDecoy(t)

	rec := streamGet(t, mux, "/v1/sessions/"+decoyID+"/stream-info?by=id")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream-info?by=id for a live id = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if want := "/v1/sessions/" + decoyID + "/stream?by=id"; !contains(rec.Body.String(), want) {
		t.Fatalf("stream-info body = %s, want the id-only stream URL %q", rec.Body.String(), want)
	}
}

// TestStreamIDOnly_RejectsContradictoryOrUnknownShapes pins the two malformed
// requests: by=id with a repo_id names both namespaces at once, and an unknown
// by= value names neither. Both are a 400, never a guess.
func TestStreamIDOnly_RejectsContradictoryOrUnknownShapes(t *testing.T) {
	mux, _, decoyID := seedStaleIDDecoy(t)
	for _, query := range []string{"by=id&repo_id=some-repo", "by=title"} {
		for _, route := range []string{"stream-info", "stream"} {
			rec := streamGet(t, mux, fmt.Sprintf("/v1/sessions/%s/%s?%s", decoyID, route, query))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s?%s = %d, want 400 (body: %s)", route, query, rec.Code, rec.Body.String())
			}
		}
	}
}
