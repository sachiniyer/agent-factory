package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestAdoptedSingletonBareWorktreePreservesIdentityForCommandResolution(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	base := testguard.CanonicalTempDir(t)
	seedPath := filepath.Join(base, "seed")
	barePath := filepath.Join(base, "identity.git")
	worktreePath := filepath.Join(base, "checkout")
	setupRootDriftRepoAt(t, seedPath)
	if err := exec.Command("git", "clone", "--bare", seedPath, barePath).Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "--git-dir", barePath, "worktree", "add", worktreePath).Run(); err != nil {
		t.Fatal(err)
	}
	project := registerTestProject(t, worktreePath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = 'codex'")
	repo, err := config.RepoFromPath(project.Root)
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: project.Root, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	root := findRootInstance(t, manager, project.Root)
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "/resolved/codex"))

	previousResolve := resolveRootProgramConfigForInspection
	identitySeen := make(chan string, 1)
	resolveRootProgramConfigForInspection = func(got *config.RepoContext, _ *config.Config) (*config.ResolvedConfig, error) {
		select {
		case identitySeen <- got.IdentityPath():
		default:
		}
		resolved := config.DefaultConfig()
		resolved.ProgramOverrides = map[string]string{"codex": "/resolved/codex"}
		return &config.ResolvedConfig{Config: *resolved}, nil
	}
	t.Cleanup(func() { resolveRootProgramConfigForInspection = previousResolve })

	manager.ensureRootAgentsAndWait()
	select {
	case got := <-identitySeen:
		if got != repo.IdentityPath() {
			t.Fatalf("drift command resolver identity = %q, want bare repository %q", got, repo.IdentityPath())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drift command resolver did not run")
	}
}

func TestTimedOutCheckoutMarkerReadRetainsRootProgramSingleFlight(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	previousInterval := rootProgramDriftConfigInspectionInterval
	rootProgramDriftConfigInspectionInterval = 0
	t.Cleanup(func() { rootProgramDriftConfigInspectionInterval = previousInterval })

	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
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
	realResolve := resolveRootProgramConfigForInspection
	var resolveMu sync.Mutex
	resolveCount := 0
	resolveRootProgramConfigForInspection = func(repo *config.RepoContext, global *config.Config) (*config.ResolvedConfig, error) {
		resolveMu.Lock()
		resolveCount++
		resolveMu.Unlock()
		return realResolve(repo, global)
	}
	t.Cleanup(func() { resolveRootProgramConfigForInspection = realResolve })
	markers, err := filepath.Glob(filepath.Join(repoPath, ".git", "agent-factory", "checkout-id-*"))
	if err != nil {
		t.Fatal(err)
	}
	marker := ""
	for _, candidate := range markers {
		if !strings.HasSuffix(candidate, ".lock") {
			marker = candidate
		}
	}
	if marker == "" {
		t.Fatalf("find checkout marker: paths=%v err=%v", markers, err)
	}
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(marker, 0o600); err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	releaseDone := make(chan error, 1)
	released := false
	release := func() {
		releaseOnce.Do(func() {
			go func() { releaseDone <- os.WriteFile(marker, markerData, 0o600) }()
		})
	}
	t.Cleanup(func() {
		if !released {
			release()
			<-releaseDone
		}
		_ = os.Remove(marker)
		_ = os.WriteFile(marker, markerData, 0o600)
	})

	profile := config.RootAgent{Enabled: true, Program: "codex"}
	st := &rootEnsureState{}
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root, nil)
	time.Sleep(500 * time.Millisecond)
	manager.mu.Lock()
	resolving := st.programDriftResolving
	manager.mu.Unlock()
	if !resolving {
		t.Fatal("checkout-marker timeout released the single flight while its os.ReadFile was still blocked")
	}
	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root, nil)
	time.Sleep(50 * time.Millisecond)
	manager.mu.Lock()
	stillResolving := st.programDriftResolving
	manager.mu.Unlock()
	if !stillResolving {
		t.Fatal("a second sweep replaced the still-running checkout-marker inspection")
	}
	resolveMu.Lock()
	started := resolveCount
	resolveMu.Unlock()
	if started != 1 {
		t.Fatalf("config inspections started while the marker reader was parked = %d, want 1", started)
	}

	release()
	releaseErr := <-releaseDone
	released = true
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, markerData, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root, nil)
	waitForRootProgramResolutionIdle(t, manager, st)
	resolveMu.Lock()
	started = resolveCount
	resolveMu.Unlock()
	if started != 2 {
		t.Fatalf("config inspections after the parked reader completed = %d, want 2", started)
	}
}

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
		daemonInstanceKey(repo.ID, session.RootSessionTitle), repo.WorkspacePath(), st, profile, root, nil)

	manager.mu.Lock()
	latched := st.programDriftLogged || manager.rootProgramDriftLogged[repo.ID]
	manager.mu.Unlock()
	if latched || strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("user-killed runtime was compared and latched: latched=%v warnings=%s", latched, warnings.String())
	}
}

func TestSlowAdoptedRootProgramInspectionStaysSingleFlightAndConsumesResult(t *testing.T) {
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
		resolved := config.DefaultConfig()
		resolved.ProgramOverrides = map[string]string{"codex": "/resolved/codex"}
		return &config.ResolvedConfig{Config: *resolved}, nil
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
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "/resolved/codex"))
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	profile := config.RootAgent{Enabled: true, Program: "codex"}
	st := &rootEnsureState{}
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)

	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root, nil)
	select {
	case <-resolveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first drift inspection did not start")
	}
	// The old caller budget is deliberately exceeded. The slow reader remains
	// the one in-flight resolution; elapsed time must neither discard its result
	// nor admit a replacement reader.
	time.Sleep(2 * rootProgramDriftConfigInspectionInterval)
	time.Sleep(rootRepoProbeBudget)
	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root, nil)
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
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	configured := st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if configured != "/resolved/codex" {
		t.Fatalf("completed slow inspection was discarded: configured command = %q", configured)
	}

	time.Sleep(2 * rootProgramDriftConfigInspectionInterval)
	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st, profile, root, nil)
	select {
	case <-resolveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("a completed reader did not permit the next inspection")
	}
	waitForRootProgramResolutionIdle(t, manager, st)
}
