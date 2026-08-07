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
	// ErrPaneMayBeLive/ErrWorkspaceStateUnknown. The sandbox reap SUCCEEDS, so
	// errors.Join(killErr, teardown()) collapses to exactly killErr. That is the
	// input KillSession misclassified.
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
	// The reap is INSTALLED, and that is the #3042 fix to this test rather than a
	// convenience. Every assertion below is about a session whose sandbox "WAS
	// reaped" — the name says so, the error message it rejects says so — and with a
	// nil teardown none of them could see it. A regression that stopped reaping on
	// the kill path kept this green while leaving a container running for every
	// remote session the user ever killed. Installing it also makes the premise
	// above true by construction instead of by absence: the joined error collapses
	// to killErr because the reap SUCCEEDED, not because there was none.
	_, _, reap := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "remote-reaped", srv.URL, session.Running)
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

	// THE EFFECT, asserted before anything about records or messages: the sandbox is
	// physically gone. This runs through the same ProvisionResult.Teardown field
	// docker populates with `docker rm -f`, so it is the reap production performs and
	// not a restatement of the REST call that triggers it (#3042).
	//
	// Exactly once. A second release is not harmless here even though the runtimes
	// make teardown idempotent: it means two code paths each believe they own this
	// runtime's lifetime, and the next one to be given a REPLACEMENT sandbox's handle
	// reaps that instead — which is how a live session's container disappears under it.
	if got := reap.count(); got != 1 {
		t.Fatalf("physical sandbox reaps = %d, want exactly 1: the /v1/agent/kill REST call failing is "+
			"precisely when the reap matters most (the container must not leak because its agent-server "+
			"was already down), and a sandbox left alive is a VM still billing that no session record "+
			"points at, so nothing will ever clean it up", got)
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
