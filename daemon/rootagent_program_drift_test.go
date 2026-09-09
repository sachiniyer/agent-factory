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

	manager.ensureRootAgentsAndWait()
	manager.ensureRootAgentsAndWait()

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

	manager.ensureRootAgentsAndWait()
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
