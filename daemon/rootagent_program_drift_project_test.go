package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestAdoptedRootProgramCacheRevalidatesProjectOverride(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	previousInterval := rootProgramDriftConfigInspectionInterval
	rootProgramDriftConfigInspectionInterval = 0
	t.Cleanup(func() { rootProgramDriftConfigInspectionInterval = previousInterval })

	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	if _, err := config.SetProjectConfigValue(project.ID, "program_overrides.codex", "/old/codex"); err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t,
		rootTestConfig(repoPath, config.RootAgentConfig{Program: "codex"}))
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	findRootInstance(t, manager, repoPath).SetTmuxSession(tmux.NewTmuxSession("root-runtime", "/old/codex"))

	manager.ensureRootAgentsAndWait()
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("ensure state was not created")
	}
	waitForRootProgramResolutionIdle(t, manager, st)

	if _, err := config.SetProjectConfigValue(project.ID, "program_overrides.codex", "/new/codex"); err != nil {
		t.Fatal(err)
	}
	manager.ensureRootAgentsAndWait()
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	configured := st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if configured != "/new/codex" {
		t.Fatalf("configured command after project override write = %q, want /new/codex", configured)
	}
	if !strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("project-local override drift was hidden by the stale cache:\n%s", warnings.String())
	}
}

func TestAdoptedRootProgramDriftSkipsStartupUnknownRuntime(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, warnings := newManagerCapturingWarnings(t,
		rootTestConfig(repoPath, config.RootAgentConfig{Program: "codex"}))
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	root := findRootInstance(t, manager, repoPath)
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "claude"))
	root.MarkStartupStateUnknown()

	manager.ensureRootAgentsAndWait()
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("ensure state was not created")
	}
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	latched := st.programDriftLogged || manager.rootProgramDriftLogged[config.RepoIDFromRoot(repoPath)]
	manager.mu.Unlock()
	if latched || strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("startup-unknown runtime was compared and latched: latched=%v warnings=%s", latched, warnings.String())
	}
}

func TestAdoptedRootProgramDriftSkipsUserKilledRuntime(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, warnings := newManagerCapturingWarnings(t,
		rootTestConfig(repoPath, config.RootAgentConfig{Program: "/opt/codex"}))
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	root := findRootInstance(t, manager, repoPath)
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "claude"))
	root.MarkUserKilled()
	profile := config.RootAgent{Enabled: true, Program: "/opt/codex"}
	st := &rootEnsureState{
		programDriftResolved:          true,
		programDriftResolvedRepoID:    repo.ID,
		programDriftResolvedWorkspace: repo.WorkspacePath(),
		programDriftResolvedProfile:   profile,
		programDriftConfiguredProgram: "/opt/codex",
	}

	manager.checkAdoptedRootProgramDrift(repo,
		daemonInstanceKey(repo.ID, session.RootSessionTitle), repo.WorkspacePath(), st, profile, root)

	manager.mu.Lock()
	latched := st.programDriftLogged || manager.rootProgramDriftLogged[repo.ID]
	manager.mu.Unlock()
	if latched || strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("user-killed runtime was compared and latched: latched=%v warnings=%s", latched, warnings.String())
	}
}

func TestTimedOutAdoptedRootProgramInspectionStaysSingleFlightUntilReaderExits(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	previousInterval := rootProgramDriftConfigInspectionInterval
	previousBudget := rootRepoProbeBudget
	previousResolve := resolveRootProgramConfigForInspection
	rootProgramDriftConfigInspectionInterval = 20 * time.Millisecond
	rootRepoProbeBudget = 50 * time.Millisecond
	readerRelease := make(chan struct{})
	readerDone := make(chan struct{}, 3)
	resolveStarted := make(chan struct{}, 3)
	released := false
	resolveRootProgramConfigForInspection = func(_ *config.RepoContext, _ *config.Config) (*config.ResolvedConfig, error) {
		resolveStarted <- struct{}{}
		<-readerRelease
		readerDone <- struct{}{}
		return &config.ResolvedConfig{Config: *config.DefaultConfig()}, nil
	}
	t.Cleanup(func() {
		rootProgramDriftConfigInspectionInterval = previousInterval
		rootRepoProbeBudget = previousBudget
		resolveRootProgramConfigForInspection = previousResolve
		if !released {
			close(readerRelease)
		}
	})

	repoPath := setupControlRepo(t)
	manager, _ := newManagerCapturingWarnings(t,
		rootTestConfig(repoPath, config.RootAgentConfig{Program: "codex"}))
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	root := findRootInstance(t, manager, repoPath)
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "codex"))
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	profile := config.RootAgent{Enabled: true, Program: "codex"}
	st := &rootEnsureState{}
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)

	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root)
	select {
	case <-resolveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first drift inspection did not start")
	}
	waitForRootProgramResolutionParked(t, manager, st)
	time.Sleep(2 * rootProgramDriftConfigInspectionInterval)
	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root)
	select {
	case <-resolveStarted:
		t.Fatal("elapsed backoff admitted a second inspection while the first reader was still blocked")
	case <-time.After(100 * time.Millisecond):
	}

	close(readerRelease)
	released = true
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("released reader did not exit")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root)
		manager.mu.Lock()
		resolving := st.programDriftResolving
		manager.mu.Unlock()
		if !resolving {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed reader did not release the single-flight state")
		}
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(2 * rootProgramDriftConfigInspectionInterval)
	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root)
	select {
	case <-resolveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("a completed reader did not permit the next inspection")
	}
	waitForRootProgramResolutionIdle(t, manager, st)
}

func waitForRootProgramResolutionParked(t *testing.T, manager *Manager, st *rootEnsureState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.mu.Lock()
		parked := st.programDriftResolving && st.programDriftResolverDone != nil
		manager.mu.Unlock()
		if parked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for root program resolution to retain its reader")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
