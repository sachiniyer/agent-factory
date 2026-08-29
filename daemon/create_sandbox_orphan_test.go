package daemon

import (
	"context"
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// TestCreateSession_UnconfirmedSandboxTeardown_RetainsAReapableRow is #3480 at
// the layer where the harm was: a create that provisioned a sandbox and could not
// confirm it torn down used to return a bare error, releasing the title over a
// container/remote workspace that no record pointed at and no retry could reach.
//
// It must instead leave a TOMBSTONED row. That is not new machinery — it is the
// same keepFailedCreate the failed-Start branch uses, which is what makes
// SaveInstances retain the row and routes it to finishUserKill on every poll.
//
// SCOPE, deliberately: this covers the daemon's half — the row exists, is
// tombstoned, keeps the announced identity, and the outcome is reported as
// committed. It does NOT prove the retained handle is reapable across a restart,
// because the fake backend here carries no durable cleanup data. That half is
// proved in session's TestDefaultBackendFactory_… /
// TestNewInstance_ClientBuildFailure… , which round-trip a real sandbox backend's
// teardown identity through ForStorage and back via FromInstanceData.
func TestCreateSession_UnconfirmedSandboxTeardown_RetainsAReapableRow(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	teardowns := 0
	// The full-ProvisionResult seam, not SetBackendFactoryForTest: the fields this
	// test turns on — a bad endpoint and a teardown that cannot confirm — are
	// exactly the ones the Backend-only seam cannot express. It also keeps the test
	// off runtime/config resolution, so it exercises the failure exit rather than a
	// backend-selection precondition.
	restore := session.SetProvisionResultFactoryForTest(
		func(session.InstanceOptions, string, session.BackendKind) (session.ProvisionResult, error) {
			return session.ProvisionResult{
				Backend:  session.NewFakeBackend(),
				Endpoint: &session.AgentServerEndpoint{URL: "://invalid", Token: "tok"},
				Teardown: func() error {
					teardowns++
					return fmt.Errorf("%w: cleanup timed out", session.ErrWorkspaceStateUnknown)
				},
			}, nil
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

	created, createErr := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "orphan",
		RepoPath: repoPath,
		Program:  "claude",
	})
	if createErr == nil {
		t.Fatal("CreateSession reported success though the endpoint could not be wired")
	}
	// Reported on every failing assertion below: without it a create refused by an
	// unrelated precondition is indistinguishable from one that reached the exit
	// under test, and the failure output says nothing about which happened.
	if teardowns != 1 {
		t.Fatalf("teardown attempts = %d, want 1: the create did not reach the post-provision failure exit; createErr = %v", teardowns, createErr)
	}

	rec := recordFor(t, repo.ID, "orphan")
	if rec == nil {
		t.Fatalf("an unconfirmed sandbox teardown left NO record: nothing holds the title and nothing can ever reap the sandbox; createErr = %v", createErr)
	}
	if !rec.UserKilled {
		t.Fatalf("the retained row must be tombstoned, or the next wholesale checkpoint drops it and the poll never finishes its teardown; createErr = %v", createErr)
	}
	if !rec.RuntimeCleanupStateUnknown {
		t.Fatalf("the row must record that the cleanup outcome was never established; createErr = %v", createErr)
	}

	// COMMITTED on the wire (#3233): the row is durable and holds the title, so a
	// plain error would invite an immediate retry against a name this record owns.
	if !isMutationCommitted(createErr) {
		t.Fatalf("CreateSession error = %T %v, want a committed-mutation marker", createErr, createErr)
	}
	// One identity, not two: the retained row must keep the id the create was
	// announced under so clients upsert the pending row instead of seeing a second.
	if created.ID != rec.ID || created.Title != "orphan" {
		t.Fatalf("CreateSession identity = {%s %s}, want the retained row's {%s orphan}", created.ID, created.Title, rec.ID)
	}
}
