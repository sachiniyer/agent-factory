package daemon

import (
	"context"
	"strings"
	"testing"

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
