package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestEnsureRootAgentsWarnsOnceForAdoptedProgramDrift is issue #4087's
// daemon-side regression: adopt-never-clobber remains intact, but adopting a
// root whose command disagrees with the frozen resolved profile must no longer
// be silent or repeat on every ensure tick.
func TestEnsureRootAgentsWarnsOnceForAdoptedProgramDrift(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, warnings := newManagerCapturingWarnings(t,
		rootTestConfig(repoPath, config.RootAgentConfig{Program: "codex"}))
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:         session.RootSessionTitle,
		RepoPath:      repoPath,
		Program:       "claude",
		InPlace:       true,
		allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	findRootInstance(t, manager, repoPath).SetTmuxSession(tmux.NewTmuxSession("root-runtime", "claude"))

	manager.ensureRootAgentsAndWait()
	manager.ensureRootAgentsAndWait()
	waitForRootProgramWarning(t, warnings)

	got := warnings.String()
	for _, want := range []string{repoPath, `configured command "codex"`, `running command "claude"`, "kill the root, then restart the daemon"} {
		if !strings.Contains(got, want) {
			t.Errorf("program-drift warning missing %q:\n%s", want, got)
		}
	}
	if count := strings.Count(got, "configured command"); count != 1 {
		t.Errorf("program drift logged %d times, want once per repo ensure-state:\n%s", count, got)
	}
	root := findRootInstance(t, manager, repoPath)
	if root == nil || root.AgentProgram() != "claude" {
		t.Fatalf("adopt-never-clobber changed: root=%v", root)
	}
}

func TestAdoptedRootProgramDriftDedupesEquivalentPathsByRepository(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	aliasPath := filepath.Join(t.TempDir(), "repo-alias")
	if err := os.Symlink(repoPath, aliasPath); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{
		repoPath:  {Program: "codex"},
		aliasPath: {Program: "codex"},
	}
	manager, warnings := newManagerCapturingWarnings(t, cfg)
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	findRootInstance(t, manager, repoPath).SetTmuxSession(tmux.NewTmuxSession("root-runtime", "claude"))

	manager.ensureRootAgentsAndWait()
	waitForRootProgramWarning(t, warnings)
	if count := strings.Count(warnings.String(), "configured command"); count != 1 {
		t.Fatalf("equivalent paths logged program drift %d times, want once by repository:\n%s", count, warnings.String())
	}
}

func TestAdoptedRootDefaultProgramResolvesOnceAcrossEnsureSweeps(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	findRootInstance(t, manager, repoPath).SetTmuxSession(tmux.NewTmuxSession("root-runtime", "claude"))

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	countPath := filepath.Join(shimDir, "count")
	script := fmt.Sprintf("#!/bin/sh\nprintf 'x\\n' >> %q\nexec %q \"$@\"\n", countPath, realGit)
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	st := &rootEnsureState{}
	manager.ensureResolvedRoot(repoPath, st, repo,
		config.RootAgentResolution{RootAgent: config.RootAgent{Enabled: true}}, nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.mu.Lock()
		resolved := st.programDriftResolved
		manager.mu.Unlock()
		if resolved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for default command resolution")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		manager.ensureResolvedRoot(repoPath, st, repo,
			config.RootAgentResolution{RootAgent: config.RootAgent{Enabled: true}}, nil)
	}
	after, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("default command resolution ran again across healthy sweeps: before=%d calls after=%d calls",
			strings.Count(string(before), "x\n"), strings.Count(string(after), "x\n"))
	}
}

func TestAdoptedRootProgramDriftResolvesBareAgentOverride(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	if err := writeRootDriftRepoConfig(repoPath, "[program_overrides]\ncodex = 'new-codex'\n"); err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t,
		rootTestConfig(repoPath, config.RootAgentConfig{Program: "codex"}))
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	root := findRootInstance(t, manager, repoPath)
	root.Program = "codex" // Fixture-only: model a persisted label whose pane is stale.
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "old-codex"))

	manager.ensureRootAgentsAndWait()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(warnings.String(), `configured command "new-codex"`) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := warnings.String()
	if !strings.Contains(got, `configured command "new-codex"`) || !strings.Contains(got, `running command "old-codex"`) {
		t.Fatalf("bare root program was not compared after override resolution:\n%s", got)
	}
}

