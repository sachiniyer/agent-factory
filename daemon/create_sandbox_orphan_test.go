package daemon

import (
	"context"
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// sandboxOrphanRuntime provisions a sandbox whose endpoint cannot be wired and
// whose teardown then cannot establish whether the sandbox is gone — the exact
// window #3480 is about.
type sandboxOrphanRuntime struct{ teardowns *int }

func (r sandboxOrphanRuntime) Provision(session.ProvisionSpec) (session.ProvisionResult, error) {
	return session.ProvisionResult{
		Backend:  session.NewFakeBackend(),
		Endpoint: &session.AgentServerEndpoint{URL: "://invalid", Token: "tok"},
		Teardown: func() error {
			*r.teardowns++
			return fmt.Errorf("%w: cleanup timed out", session.ErrWorkspaceStateUnknown)
		},
	}, nil
}

// TestCreateSession_UnconfirmedSandboxTeardown_RetainsAReapableRow is #3480 at
// the layer where the harm was: a create that provisioned a sandbox and could not
// confirm it torn down used to return a bare error, releasing the title over a
// container/remote workspace that no record pointed at and no retry could reach.
//
// It must instead leave a TOMBSTONED row. That is not new machinery: it is the
// same keepFailedCreate the failed-Start branch uses, which is what makes
// SaveInstances retain the row and routes it to finishUserKill on every poll.
func TestCreateSession_UnconfirmedSandboxTeardown_RetainsAReapableRow(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	teardowns := 0
	restoreRuntime := session.SetRuntimeForTest(session.BackendSandbox, func() session.Runtime {
		return sandboxOrphanRuntime{teardowns: &teardowns}
	})
	t.Cleanup(restoreRuntime)

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
		Backend:  "sandbox",
	})
	if createErr == nil {
		t.Fatal("CreateSession reported success though the endpoint could not be wired")
	}
	if teardowns != 1 {
		t.Fatalf("teardown attempts = %d, want 1: the unusable sandbox must still be reaped", teardowns)
	}

	rec := recordFor(t, repo.ID, "orphan")
	if rec == nil {
		t.Fatal("an unconfirmed sandbox teardown left NO record: nothing holds the title and nothing can ever reap the sandbox")
	}
	if !rec.UserKilled {
		t.Fatal("the retained row must be tombstoned, or the next wholesale checkpoint drops it and the poll never finishes its teardown")
	}
	if !rec.RuntimeCleanupStateUnknown {
		t.Fatal("the row must record that the cleanup outcome was never established")
	}

	// COMMITTED on the wire (#3233): the row is durable and holds the title, so a
	// plain error would invite an immediate retry against a name this record owns.
	if !isMutationCommitted(createErr) {
		t.Fatalf("CreateSession error = %T %v, want a committed-mutation marker", createErr, createErr)
	}
	if created.ID != rec.ID || created.Title != "orphan" {
		t.Fatalf("CreateSession identity = {%s %s}, want the retained row's {%s orphan}", created.ID, created.Title, rec.ID)
	}
}
