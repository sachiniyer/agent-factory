package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// newUnsafeTeardownKillFixture builds a started session whose teardown can never
// complete safely — the pane's liveness is never established, so KillSession
// durably tombstones the record, fails the teardown, and retains the row for the
// poll's automatic retry. That is the central reachable post-commit failure of
// #3234.
func newUnsafeTeardownKillFixture(t *testing.T, title string) (*Manager, string, session.InstanceData) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	restore := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		return unsafeTeardownBackend{readyFakeBackend{fake}}, nil
	})
	t.Cleanup(restore)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	created, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return manager, repo.ID, created
}

// TestKillSession_UnsafeTeardownAfterTombstone_ReportsCommittedMutation is
// #3234: KillSession names the successful tombstone write as its commit point,
// so every later error means the kill intent is already durable — here the
// teardown fails with an unknown outcome and the tombstoned row is deliberately
// retained for the automatic retry. Returning that as a plain error tells every
// control/HTTP client failed-nothing-committed, the exact ambiguity
// mutationCommittedError exists to remove.
func TestKillSession_UnsafeTeardownAfterTombstone_ReportsCommittedMutation(t *testing.T) {
	manager, repoID, created := newUnsafeTeardownKillFixture(t, "committed-kill")

	killed, err := manager.KillSession(KillSessionRequest{Title: "committed-kill", RepoID: repoID})
	if err == nil {
		t.Fatal("expected KillSession to surface the failing teardown")
	}
	if !errors.Is(err, session.ErrPaneMayBeLive) {
		t.Fatalf("the error must keep its unknown-teardown cause, got: %v", err)
	}
	if !isMutationCommitted(err) {
		t.Fatalf("KillSession error = %T %v, want a committed-mutation marker: the tombstone is durable, so this must not read as an untouched, freely retryable failure", err, err)
	}
	if killed.ID != created.ID || killed.Title != created.Title {
		t.Fatalf("KillSession identity = %+v, want the resolved {%s %s} preserved on the committed path", killed, created.ID, created.Title)
	}

	// The marker must describe reality: the tombstoned record stays for the retry.
	rec := recordFor(t, repoID, "committed-kill")
	if rec == nil || !rec.UserKilled {
		t.Fatalf("the retained record must survive with its tombstone, got %+v", rec)
	}
}

// TestControlKillSession_CommittedTeardownFailure_FillsEnvelopeWithoutKilledEvent
// pins the handler half of #3234: a committed-but-unfinished kill must be
// recorded in the response envelope (net/rpc flattens a returned error to a
// string, which reads as failed-nothing-committed) — and it must NOT publish
// session.killed, because that event means the durable row and authoritative map
// entry disappeared, which is exactly what did not happen; finishUserKill
// publishes it when the row actually goes.
func TestControlKillSession_CommittedTeardownFailure_FillsEnvelopeWithoutKilledEvent(t *testing.T) {
	manager, repoID, _ := newUnsafeTeardownKillFixture(t, "committed-envelope")
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()
	var resp KillSessionResponse
	if err := cs.KillSession(KillSessionRequest{Title: "committed-envelope", RepoID: repoID}, &resp); err != nil {
		t.Fatalf("a committed kill must land in the envelope, not be returned as an rpc error: %v", err)
	}
	if !resp.OK {
		t.Fatal("resp.OK = false, want true: the mutation committed")
	}
	if resp.MutationOutcome.Code != apiproto.ErrorCodeMutationCommitted {
		t.Fatalf("resp code = %q, want %q", resp.MutationOutcome.Code, apiproto.ErrorCodeMutationCommitted)
	}
	if !strings.Contains(resp.MutationOutcome.Warning, "retried automatically") {
		t.Fatalf("the warning must carry the teardown failure's own text, got %q", resp.MutationOutcome.Warning)
	}

	// publish is synchronous, so anything the handler emitted is already queued.
	for {
		select {
		case ev := <-ch:
			if ev.Type == agentproto.EventSessionKilled {
				t.Fatal("the handler published session.killed for a kill whose row was retained: clients drop the row while its record still exists, and the poll's finishUserKill publishes a second killed event when the row actually goes")
			}
		default:
			return
		}
	}
}
