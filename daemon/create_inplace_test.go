package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// installOptionsRecordingBackend is installInstantBackend plus a recorder: it
// returns a pointer to the slice of InstanceOptions the daemon handed to
// session.NewInstance, so tests can assert request fields (InPlace) survive
// the CreateSession plumbing without spinning up real tmux/git worktrees.
func installOptionsRecordingBackend(t *testing.T) *[]session.InstanceOptions {
	t.Helper()
	var seen []session.InstanceOptions
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		seen = append(seen, opts)
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return readyFakeBackend{backend}, nil
	})
	t.Cleanup(restore)
	return &seen
}

// TestManagerCreateSessionCarriesInPlace verifies the daemon's CreateSession
// contract for `af sessions create --here`: the request's InPlace flag reaches
// session.NewInstance so the instance attaches to the repo working tree
// instead of cutting a fresh worktree.
func TestManagerCreateSessionCarriesInPlace(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "captain-here",
		RepoPath: repoPath,
		Program:  "claude",
		InPlace:  true,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 NewInstance call, got %d", len(*seen))
	}
	if !(*seen)[0].InPlace {
		t.Fatalf("InPlace was dropped between CreateSessionRequest and session.NewInstance")
	}

	// A plain create must stay on the fresh-worktree path.
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "normal",
		RepoPath: repoPath,
		Program:  "claude",
	}); err != nil {
		t.Fatalf("CreateSession (normal): %v", err)
	}
	if (*seen)[1].InPlace {
		t.Fatalf("plain create must not set InPlace")
	}
}

// TestManagerCreateSessionRejectsInPlaceRemote mirrors the pre-#930 guard:
// a remote session has no local worktree, so combining it with --here must
// fail instead of silently ignoring one of the flags.
func TestManagerCreateSessionRejectsInPlaceRemote(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, err = manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:       "here-remote",
		RepoPath:    repoPath,
		Program:     "claude",
		InPlace:     true,
		ForceRemote: true,
	})
	if err == nil {
		t.Fatalf("expected InPlace+ForceRemote to be rejected")
	}
	if !strings.Contains(err.Error(), "in-place") {
		t.Fatalf("rejection must name the in-place conflict, got: %v", err)
	}
}

// TestCreateSessionRPCCarriesInPlace exercises the actual control-socket RPC
// (net/rpc over the daemon's unix socket), so `af sessions create --here`
// against a RUNNING daemon carries the flag across the wire — not just the
// in-process Manager path.
func TestCreateSessionRPCCarriesInPlace(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	closeServer, err := startControlServer(manager, nil, nil, make(chan struct{}))
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	var resp CreateSessionResponse
	if err := callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    "captain-here-rpc",
		RepoPath: repoPath,
		Program:  "claude",
		InPlace:  true,
	}, &resp); err != nil {
		t.Fatalf("CreateSession RPC: %v", err)
	}
	if resp.Instance.Title != "captain-here-rpc" {
		t.Fatalf("unexpected instance data: %+v", resp.Instance)
	}
	if len(*seen) != 1 || !(*seen)[0].InPlace {
		t.Fatalf("InPlace did not survive the control-socket round-trip: %+v", *seen)
	}
}
