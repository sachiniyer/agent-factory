package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// TestKillSession_RemoteReapSucceededDeletesRowWithoutMisleadingError is the
// #2017 regression.
//
// A remote (docker/ssh/hook) session is killed after its in-sandbox agent-server
// has already died — the common reason to kill one. remoteAgentServer.Kill then
// joins the failed /v1/agent/kill REST call with the sandbox reap
// (errors.Join(killErr, teardown())); the reap SUCCEEDS, so instance.Kill returns
// a PLAIN endpoint error whose subject is a dead endpoint, not the workspace.
// session.TeardownStateUnknown(err) is therefore false — the workspace is provably
// gone — which is exactly the shape deleteSessionRecord was built to let through
// and delete the row.
//
// The bug: KillSession early-returned on ANY non-nil instance.Kill() error before
// reaching that choke point, so it surfaced "its workspace was left intact; the
// kill is recorded and will be retried automatically" (FALSE — the sandbox WAS
// reaped) and kept the row for a one-poll flicker until finishUserKill deleted it
// anyway. The fix routes the decision through the SAME TeardownStateUnknown
// classifier deleteSessionRecord uses, so only an UNKNOWN-state teardown retains.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: KillSession returns the "workspace left
// intact" error and RETAINS the record on a fully successful reap.
func TestKillSession_RemoteReapSucceededDeletesRowWithoutMisleadingError(t *testing.T) {
	// The in-sandbox agent-server is dead: /v1/agent/kill answers with the error
	// envelope the real agent-server returns for a failed op, so
	// remoteAgentServer.Kill's killErr is a PLAIN REST error, not
	// ErrPaneMayBeLive/ErrWorkspaceStateUnknown. runtimeTeardown is nil in this
	// harness, so the joined error reduces to exactly killErr — the identical shape
	// errors.Join(killErr, teardown()) collapses to when the sandbox reap SUCCEEDS
	// (teardown() == nil). That is the input KillSession misclassified.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/agent/kill" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "in-sandbox agent-server is gone"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	manager, repoID, repoPath := newStatusTestManager(t)
	registerStartedRemote(t, manager, repoID, repoPath, "remote-reaped", srv.URL, session.Running)
	key := daemonInstanceKey(repoID, "remote-reaped")

	killed, err := manager.KillSession(KillSessionRequest{Title: "remote-reaped", RepoID: repoID})
	if err != nil {
		if strings.Contains(err.Error(), "left intact") {
			t.Fatalf("KillSession surfaced the misleading \"workspace left intact\" error on a successful remote reap — the sandbox WAS reaped, only the in-sandbox /kill REST call failed, so the message is false (#2017): %v", err)
		}
		t.Fatalf("KillSession returned an unexpected error on a successful remote reap: %v", err)
	}
	if killed.Title != "remote-reaped" {
		t.Fatalf("killed event resolved the wrong session: got %q, want %q", killed.Title, "remote-reaped")
	}

	// The row MUST be gone with no one-poll flicker: a KNOWN-state teardown (dead
	// endpoint, successful reap) flows through deleteSessionRecord, which logs the
	// cause and deletes the record rather than leaving a tombstone for the next poll.
	if rec := recordFor(t, repoID, "remote-reaped"); rec != nil {
		t.Fatalf("killed remote session's record must be deleted after a successful reap, still present: %+v", rec)
	}
	manager.mu.Lock()
	_, tracked := manager.instances[key]
	manager.mu.Unlock()
	if tracked {
		t.Fatal("killed remote session must be dropped from the manager after a successful reap")
	}
}