func TestAdoptedRootProgramResolutionFailureRetries(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	root := findRootInstance(t, manager, repoPath)
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "claude"))
	if err := writeRootDriftRepoConfig(repoPath, "not valid toml = [\n"); err != nil {
		t.Fatal(err)
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	st := &rootEnsureState{}
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	manager.checkAdoptedRootProgramDrift(repo, key, repoPath, st, config.RootAgent{}, root)
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	resolvedAfterFailure := st.programDriftResolved
	manager.mu.Unlock()
	if resolvedAfterFailure {
		t.Fatal("transient resolution failure was cached as a successful fallback")
	}

	if err := writeRootDriftRepoConfig(repoPath, "[program_overrides]\nclaude = 'codex'\n"); err != nil {
		t.Fatal(err)
	}
	manager.checkAdoptedRootProgramDrift(repo, key, repoPath, st, config.RootAgent{}, root)
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	resolved, configured := st.programDriftResolved, st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if !resolved || configured != "codex" {
		t.Fatalf("recovered resolution = (%v, %q), want (true, codex)", resolved, configured)
	}
}

func TestAdoptedRootProgramCacheIncludesRepositoryIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	base := t.TempDir()
	firstPath := filepath.Join(base, "first")
	secondPath := filepath.Join(base, "second")
	workspace := filepath.Join(base, "configured-root")
	setupRootDriftRepoAt(t, firstPath)
	setupRootDriftRepoAt(t, secondPath)
	if err := writeRootDriftRepoConfig(firstPath, "[program_overrides]\nclaude = 'codex'\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeRootDriftRepoConfig(secondPath, "[program_overrides]\nclaude = 'gemini'\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(firstPath, workspace); err != nil {
		t.Fatal(err)
	}
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: workspace, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	root := findRootInstance(t, manager, workspace)
	root.Program = "codex" // Fixture-only: keep the label constant across the repoint.
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "codex"))
	firstRepo, err := config.RepoFromPath(workspace)
	if err != nil {
		t.Fatal(err)
	}
	st := &rootEnsureState{}
	manager.checkAdoptedRootProgramDrift(firstRepo,
		daemonInstanceKey(firstRepo.ID, session.RootSessionTitle), workspace, st, config.RootAgent{}, root)
	waitForRootProgramResolutionIdle(t, manager, st)

	if err := os.Remove(workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondPath, workspace); err != nil {
		t.Fatal(err)
	}
	secondRepo, err := config.RepoFromPath(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if secondRepo.ID == firstRepo.ID {
		t.Fatal("replacement repository unexpectedly kept the old identity")
	}
	manager.checkAdoptedRootProgramDrift(secondRepo,
		daemonInstanceKey(secondRepo.ID, session.RootSessionTitle), workspace, st, config.RootAgent{}, root)
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	configured := st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if configured != "gemini" {
		t.Fatalf("repointed workspace reused configured command %q, want gemini", configured)
	}
}

func waitForRootProgramResolutionIdle(t *testing.T, manager *Manager, st *rootEnsureState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.mu.Lock()
		resolving := st.programDriftResolving
		manager.mu.Unlock()
		if !resolving {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for root program resolution")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForRootProgramWarning(t *testing.T, warnings *logCapture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(warnings.String(), "configured command") {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeRootDriftRepoConfig(repoPath, body string) error {
	path := config.InRepoTomlConfigPath(repoPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

func setupRootDriftRepoAt(t *testing.T, path string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", path},
		{"-C", path, "config", "user.email", "test@example.com"},
		{"-C", path, "config", "user.name", "Test User"},
		{"-C", path, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}
