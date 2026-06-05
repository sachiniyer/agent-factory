package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// readyFakeBackend is a FakeBackend whose Preview reports a ready prompt so
// that the daemon's waitForReady loop returns immediately. The create path
// now always waits for readiness — even for empty-prompt sessions (#698) — so
// the backend must look ready rather than returning blank Preview output.
type readyFakeBackend struct {
	*session.FakeBackend
}

func (readyFakeBackend) Preview(*session.Instance) (string, error) { return "ready\n❯", nil }

func installInstantBackend(t *testing.T) {
	t.Helper()
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return readyFakeBackend{backend}, nil
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

// TestManagerCreateSessionAtomicWithRefresh is a regression test for
// sachiniyer/agent-factory#509. The pre-fix code persisted a new session to
// disk before inserting it into m.instances under m.mu. The daemon's refresh
// loop rebuilds session.Instance objects from disk for any key it does not
// already see in m.instances — so a refresh that fired between disk-write
// and memory-insert constructed a duplicate Instance via FromInstanceData
// (opening a fresh PTY in the tmux backend) that became unreachable when
// CreateSession finally stored its own instance under the same key. The
// duplicate's PTY fd was then leaked for the lifetime of the daemon.
//
// The fix folds the in-memory insert and the disk write into a single
// critical section under m.mu, so refresh either runs before the disk
// write happens or blocks until the in-memory entry is present and is
// reused via existing-key dedup.
//
// We assert that property by counting how many times the refresh path
// invokes FromInstanceData. With the fix, refresh races with CreateSession
// can never observe a disk-only state, so FromInstanceData is never called
// for newly-created sessions and the counter stays at zero. With the buggy
// ordering, refresh occasionally observes the gap and increments the
// counter — even in the test environment where the real FromInstanceData
// would have failed for lack of tmux, the call itself is recorded.
func TestManagerCreateSessionAtomicWithRefresh(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	var fromInstanceDataCalls atomic.Int32
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		fromInstanceDataCalls.Add(1)
		return prev(d)
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = manager.RefreshInstances()
				}
			}
		}()
	}
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})

	const numSessions = 8
	for i := 0; i < numSessions; i++ {
		title := fmt.Sprintf("race-%d", i)
		if _, err := manager.CreateSession(CreateSessionRequest{
			Title:    title,
			RepoPath: repoPath,
			Program:  "claude",
		}); err != nil {
			t.Fatalf("CreateSession(%q): %v", title, err)
		}
	}

	for i := 0; i < 50; i++ {
		_ = manager.RefreshInstances()
	}

	if got := fromInstanceDataCalls.Load(); got != 0 {
		t.Fatalf("refresh invoked FromInstanceData %d times — would have constructed duplicate Instance(s) and orphaned their PTY fds (#509)", got)
	}

	snap := manager.InstancesSnapshot()
	if len(snap) != numSessions {
		t.Fatalf("expected %d instances in memory, got %d", numSessions, len(snap))
	}

	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var stored []session.InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if len(stored) != numSessions {
		t.Fatalf("expected %d sessions on disk, got %d", numSessions, len(stored))
	}
}

// TestManagerCreateSessionIgnoresLoadingGhost is a regression test for
// sachiniyer/agent-factory#551. An older TUI binary could persist a
// Loading-status entry on quit; the daemon's title-collision check
// then treated it as a live reservation and rejected any future
// session creation with the same title. The fix skips Loading entries
// in disk-side validation so they no longer block — the next save will
// reap them.
func TestManagerCreateSessionIgnoresLoadingGhost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	ghostJSON, err := json.Marshal([]session.InstanceData{
		{Title: "stuck", Path: repoPath, Status: session.Loading},
	})
	if err != nil {
		t.Fatalf("marshal ghost: %v", err)
	}
	if err := config.LoadState().SaveInstances(repo.ID, ghostJSON); err != nil {
		t.Fatalf("seed ghost: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(CreateSessionRequest{
		Title:    "stuck",
		RepoPath: repoPath,
		Program:  "claude",
	}); err != nil {
		t.Fatalf("CreateSession should ignore Loading ghost, got: %v", err)
	}
}

