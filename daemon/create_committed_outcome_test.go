package daemon

import (
	"context"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// newCreateCommittedFixture builds a manager whose backend factory installs the
// given backend and returns the repo context, mirroring the fixtures the
// retained-create regressions already use.
func newCreateCommittedFixture(t *testing.T, backend session.Backend) (*Manager, string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	restore := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		return backend, nil
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
	return manager, repo.ID, repoPath
}

// TestCreateSession_UnknownStart_ReportsCommittedWithRetainedIdentity is #3233:
// a create whose startup outcome is unknown deliberately retains and durably
// records the session/workspace — a COMMITTED outcome, as the branch itself
// says — yet returned a plain error and an empty InstanceData, so every client
// read failed-nothing-committed about a session row that exists.
func TestCreateSession_UnknownStart_ReportsCommittedWithRetainedIdentity(t *testing.T) {
	backend := &unknownStartBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	manager, repoID, repoPath := newCreateCommittedFixture(t, backend)

	created, createErr := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "uncertain-start", RepoPath: repoPath, Program: "claude",
	})
	if createErr == nil {
		t.Fatal("CreateSession reported success though startup state is unknown")
	}
	if !isMutationCommitted(createErr) {
		t.Fatalf("CreateSession error = %T %v, want a committed-mutation marker: the retained row is durable", createErr, createErr)
	}
	rec := recordFor(t, repoID, "uncertain-start")
	if rec == nil {
		t.Fatal("the retained row must exist")
	}
	if created.ID != rec.ID || created.Title != "uncertain-start" {
		t.Fatalf("CreateSession identity = %+v, want the retained row's {%s uncertain-start} preserved", created, rec.ID)
	}
	if created.Worktree.WorktreePath == "" {
		t.Fatalf("the retained workspace path must be preserved for the caller, got %+v", created)
	}
}

// TestCreateSession_UnknownCleanup_ReportsCommittedWithRetainedIdentity covers
// #3233's second branch: startup failed with a KNOWN cause, but the cleanup's
// outcome is unknown, so the tombstoned row is retained for the poll's cleanup
// retry — durable state a plain error hides.
func TestCreateSession_UnknownCleanup_ReportsCommittedWithRetainedIdentity(t *testing.T) {
	backend := unsafeKillBackend{readyFakeBackend{session.NewFakeBackend()}}
	manager, repoID, repoPath := newCreateCommittedFixture(t, backend)

	created, createErr := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "uncertain-cleanup", RepoPath: repoPath, Program: "claude",
	})
	if createErr == nil {
		t.Fatal("CreateSession reported success though its cleanup could not complete safely")
	}
	if !isMutationCommitted(createErr) {
		t.Fatalf("CreateSession error = %T %v, want a committed-mutation marker: the tombstoned row is durable and the daemon retries its cleanup", createErr, createErr)
	}
	rec := recordFor(t, repoID, "uncertain-cleanup")
	if rec == nil || !rec.UserKilled {
		t.Fatalf("the retained row must exist with its cleanup tombstone, got %+v", rec)
	}
	if created.ID != rec.ID || created.Title != "uncertain-cleanup" {
		t.Fatalf("CreateSession identity = %+v, want the retained row's {%s uncertain-cleanup} preserved", created, rec.ID)
	}
}

// TestControlCreateSession_CommittedRetainedCreate_FillsEnvelope pins the
// handler half of #3233: the committed outcome must ride the response envelope
// (net/rpc flattens a returned error to a string) with the retained identity in
// resp.Instance, so CLI/HTTP clients can address the row the create left behind.
func TestControlCreateSession_CommittedRetainedCreate_FillsEnvelope(t *testing.T) {
	backend := &unknownStartBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	manager, repoID, repoPath := newCreateCommittedFixture(t, backend)
	cs := &controlServer{manager: manager}

	var resp CreateSessionResponse
	if err := cs.CreateSession(CreateSessionRequest{
		Title: "uncertain-envelope", RepoPath: repoPath, Program: "claude",
	}, &resp); err != nil {
		t.Fatalf("a committed retained create must land in the envelope, not be returned as an rpc error: %v", err)
	}
	if resp.MutationOutcome.Code != apiproto.ErrorCodeMutationCommitted {
		t.Fatalf("resp code = %q, want %q", resp.MutationOutcome.Code, apiproto.ErrorCodeMutationCommitted)
	}
	if resp.MutationOutcome.Warning == "" {
		t.Fatal("the warning must carry the retained-create explanation")
	}
	rec := recordFor(t, repoID, "uncertain-envelope")
	if rec == nil {
		t.Fatal("the retained row must exist")
	}
	if resp.Instance.ID != rec.ID || resp.Instance.Title != "uncertain-envelope" {
		t.Fatalf("resp.Instance = %+v, want the retained row's identity {%s uncertain-envelope}", resp.Instance, rec.ID)
	}
}
