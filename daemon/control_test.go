package daemon

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

func setupControlRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := exec.Command("git", "-C", repo, "config", "user.email", "test@example.com").Run(); err != nil {
		t.Fatalf("git config email: %v", err)
	}
	if err := exec.Command("git", "-C", repo, "config", "user.name", "Test User").Run(); err != nil {
		t.Fatalf("git config name: %v", err)
	}
	if err := exec.Command("git", "-C", repo, "commit", "--allow-empty", "-m", "init").Run(); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	return repo
}

func installInstantBackend(t *testing.T) {
	t.Helper()
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return backend, nil
	})
	t.Cleanup(restore)
}

func TestManagerCreateSessionPersistsAndRejectsDuplicate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(CreateSessionRequest{
		Title:    "daemon-owned",
		RepoPath: repoPath,
		Program:  "claude",
		AutoYes:  true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if data.Title != "daemon-owned" || !data.AutoYes || data.Status != session.Running {
		t.Fatalf("unexpected created data: %+v", data)
	}

	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var stored []session.InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if len(stored) != 1 || stored[0].Title != "daemon-owned" {
		t.Fatalf("expected created session in storage, got %+v", stored)
	}

	if _, err := manager.CreateSession(CreateSessionRequest{
		Title:    "daemon-owned",
		RepoPath: repoPath,
		Program:  "claude",
	}); err == nil {
		t.Fatalf("expected duplicate title to be rejected")
	}
}

func TestControlServerCreateAndKillSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	closeServer, err := startControlServer(manager, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	var createResp CreateSessionResponse
	if err := callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    "rpc-session",
		RepoPath: repoPath,
		Program:  "claude",
	}, &createResp); err != nil {
		t.Fatalf("rpc CreateSession: %v", err)
	}
	if createResp.Instance.Title != "rpc-session" {
		t.Fatalf("unexpected create response: %+v", createResp)
	}

	var killResp KillSessionResponse
	if err := callDaemonNoEnsure("KillSession", KillSessionRequest{
		Title:  "rpc-session",
		RepoID: repo.ID,
	}, &killResp); err != nil {
		t.Fatalf("rpc KillSession: %v", err)
	}

	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var stored []session.InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("expected storage to be empty after kill, got %+v", stored)
	}
}

// TestControlServerShutdownClosesChannel verifies that the Shutdown RPC
// acknowledges with OK and closes the supplied shutdownCh exactly once,
// which is what RunDaemon's main select uses to exit the daemon loop (#498).
func TestControlServerShutdownClosesChannel(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	shutdownCh := make(chan struct{})
	closeServer, err := startControlServer(manager, shutdownCh)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	var resp ShutdownResponse
	if err := callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp); err != nil {
		t.Fatalf("rpc Shutdown: %v", err)
	}
	if !resp.OK {
		t.Fatalf("Shutdown returned OK=false")
	}

	select {
	case <-shutdownCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("shutdownCh was not closed within 2s after Shutdown RPC")
	}

	// A second Shutdown call must not double-close the channel (panic).
	var resp2 ShutdownResponse
	if err := callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp2); err != nil {
		// The listener may already be closed depending on timing; accept
		// either a successful ack or a transport-level error, but never a
		// double-close panic on the server side.
		t.Logf("second Shutdown returned (expected, may race with teardown): %v", err)
	}
}

// TestRequestShutdownNoDaemon verifies that RequestShutdown silently
// no-ops when no daemon socket exists — the case during `af upgrade` on a
// fresh install or in CI.
func TestRequestShutdownNoDaemon(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	restarted, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown returned error when no daemon present: %v", err)
	}
	if restarted {
		t.Fatalf("RequestShutdown reported restart when no daemon was running")
	}
}

// TestRequestShutdownStaleSocket verifies that RequestShutdown treats a
// socket file with no listener as "no daemon" (connection refused) rather
// than propagating the transport error to callers.
func TestRequestShutdownStaleSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	socketPath, err := DaemonSocketPath()
	if err != nil {
		t.Fatalf("DaemonSocketPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A regular file at the socket path causes Dial to return ECONNREFUSED
	// (or an equivalent transport error). RequestShutdown must swallow it.
	if err := os.WriteFile(socketPath, nil, 0600); err != nil {
		t.Fatalf("write stale socket placeholder: %v", err)
	}

	restarted, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown returned error on stale socket: %v", err)
	}
	if restarted {
		t.Fatalf("RequestShutdown reported restart against stale socket")
	}
}

// TestRequestShutdownSuccess starts a real control server and verifies the
// end-to-end Shutdown flow: client sees OK, server's shutdownCh closes.
func TestRequestShutdownSuccess(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	shutdownCh := make(chan struct{})
	closeServer, err := startControlServer(manager, shutdownCh)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	restarted, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if !restarted {
		t.Fatalf("expected restarted=true against live control server")
	}
	select {
	case <-shutdownCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("shutdownCh was not closed after RequestShutdown")
	}
}

// TestIsDaemonAbsentErr covers the small classifier RequestShutdown uses to
// decide whether a dial/RPC failure means "no daemon" (silent no-op) or
// "unexpected transport problem" (surface to the caller).
func TestIsDaemonAbsentErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"no such file", &net.OpError{Op: "dial", Err: fs.ErrNotExist}, true},
		{"wrapped enoent", errors.New("some other error"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDaemonAbsentErr(c.err); got != c.want {
				t.Errorf("isDaemonAbsentErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