// TestManagerCreateSessionRejectsCaseVariantTitle is a regression test for
// sachiniyer/agent-factory#605. git.SanitizeBranchName lowercases titles when
// deriving git branch names, so two case-variant titles ("MyApp" and "myapp")
// would map to the same branch. The daemon used to validate titles
// case-sensitively, accept both, and let the second worktree create fail with
// a cryptic git error. The daemon now rejects the case-variant up front with
// a clear conflict error before any worktree or tmux setup runs.
func TestManagerCreateSessionRejectsCaseVariantTitle(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(CreateSessionRequest{
		Title:    "MyApp",
		RepoPath: repoPath,
		Program:  "claude",
		AutoYes:  true,
	}); err != nil {
		t.Fatalf("first CreateSession: %v", err)
	}

	_, err = manager.CreateSession(CreateSessionRequest{
		Title:    "myapp",
		RepoPath: repoPath,
		Program:  "claude",
		AutoYes:  true,
	})
	if err == nil {
		t.Fatalf("expected case-variant title to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "myapp") || !strings.Contains(msg, "MyApp") {
		t.Fatalf("expected error to name both titles, got: %v", err)
	}
	if !strings.Contains(strings.ToLower(msg), "branch") {
		t.Fatalf("expected error to mention the shared git branch, got: %v", err)
	}
}

// TestManagerCreateSessionRejectsCaseVariantTitleFromDisk covers the disk-side
// branch of the #605 fix: a case-variant title persisted to disk from a prior
// daemon run must still be rejected when the manager loads fresh and a new
// CreateSession arrives.
func TestManagerCreateSessionRejectsCaseVariantTitleFromDisk(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	seeded, err := json.Marshal([]session.InstanceData{
		{Title: "MyApp", Path: repoPath, Status: session.Running},
	})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := config.LoadState().SaveInstances(repo.ID, seeded); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, err = manager.CreateSession(CreateSessionRequest{
		Title:    "myapp",
		RepoPath: repoPath,
		Program:  "claude",
		AutoYes:  true,
	})
	if err == nil {
		t.Fatalf("expected case-variant title to be rejected by disk check")
	}
	if !strings.Contains(err.Error(), "MyApp") {
		t.Fatalf("expected error to name the on-disk title, got: %v", err)
	}
}

// TestManagerCreateSessionRejectsSanitizeCollision is a regression test for
// sachiniyer/agent-factory#741, which completes #605. git.SanitizeBranchName
// normalizes more than case: it turns spaces into dashes, strips unsafe chars,
// and collapses dashes. So "A B" and "a-b" both derive the same branch (e.g.
// "<prefix>/a-b") even though they differ by more than case. The #605 fix only compared titles
// case-insensitively, so it accepted both and let the second worktree create
// fail with a cryptic git error. The daemon now compares the derived branch and
// rejects the collision up front.
func TestManagerCreateSessionRejectsSanitizeCollision(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(CreateSessionRequest{
		Title:    "A B",
		RepoPath: repoPath,
		Program:  "claude",
		AutoYes:  true,
	}); err != nil {
		t.Fatalf("first CreateSession: %v", err)
	}

	_, err = manager.CreateSession(CreateSessionRequest{
		Title:    "a-b",
		RepoPath: repoPath,
		Program:  "claude",
		AutoYes:  true,
	})
	if err == nil {
		t.Fatalf("expected sanitize-collision title to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "a-b") || !strings.Contains(msg, "A B") {
		t.Fatalf("expected error to name both titles, got: %v", err)
	}
	if !strings.Contains(strings.ToLower(msg), "branch") {
		t.Fatalf("expected error to mention the shared git branch, got: %v", err)
	}

	// The case-only path from #605 must still work: "Foo" then "foo" collides.
	if _, err := manager.CreateSession(CreateSessionRequest{
		Title:    "Foo",
		RepoPath: repoPath,
		Program:  "claude",
		AutoYes:  true,
	}); err != nil {
		t.Fatalf("CreateSession Foo: %v", err)
	}
	if _, err := manager.CreateSession(CreateSessionRequest{
		Title:    "foo",
		RepoPath: repoPath,
		Program:  "claude",
		AutoYes:  true,
	}); err == nil {
		t.Fatalf("expected case-variant title \"foo\" to still be rejected (#605)")
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

	result, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown returned error when no daemon present: %v", err)
	}
	if result != ShutdownNoDaemon {
		t.Fatalf("RequestShutdown returned %v when no daemon was running, want ShutdownNoDaemon", result)
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

	result, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown returned error on stale socket: %v", err)
	}
	if result != ShutdownNoDaemon {
		t.Fatalf("RequestShutdown returned %v against stale socket, want ShutdownNoDaemon", result)
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

	result, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if result != ShutdownViaRPC {
		t.Fatalf("RequestShutdown returned %v against live control server, want ShutdownViaRPC", result)
	}
	select {
	case <-shutdownCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("shutdownCh was not closed after RequestShutdown")
	}
}

// TestFormatWaitForReadyTimeoutError covers the UX half of
// sachiniyer/agent-factory#502: when waitForReady gives up, the returned
// error must carry a trimmed snippet of the captured pane content so the
// user-facing TUI shows what the agent was doing — not just "timed out".
// Empty captured content collapses back to the bare timeout message so
// users don't see a dangling "last pane content:" header.
func TestFormatWaitForReadyTimeoutError(t *testing.T) {
	timeout := 60 * time.Second

	t.Run("happy case appends trimmed snippet", func(t *testing.T) {
		// >5 lines and well under 400 bytes — should keep only the last 5.
		content := "boot 1\nboot 2\nboot 3\nLoading config...\nConnecting to MCP server...\nStill waiting on handshake\nAlmost there..."
		got := formatWaitForReadyTimeoutError(timeout, content).Error()

		wantHeader := "timed out waiting for program to start (1m0s)\nlast pane content:"
		if !strings.HasPrefix(got, wantHeader) {
			t.Fatalf("missing header.\n got=%q\nwant prefix=%q", got, wantHeader)
		}
		if !strings.Contains(got, "  Almost there...") {
			t.Errorf("expected indented snippet line in error, got %q", got)
		}
		if !strings.Contains(got, "  Loading config...") {
			t.Errorf("expected last-5-lines window to include 'Loading config...', got %q", got)
		}
		if strings.Contains(got, "boot 1") || strings.Contains(got, "boot 2") {
			t.Errorf("expected oldest lines to be trimmed off, got %q", got)
		}
	})

	t.Run("empty content omits header entirely", func(t *testing.T) {
		got := formatWaitForReadyTimeoutError(timeout, "").Error()
		want := "timed out waiting for program to start (1m0s)"
		if got != want {
			t.Fatalf("empty content error mismatch.\n got=%q\nwant=%q", got, want)
		}
	})

	t.Run("whitespace-only content treated as empty", func(t *testing.T) {
		got := formatWaitForReadyTimeoutError(timeout, "\n\n   \n\n").Error()
		want := "timed out waiting for program to start (1m0s)"
		if got != want {
			t.Fatalf("whitespace-only content error mismatch.\n got=%q\nwant=%q", got, want)
		}
	})

	t.Run("long content is byte-capped", func(t *testing.T) {
		// One huge line well over the 400-byte cap.
		long := strings.Repeat("x", 1200)
		got := formatWaitForReadyTimeoutError(timeout, long).Error()
		// Header + "\n  " + at most 400 bytes of snippet.
		header := "timed out waiting for program to start (1m0s)\nlast pane content:\n  "
		if !strings.HasPrefix(got, header) {
			t.Fatalf("missing header prefix, got %q", got)
		}
		snippet := strings.TrimPrefix(got, header)
		if len(snippet) > 400 {
			t.Errorf("snippet not capped: len=%d, want <=400", len(snippet))
		}
	})
}

// codexYOLOBanner is the actual codex startup pane reported in
// sachiniyer/agent-factory#714: codex rendered its banner, the YOLO-mode
// header, and the "›" (U+203A) input prompt, but the claude-only
// isReadyContent never matched it, so waitForReady spun for the full timeout.
const codexYOLOBanner = "╭───────────────────────────────────────────────╮\n" +
	"│ >_ OpenAI Codex (v0.135.0)                    │\n" +
	"│ permissions: YOLO mode                        │\n" +
	"╰───────────────────────────────────────────────╯\n" +
	"› Use /skills to list available skills"

// TestIsReadyContent covers the agent-aware ready detection added for #714.
// Mirrors task.TestIsReadyContent — the two packages keep duplicate copies of
// isReadyContent (no shared home without an import cycle), so both are tested.
func TestIsReadyContent(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		content string
		want    bool
	}{
		// claude (and the default / legacy fallback)
		{"empty", "claude", "", false},
		{"claude input prompt", "claude", "some output\n\n❯ ", true},
		{"claude trust prompt", "claude", "Do you trust the files in this folder?\n1. Yes", true},
		{"claude mcp trust prompt", "claude", "detected a new MCP server from `.mcp.json`.", true},
		{
			name:    "claude doc trust prompt",
			agent:   "claude",
			content: "Open documentation url: https://docs/\n(Y)es/(N)o/(D)on't ask again [Yes]:",
			want:    true,
		},
		{"claude not ready", "claude", "installing dependencies...", false},
		// Unknown / legacy program falls through to the claude signals.
		{"unknown program uses claude signals", "/usr/bin/some-tool", "out\n❯ ", true},
		{"unknown program not ready", "/usr/bin/some-tool", "compiling…", false},

		// codex — the #714 regression case.
		{"codex YOLO banner with prompt (#714)", "codex", codexYOLOBanner, true},
		{"codex bare prompt glyph", "codex", "some output\n› ", true},
		// #729: the workspace-trust dialog must NOT be treated as ready —
		// there is no codex dismissal for it, so waitForReady must keep
		// waiting for the real "›" prompt rather than letting the user's
		// prompt be typed into the dialog. Regression from #714/#715.
		{"codex trust folder prompt is not ready (#729)", "codex", "Do you trust this folder?\n> Yes", false},
		{"codex trust dialog with later prompt is ready (#729)", "codex", "Do you trust this folder?\n› ", true},
		{"codex not ready on claude glyph", "codex", "rendering\n❯ ", false},
		{"codex not ready on box border alone", "codex", "╭──╮\n│ x │\n╰──╯", false},

		// aider
		{"aider banner", "aider", "Aider v0.74.0\nMain model: ...", true},
		{"aider input prompt", "aider", "some output\n> ", true},
		{
			name:    "aider doc trust prompt",
			agent:   "aider",
			content: "Open documentation url: https://aider.chat/docs/\n(Y)es/(N)o/(D)on't ask again [Yes]:",
			want:    true,
		},
		{"aider not ready", "aider", "loading model weights…", false},

		// gemini (best-guess box-border signal — see #714)
		{"gemini box frame", "gemini", "╭──╮\n│ Gemini │\n╰──╯", true},
		{
			name:    "gemini doc trust prompt",
			agent:   "gemini",
			content: "Gemini CLI\nOpen documentation url for more info.\n(D)on't ask again",
			want:    true,
		},
		{"gemini not ready", "gemini", "starting gemini-cli…", false},

		// shared doc-trust guard: both substrings required.
		{"only open documentation url without confirm", "claude", "See Open documentation url for details.", false},
		{"only dont ask again without doc url", "aider", "Some prompt asking (D)on't ask again", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadyContent(tc.content, tc.agent); got != tc.want {
				t.Errorf("isReadyContent(%q, %q) = %v, want %v", tc.content, tc.agent, got, tc.want)
			}
		})
	}
}

// stubGhostCleanup replaces both ghostCleanupWorktree and ghostKillTmuxByName
// with recorders so tests can assert which teardown branches fired without
// invoking real git / real tmux.
func stubGhostCleanup(t *testing.T) (wtCalls *[]string, tmuxCalls *[]string) {
	t.Helper()
	var wt, tm []string
	prevWT := ghostCleanupWorktree
	prevTmux := ghostKillTmuxByName
	ghostCleanupWorktree = func(data *session.InstanceData, title string) {
		if data.Worktree.RepoPath == "" || data.Worktree.WorktreePath == "" || data.Worktree.ExternalWorktree {
			return
		}
		wt = append(wt, title)
	}
	ghostKillTmuxByName = func(name string) error {
		tm = append(tm, name)
		return nil
	}
	t.Cleanup(func() {
		ghostCleanupWorktree = prevWT
		ghostKillTmuxByName = prevTmux
	})
	return &wt, &tm
}

// TestGhostCleanup_TmuxOrphan is the daemon-side regression test for #549:
// PR #536 fixed the same orphan in api/sessions.go, but the daemon kill path
// kept the old worktree-only teardown, so the bug returned through the
// daemon-routed kill. With an empty worktree path and a populated TmuxName,
// ghostCleanup must still attempt to kill the tmux session.
func TestGhostCleanup_TmuxOrphan(t *testing.T) {
	wtCalls, tmCalls := stubGhostCleanup(t)

	data := &session.InstanceData{
		Title:    "ghost",
		Program:  "claude",
		TmuxName: "af_ghost",
	}
	ghostCleanup(data, "ghost")

	if len(*wtCalls) != 0 {
		t.Fatalf("expected worktree cleanup skipped, got: %v", *wtCalls)
	}
	if len(*tmCalls) != 1 || (*tmCalls)[0] != "af_ghost" {
		t.Fatalf("expected tmux kill for af_ghost, got: %v", *tmCalls)
	}
}

// TestGhostCleanup_BothPopulated verifies the fix did not regress the
// worktree-cleanup branch: with both fields populated, both teardowns fire.
func TestGhostCleanup_BothPopulated(t *testing.T) {
	wtCalls, tmCalls := stubGhostCleanup(t)

	data := &session.InstanceData{
		Title:    "ghost",
		Program:  "claude",
		TmuxName: "af_ghost",
		Worktree: session.GitWorktreeData{
			RepoPath:     "/tmp/repo",
			WorktreePath: "/tmp/wt",
			SessionName:  "ghost",
			BranchName:   "af/ghost",
		},
	}
	ghostCleanup(data, "ghost")

	if len(*wtCalls) != 1 || (*wtCalls)[0] != "ghost" {
		t.Fatalf("expected worktree cleanup, got: %v", *wtCalls)
	}
	if len(*tmCalls) != 1 || (*tmCalls)[0] != "af_ghost" {
		t.Fatalf("expected tmux kill for af_ghost, got: %v", *tmCalls)
	}
}

// TestGhostCleanup_AllEmpty verifies that with no TmuxName and no worktree
// paths, both teardown branches are skipped.
func TestGhostCleanup_AllEmpty(t *testing.T) {
	wtCalls, tmCalls := stubGhostCleanup(t)

	data := &session.InstanceData{
		Title:   "ghost",
		Program: "claude",
	}
	ghostCleanup(data, "ghost")

	if len(*wtCalls) != 0 {
		t.Fatalf("expected no worktree cleanup, got: %v", *wtCalls)
	}
	if len(*tmCalls) != 0 {
		t.Fatalf("expected no tmux kill, got: %v", *tmCalls)
	}
}

// TestGhostKillTmuxByName_RefusesNonAfPrefix guards the validation in the
// real ghostKillTmuxByName: a sanitized name without the af_ prefix would
// only appear via storage corruption, and silently killing whatever tmux
// session it names could destroy unrelated work.
func TestGhostKillTmuxByName_RefusesNonAfPrefix(t *testing.T) {
	if err := ghostKillTmuxByName("not-ours"); err == nil {
		t.Fatalf("expected refusal for non-af prefix, got nil")
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
