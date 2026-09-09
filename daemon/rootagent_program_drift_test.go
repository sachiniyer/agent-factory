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
	waitForRootProgramWarning(t, warnings)
	got := warnings.String()
	manager.mu.Lock()
	configured := manager.rootEnsureStates[repoPath].programDriftConfiguredProgram
	manager.mu.Unlock()
	if configured != "new-codex" || !strings.Contains(got, "root agent program drift") {
		t.Fatalf("bare root program was not compared after override resolution: configured=%q warnings=%s", configured, got)
	}
}

func TestCreatedRootWithPaddedBareProgramDoesNotReportDrift(t *testing.T) {
	testguard.IsolateTmux(t)
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "codex")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf 'ready\\n❯\\n›\\n> \\n╰\\n'\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const padded = " codex "
	cfg := rootTestConfig(repoPath, config.RootAgentConfig{Program: padded})
	cfg.ProgramOverrides = map[string]string{"codex": shim}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t, loaded)
	manager.ensureRootAgentsAndWait()
	root := findRootInstance(t, manager, repoPath)
	if root == nil {
		t.Fatal("root was not created")
	}
	if got := root.RuntimeProgram(); got != shim {
		t.Fatalf("real launch recorded RuntimeProgram %q, want the normalized label's override %q", got, shim)
	}

	manager.ensureRootAgentsAndWait()
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("ensure state was not created")
	}
	waitForRootProgramResolutionIdle(t, manager, st)
	if strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("root created by AF reported drift against its own launch command:\n%s", warnings.String())
	}
}

func TestAdoptedRootDefaultProfileMatchesChainedLaunchOverrides(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	cfg := rootTestConfig(repoPath, config.RootAgentConfig{})
	cfg.ProgramOverrides = map[string]string{
		"claude": "codex",
		"codex":  "/opt/codex",
	}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t, cfg)
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create launched root: %v", err)
	}
	root := findRootInstance(t, manager, repoPath)
	// SetTmuxSession is the test launch boundary: the root create hands its
	// first-stage "codex" result to the ordinary session resolver, whose second
	// lookup selects /opt/codex and records that command as runtime evidence.
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "/opt/codex"))

	manager.ensureRootAgentsAndWait()
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("ensure state was not created")
	}
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	configured := st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if configured != "/opt/codex" {
		t.Fatalf("configured command = %q, want the twice-resolved launch command /opt/codex", configured)
	}
	if strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("freshly launched root reported false drift:\n%s", warnings.String())
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

func TestAdoptedRootProgramDriftRevalidatesRuntimeBeforeLatching(t *testing.T) {
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
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	root := findRootInstance(t, manager, repoPath)
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "claude"))
	st := &rootEnsureState{programDriftResolving: true}
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	evidence := root.ObserveRuntimeProgram()
	if err := root.Transition(session.BeginHandoff()); err != nil {
		t.Fatal(err)
	}
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "codex"))
	manager.finishAdoptedRootProgramDrift(repo.ID, key, repoPath, st,
		config.RootAgent{Enabled: true, Program: "codex"}, "codex", nil, root, evidence)
	if strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("completion latched drift from the replaced runtime:\n%s", warnings.String())
	}
	manager.mu.Lock()
	logged := st.programDriftLogged || manager.rootProgramDriftLogged[repo.ID]
	manager.mu.Unlock()
	if logged {
		t.Fatal("stale runtime evidence permanently latched the repository drift bit")
	}
}

func TestAdoptedRootProgramDriftRedactsCommandPayloads(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	configured := "/home/private-user/bin/claude --token sk-ant-PRIVATEVALUE1234567890"
	running := "/home/other-user/bin/codex --api-key=PRIVATE-RUNTIME-TOKEN"
	manager, warnings := newManagerCapturingWarnings(t,
		rootTestConfig(repoPath, config.RootAgentConfig{Program: configured}))
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	findRootInstance(t, manager, repoPath).SetTmuxSession(tmux.NewTmuxSession("root-runtime", running))

	manager.ensureRootAgentsAndWait()
	got := warnings.String()
	for _, private := range []string{"private-user", "other-user", "PRIVATEVALUE", "PRIVATE-RUNTIME-TOKEN"} {
		if strings.Contains(got, private) {
			t.Errorf("drift warning leaked %q:\n%s", private, got)
		}
	}
	for _, label := range []string{`configured command "claude"`, `running command "codex"`} {
		if !strings.Contains(got, label) {
			t.Errorf("redacted warning missing bounded label %q:\n%s", label, got)
		}
	}
}

func TestAdoptedSingletonBareWorktreeUsesCheckoutCommandLayers(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	base := t.TempDir()
	seedPath := filepath.Join(base, "seed")
	barePath := filepath.Join(base, "identity.git")
	worktreePath := filepath.Join(base, "checkout")
	setupRootDriftRepoAt(t, seedPath)
	if err := writeRootDriftRepoConfig(seedPath, "[program_overrides]\ncodex = '/repo/codex'\n"); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", seedPath, "add", ".").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", seedPath, "commit", "-m", "config").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "clone", "--bare", seedPath, barePath).Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "--git-dir", barePath, "worktree", "add", worktreePath).Run(); err != nil {
		t.Fatal(err)
	}
	project := registerTestProject(t, worktreePath)
	projectConfig, err := config.ProjectConfigTomlPath(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("[root_agent]\nenabled = true\nprogram = 'codex'\n\n[program_overrides]\ncodex = '/personal/codex'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t, config.DefaultConfig())
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: worktreePath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	findRootInstance(t, manager, worktreePath).SetTmuxSession(tmux.NewTmuxSession("root-runtime", "/personal/codex"))

	manager.ensureRootAgentsAndWait()
	manager.mu.Lock()
	st := manager.rootEnsureStates[worktreePath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("singleton ensure state was not created")
	}
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	configured := st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if configured != "/personal/codex" {
		t.Fatalf("bare-worktree command = %q, want the registered checkout's personal override", configured)
	}
	if strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("bare-worktree root reported false drift:\n%s", warnings.String())
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
