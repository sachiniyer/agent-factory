package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	stubRootProgramResolution(t, manager, nil)

	// ensureRootAgentsAndWait joins the drift worker, and the worker commits the
	// warning before it exits, so the log is complete when it returns: there is
	// nothing left to wait for, and no window for a loaded runner to overrun.
	manager.ensureRootAgentsAndWait()
	manager.ensureRootAgentsAndWait()

	got := warnings.String()
	for _, want := range []string{repoPath, `configured command "codex"`, `running command "claude"`, "restart the daemon, then kill the root"} {
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
	stubRootProgramResolution(t, manager, nil)

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
		waitForRootProgramResolutionIdle(t, manager, st)
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

func TestApplyConfigInvalidatesAdoptedRootProgramResolution(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	cfg := rootTestConfig(repoPath, config.RootAgentConfig{Program: "codex"})
	cfg.ProgramOverrides = map[string]string{"codex": "/old/codex"}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t, cfg)
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
	manager.mu.Lock()
	before := st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if before != "/old/codex" {
		t.Fatalf("initial configured command = %q, want /old/codex", before)
	}

	if _, err := config.SetGlobalConfigValue("program_overrides.codex", "/new/codex"); err != nil {
		t.Fatal(err)
	}
	result, err := manager.ApplyConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.Applied, "program_overrides") {
		t.Fatalf("ApplyConfig result = %+v, want program_overrides applied live", result)
	}

	manager.ensureRootAgentsAndWait()
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	after := st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if after != "/new/codex" {
		t.Fatalf("configured command after live apply = %q, want /new/codex", after)
	}
	if !strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("live override drift was hidden by the pre-apply cache:\n%s", warnings.String())
	}
}

func TestApplyConfigRejectsPreApplyRootProgramResolutionCompletion(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	cfg := rootTestConfig(repoPath, config.RootAgentConfig{Program: "codex"})
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t, cfg)
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	root := findRootInstance(t, manager, repoPath)
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "claude"))
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	st := &rootEnsureState{programDriftResolving: true, programDriftResolvingEpoch: 0}
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	manager.mu.Lock()
	manager.rootEnsureStates[repoPath] = st
	manager.mu.Unlock()
	evidence := root.ObserveRuntimeProgram()

	if _, err := manager.ApplyConfig(); err != nil {
		t.Fatal(err)
	}
	manager.finishAdoptedRootProgramDrift(repo.ID, key, repoPath, st,
		config.RootAgent{Enabled: true, Program: "codex"}, 0, "", "codex", nil, root, evidence)

	manager.mu.Lock()
	resolved, configured := st.programDriftResolved, st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if resolved || configured != "" {
		t.Fatalf("pre-apply completion repopulated invalidated cache: resolved=%v configured=%q", resolved, configured)
	}
	if strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("pre-apply completion emitted a warning after invalidation:\n%s", warnings.String())
	}
}

func TestAdoptedRootProgramCacheRevalidatesCheckedInConfig(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	previousInterval := rootProgramDriftConfigInspectionInterval
	rootProgramDriftConfigInspectionInterval = 0
	t.Cleanup(func() { rootProgramDriftConfigInspectionInterval = previousInterval })
	repoPath := setupControlRepo(t)
	// Relative spellings: checked-in program_overrides may not name absolute
	// executables (#5265 review).
	if err := writeRootDriftRepoConfig(repoPath, "[program_overrides]\ncodex = 'old/codex'\n"); err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t,
		rootTestConfig(repoPath, config.RootAgentConfig{Program: "codex"}))
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: repoPath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	findRootInstance(t, manager, repoPath).SetTmuxSession(tmux.NewTmuxSession("root-runtime", "old/codex"))

	manager.ensureRootAgentsAndWait()
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("ensure state was not created")
	}
	waitForRootProgramResolutionIdle(t, manager, st)

	if err := writeRootDriftRepoConfig(repoPath, "[program_overrides]\ncodex = 'new/codex'\n"); err != nil {
		t.Fatal(err)
	}
	manager.ensureRootAgentsAndWait()
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	configured := st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if configured != "new/codex" {
		t.Fatalf("configured command after checked-in edit = %q, want new/codex", configured)
	}
	if !strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("checked-in override drift was hidden by the stale cache:\n%s", warnings.String())
	}
}

func TestAdoptedRootProgramDriftLatchRejectsReplacedRuntime(t *testing.T) {
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
	profile := config.RootAgent{Enabled: true, Program: "/opt/codex"}
	st := &rootEnsureState{
		programDriftResolved:          true,
		programDriftResolvedRepoID:    repo.ID,
		programDriftResolvedWorkspace: repo.WorkspacePath(),
		programDriftResolvedProfile:   profile,
		programDriftConfiguredProgram: "/opt/codex",
	}
	st.programDriftBeforeLatchForTest = func() {
		root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "/opt/codex"))
	}
	manager.checkAdoptedRootProgramDrift(repo,
		daemonInstanceKey(repo.ID, session.RootSessionTitle), repo.WorkspacePath(), st, profile, root, nil)

	manager.mu.Lock()
	latched := st.programDriftLogged || manager.rootProgramDriftLogged[repo.ID]
	manager.mu.Unlock()
	if latched || strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("superseded runtime evidence was permanently latched: latched=%v warnings=%s", latched, warnings.String())
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

func TestCreatedRootWithPaddedProgramPreservesLaunchBytesWithoutDrift(t *testing.T) {
	testguard.IsolateTmux(t)
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "codex")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf 'ready\\n❯\\n›\\n> \\n╰\\n'\n"+testguard.BoundedSpin(time.Second, 5*time.Minute)+"\n"), 0o700); err != nil {
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
	if got := root.RuntimeProgram(); got != padded {
		t.Fatalf("real launch recorded RuntimeProgram %q, want configured bytes %q", got, padded)
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

func TestCreatedRootDefaultProfileStopsAfterTwoOverrideLookups(t *testing.T) {
	testguard.IsolateTmux(t)
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	shimDir := t.TempDir()
	readyScript := []byte("#!/bin/sh\nprintf 'ready\\n❯\\n›\\n> \\n╰\\n'\n" + testguard.BoundedSpin(time.Second, 5*time.Minute) + "\n")
	geminiShim := filepath.Join(shimDir, "gemini")
	thirdLookup := filepath.Join(shimDir, "third-lookup")
	for _, path := range []string{geminiShim, thirdLookup} {
		if err := os.WriteFile(path, readyScript, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := rootTestConfig(repoPath, config.RootAgentConfig{})
	cfg.ProgramOverrides = map[string]string{
		"claude": "codex",
		"codex":  "gemini",
		"gemini": thirdLookup,
	}
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
	if got := root.RuntimeProgram(); got != "gemini" {
		t.Errorf("real root launch recorded RuntimeProgram %q, want the two-stage result gemini", got)
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
		t.Fatalf("freshly created root reported drift after an extra override lookup:\n%s", warnings.String())
	}
}

func TestAdoptedRootProgramResolutionFailureRetries(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	previousInterval := rootProgramDriftConfigInspectionInterval
	rootProgramDriftConfigInspectionInterval = 0
	t.Cleanup(func() { rootProgramDriftConfigInspectionInterval = previousInterval })
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
	manager.checkAdoptedRootProgramDrift(repo, key, repoPath, st, config.RootAgent{}, root, nil)
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
	manager.checkAdoptedRootProgramDrift(repo, key, repoPath, st, config.RootAgent{}, root, nil)
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
		daemonInstanceKey(firstRepo.ID, session.RootSessionTitle), workspace, st, config.RootAgent{}, root, nil)
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
		daemonInstanceKey(secondRepo.ID, session.RootSessionTitle), workspace, st, config.RootAgent{}, root, nil)
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	configured := st.programDriftConfiguredProgram
	manager.mu.Unlock()
	if configured != "gemini" {
		t.Fatalf("repointed workspace reused configured command %q, want gemini", configured)
	}
}

func TestResolveAdoptedRootProgramDriftRequiresStableRegisteredCheckout(t *testing.T) {
	t.Run("unchanged checkout", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		repoPath := setupControlRepo(t)
		project := registerTestProject(t, repoPath)
		repo, err := config.RepoFromPath(project.Root)
		if err != nil {
			t.Fatal(err)
		}
		identity := &resolvedProjectRoot{
			root: project.Root, projectID: project.ID, checkoutID: project.CheckoutID,
		}
		resolve := func(*config.RepoContext) (*config.ResolvedConfig, error) {
			resolved := config.DefaultConfig()
			resolved.ProgramOverrides = map[string]string{"codex": "/registered/codex"}
			return &config.ResolvedConfig{Config: *resolved}, nil
		}
		configured, err := resolveAdoptedRootProgramForDrift(repo,
			config.RootAgent{Enabled: true, Program: "codex"}, identity, resolve)
		if err != nil {
			t.Fatal(err)
		}
		if configured != "/registered/codex" {
			t.Fatalf("configured command = %q, want /registered/codex", configured)
		}
	})

	t.Run("replacement during command read", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		repoPath := setupControlRepo(t)
		project := registerTestProject(t, repoPath)
		repo, err := config.RepoFromPath(project.Root)
		if err != nil {
			t.Fatal(err)
		}
		identity := &resolvedProjectRoot{
			root: project.Root, projectID: project.ID, checkoutID: project.CheckoutID,
		}
		resolve := func(*config.RepoContext) (*config.ResolvedConfig, error) {
			if err := os.Rename(project.Root, project.Root+".original"); err != nil {
				return nil, err
			}
			setupRootDriftRepoAt(t, project.Root)
			resolved := config.DefaultConfig()
			resolved.ProgramOverrides = map[string]string{"codex": "/replacement/codex"}
			return &config.ResolvedConfig{Config: *resolved}, nil
		}
		configured, err := resolveAdoptedRootProgramForDrift(repo,
			config.RootAgent{Enabled: true, Program: "codex"}, identity, resolve)
		if err == nil {
			t.Fatalf("replacement checkout command %q was accepted under the original personal profile", configured)
		}
		if !strings.Contains(err.Error(), "no longer carries") {
			t.Fatalf("replacement refusal = %v, want checkout-marker mismatch", err)
		}
	})
}

func TestAdoptedRootExplicitProgramRejectsReplacedRegisteredCheckout(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	repo, err := config.RepoFromPath(project.Root)
	if err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t, config.DefaultConfig())
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: project.Root, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	root := findRootInstance(t, manager, project.Root)
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", "claude"))
	identity := &resolvedProjectRoot{
		root: project.Root, projectID: project.ID, checkoutID: project.CheckoutID,
	}
	if err := os.Rename(project.Root, project.Root+".original"); err != nil {
		t.Fatal(err)
	}
	setupRootDriftRepoAt(t, project.Root)

	st := &rootEnsureState{}
	manager.checkAdoptedRootProgramDrift(repo,
		daemonInstanceKey(repo.ID, session.RootSessionTitle), project.Root, st,
		config.RootAgent{Enabled: true, Program: "/opt/codex"}, root, identity)
	waitForRootProgramResolutionIdle(t, manager, st)
	manager.mu.Lock()
	resolved := st.programDriftResolved
	latched := st.programDriftLogged || manager.rootProgramDriftLogged[repo.ID]
	manager.mu.Unlock()
	if resolved || latched || strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("replaced checkout produced stale explicit-program drift: resolved=%v latched=%v warnings=%s",
			resolved, latched, warnings.String())
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
	root.SetStatusForTest(session.Running)
	st := &rootEnsureState{programDriftResolving: true}
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	evidence := root.ObserveRuntimeProgram()
	if err := root.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatal(err)
	}
	manager.finishAdoptedRootProgramDrift(repo.ID, key, repoPath, st,
		config.RootAgent{Enabled: true, Program: "codex"}, 0, "", "codex", nil, root, evidence)
	if strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("completion latched drift from the replaced runtime:\n%s", warnings.String())
	}
	manager.mu.Lock()
	logged := st.programDriftLogged || manager.rootProgramDriftLogged[repo.ID]
	manager.mu.Unlock()
	if logged {
		t.Fatal("stale runtime evidence permanently latched the repository drift bit")
	}
	if !st.programDriftLatchPending {
		t.Fatal("rejected latch did not preserve the cached command for fresh runtime evidence")
	}

	previousResolve := resolveRootProgramConfigForInspection
	resolveCalls := 0
	resolveRootProgramConfigForInspection = func(*config.RepoContext, *config.Config) (*config.ResolvedConfig, error) {
		resolveCalls++
		return nil, fmt.Errorf("cached latch retry unexpectedly started a config reader")
	}
	t.Cleanup(func() { resolveRootProgramConfigForInspection = previousResolve })
	manager.checkAdoptedRootProgramDrift(repo, key, repo.WorkspacePath(), st,
		config.RootAgent{Enabled: true, Program: "codex"}, root, nil)
	if resolveCalls != 0 {
		t.Fatalf("cached latch retry started %d config reader(s), want zero", resolveCalls)
	}
	if !strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("cached command was not retried against fresh runtime evidence:\n%s", warnings.String())
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
	resolvedBase := t.TempDir()
	base := filepath.Join(t.TempDir(), "temp-root")
	if err := os.Symlink(resolvedBase, base); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(base, "seed")
	barePath := filepath.Join(base, "identity.git")
	worktreePath := filepath.Join(base, "checkout")
	setupRootDriftRepoAt(t, seedPath)
	// Relative spelling: checked-in program_overrides may not name absolute
	// executables (#5265 review).
	if err := writeRootDriftRepoConfig(seedPath, "[program_overrides]\ncodex = 'repo/codex'\n"); err != nil {
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
	// worktreePath keeps the caller's symlink spelling. Registration resolves
	// it before recording Project.Root, which is also the singleton snapshot's
	// command-resolution path and ensure-state key.
	registeredWorktreePath := project.Root
	if registeredWorktreePath == worktreePath {
		t.Fatal("symlinked fixture path unexpectedly matched the registry-resolved root")
	}
	projectConfig, err := config.ProjectConfigTomlPath(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("[root_agent]\nenabled = true\nprogram = 'codex'\n\n[program_overrides]\ncodex = '/personal/codex'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, warnings := newManagerCapturingWarnings(t, config.DefaultConfig())
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: session.RootSessionTitle, RepoPath: registeredWorktreePath, Program: "claude", InPlace: true, allowReserved: true,
	}); err != nil {
		t.Fatalf("create pre-existing root: %v", err)
	}
	findRootInstance(t, manager, registeredWorktreePath).SetTmuxSession(tmux.NewTmuxSession("root-runtime", "/personal/codex"))

	manager.ensureRootAgentsAndWait()
	manager.mu.Lock()
	st := manager.rootEnsureStates[registeredWorktreePath]
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

// seedAdoptedRootDriftFixture builds a manager holding one adopted-looking
// root whose runtime evidence reports running, plus an ensure-state whose
// cached resolution says the frozen profile wants configured — the shape
// TestAdoptedRootProgramDriftLatchRejectsReplacedRuntime seeds, so a direct
// checkAdoptedRootProgramDrift pass reaches the latch without the ensure
// loop's asynchronous config reader.
func seedAdoptedRootDriftFixture(t *testing.T, configured, running string) (*Manager, *logCapture, *config.RepoContext, *rootEnsureState, config.RootAgent, *session.Instance) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, warnings := newManagerCapturingWarnings(t,
		rootTestConfig(repoPath, config.RootAgentConfig{Program: configured}))
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
	root.SetTmuxSession(tmux.NewTmuxSession("root-runtime", running))
	profile := config.RootAgent{Enabled: true, Program: configured}
	st := &rootEnsureState{
		programDriftResolved:          true,
		programDriftResolvedRepoID:    repo.ID,
		programDriftResolvedWorkspace: repo.WorkspacePath(),
		programDriftResolvedProfile:   profile,
		programDriftConfiguredProgram: configured,
	}
	return manager, warnings, repo, st, profile, root
}

// THE BUG (#5504): the drift latch compared configured and running commands
// byte-for-byte, so an adopted root whose stored command only differs in
// quoting warned "restart the daemon, then kill the root" over nothing. The
// comparison must be on parsed argv; commands that are not plain literal word
// lists keep the byte compare as their whole verdict.
func TestAdoptedRootProgramDriftComparesArgvNotBytes(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured string
		running    string
		wantDrift  bool
		wantSignal string
	}{
		{"single-quoted configured command",
			`'/opt/af-detected/claude' --dangerously-skip-permissions`,
			"/opt/af-detected/claude --dangerously-skip-permissions", false, ""},
		{"double-quoted configured command",
			`"/opt/af-detected/claude" --dangerously-skip-permissions`,
			"/opt/af-detected/claude --dangerously-skip-permissions", false, ""},
		{"quoted flag word",
			`/opt/af-detected/claude '--dangerously-skip-permissions'`,
			"/opt/af-detected/claude --dangerously-skip-permissions", false, ""},
		// Byte-identical commands are no drift even when the parse cannot
		// vouch for them — the byte compare remains the fallback verdict.
		{"byte-equal non-literal stays quiet",
			`claude "$(cat creds)"`, `claude "$(cat creds)"`, false, ""},
		{"quoted glob word stays quiet",
			`claude '*'`, `claude "*"`, false, ""},
		{"escaped-space spelling stays quiet",
			`claude 'a b'`, `claude a\ b`, false, "",
		},
		{"apostrophe escape idiom stays quiet",
			`'/o'\''b/claude' --x`, `"/o'b/claude" --x`, false, "",
		},
		{"an extra argument is real drift",
			"/opt/af-detected/claude --dangerously-skip-permissions",
			"/opt/af-detected/claude --dangerously-skip-permissions --verbose", true,
			"word count differs (configured 2, running 3)"},
		{"non-literal byte difference still warns",
			`claude "$(cat creds)"`, `claude $(cat creds)`, true,
			"command text differs"},
		{"unquoted glob is real drift",
			`claude '*'`, `claude *`, true,
			"command text differs"},
		{"unquoted tilde is real drift",
			`claude '~'`, `claude ~`, true,
			"command text differs"},
		{"nested brace expansion is real drift",
			`claude '{a{b},c}'`, `claude {a{b},c}`, true,
			"command text differs"},
		{"double-quoted escape differs from single-quoted",
			`claude "a\$b"`, `claude 'a\$b'`, true,
			"word 1 differs"},
		{"separator past unmarked brace close is real drift",
			`claude '{a},b}'`, `claude {a},b}`, true,
			"command text differs"},
		{"unquoted keyword is real drift",
			"time claude", "'time' claude", true,
			"command text differs"},
		{"escaped keyword spelling stays quiet",
			`ti\me claude`, "'time' claude", false, ""},
		{"non-assignment colon-tilde stays quiet",
			`'/tmp/a:~/claude' --x`, "/tmp/a:~/claude --x", false, ""},
		{"argument colon-tilde stays quiet",
			`claude 'x=a:~/x'`, "claude x=a:~/x", false, ""},
		{"command-leading assignment word is real drift",
			`'x=a:~/claude'`, "x=a:~/claude", true,
			"command text differs"},
		{"continuation-joined keyword is real drift",
			"'time' claude", "ti\\\nme claude", true,
			"command text differs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, warnings, repo, st, profile, root :=
				seedAdoptedRootDriftFixture(t, test.configured, test.running)
			manager.checkAdoptedRootProgramDrift(repo,
				daemonInstanceKey(repo.ID, session.RootSessionTitle),
				repo.WorkspacePath(), st, profile, root, nil)

			manager.mu.Lock()
			latched := st.programDriftLogged || manager.rootProgramDriftLogged[repo.ID]
			pending := st.programDriftLatchPending
			manager.mu.Unlock()
			got := warnings.String()
			warned := strings.Contains(got, "root agent program drift")
			if warned != test.wantDrift || latched != test.wantDrift || pending {
				t.Fatalf("drift verdict = (warned %v, latched %v, pending %v), want drift %v:\n%s",
					warned, latched, pending, test.wantDrift, got)
			}
			if test.wantSignal != "" && !strings.Contains(got, "("+test.wantSignal+")") {
				t.Fatalf("warning missing distinguishing signal %q:\n%s", test.wantSignal, got)
			}
		})
	}
}

// A latch conclusion that could not commit last sweep is carried as
// programDriftLatchPending and retried on the next pass — the only drift
// verdict that survives across ensure passes. If that carry was seeded while
// the compare was byte-based, the retried pass under the argv compare must
// find the quoting-only pair equal, consume the pending bit, and stay quiet
// (approval note 2). A committed latch cannot reach this code from an old
// binary at all: it lives only in Manager/rootEnsureState memory and dies
// with the daemon process the upgrade replaces.
func TestAdoptedRootProgramDriftPendingLatchGoesQuietWhenArgvEqual(t *testing.T) {
	manager, warnings, repo, st, profile, root := seedAdoptedRootDriftFixture(t,
		`'/opt/af-detected/claude' --dangerously-skip-permissions`,
		"/opt/af-detected/claude --dangerously-skip-permissions")
	st.programDriftLatchPending = true

	manager.checkAdoptedRootProgramDrift(repo,
		daemonInstanceKey(repo.ID, session.RootSessionTitle),
		repo.WorkspacePath(), st, profile, root, nil)

	manager.mu.Lock()
	pending := st.programDriftLatchPending
	latched := st.programDriftLogged || manager.rootProgramDriftLogged[repo.ID]
	manager.mu.Unlock()
	if pending || latched || strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("carried quoting-only latch did not go quiet: pending=%v latched=%v warnings=%s",
			pending, latched, warnings.String())
	}
}

// A seedable fully-committed latch can only model what an old binary produced
// in memory — under this binary a latch is committed only after the argv
// compare already found real drift. Whatever its provenance, a carried latch
// must never emit a second warning for an argv-equal pair.
func TestAdoptedRootProgramDriftLoggedLatchNeverWarnsAgain(t *testing.T) {
	manager, warnings, repo, st, profile, root := seedAdoptedRootDriftFixture(t,
		`'/opt/af-detected/claude' --dangerously-skip-permissions`,
		"/opt/af-detected/claude --dangerously-skip-permissions")
	st.programDriftLogged = true
	st.programDriftLoggedRepoID = repo.ID
	manager.mu.Lock()
	manager.rootProgramDriftLogged[repo.ID] = true
	manager.mu.Unlock()

	manager.checkAdoptedRootProgramDrift(repo,
		daemonInstanceKey(repo.ID, session.RootSessionTitle),
		repo.WorkspacePath(), st, profile, root, nil)

	if strings.Contains(warnings.String(), "root agent program drift") {
		t.Fatalf("a carried latch re-warned for an argv-equal pair:\n%s", warnings.String())
	}
}

// The redaction oracle (#5504, approval note 3): when a real drift collapses
// both commands to the same agent name, the warning must still distinguish
// the two fragments — and must carry neither a raw path nor a flag value.
func TestAdoptedRootProgramDriftWarningDistinguishesIdenticalRedactions(t *testing.T) {
	for _, test := range []struct {
		name        string
		configured  string
		running     string
		wantSignal  string
		secretParts []string
	}{
		{"executable differs",
			"/home/dweller-alpha/bin/claude --settings ALPHA-SECRET",
			"/home/dweller-beta/bin/claude --settings BETA-SECRET",
			"executable differs",
			[]string{"dweller-alpha", "dweller-beta", "ALPHA-SECRET", "BETA-SECRET"}},
		{"same executable, argument differs",
			"/opt/agents/claude --token ALPHA-SECRET",
			"/opt/agents/claude --token BETA-SECRET",
			"word 2 differs",
			[]string{"ALPHA-SECRET", "BETA-SECRET", "--token"}},
		{"word count differs",
			"/opt/agents/claude",
			"/opt/agents/claude --token BETA-SECRET",
			"word count differs (configured 1, running 3)",
			[]string{"BETA-SECRET", "--token"}},
		{"non-literal byte drift",
			`claude $(cat ALPHA-SECRET)`,
			`claude "$(cat ALPHA-SECRET)"`,
			"command text differs",
			[]string{"ALPHA-SECRET", "$(cat", "cat "}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, warnings, repo, st, profile, root :=
				seedAdoptedRootDriftFixture(t, test.configured, test.running)
			manager.checkAdoptedRootProgramDrift(repo,
				daemonInstanceKey(repo.ID, session.RootSessionTitle),
				repo.WorkspacePath(), st, profile, root, nil)

			got := warnings.String()
			line := ""
			for _, candidate := range strings.Split(got, "\n") {
				if strings.Contains(candidate, "root agent program drift") {
					line = candidate
					break
				}
			}
			if line == "" {
				t.Fatalf("real drift produced no warning:\n%s", got)
			}
			for _, secret := range test.secretParts {
				if strings.Contains(line, secret) {
					t.Errorf("warning leaked %q:\n%s", secret, line)
				}
			}
			if !strings.Contains(line, `configured command "claude"`) ||
				!strings.Contains(line, `running command "claude"`) {
				t.Errorf("warning must still show the redacted agent names:\n%s", line)
			}
			if !strings.Contains(line, "("+test.wantSignal+")") {
				t.Errorf("warning missing distinguishing signal %q:\n%s", test.wantSignal, line)
			}
			configuredFrag := line[strings.Index(line, "configured command ")+len("configured command "):]
			configuredFrag = configuredFrag[:strings.Index(configuredFrag, " · running command ")]
			runningFrag := line[strings.Index(line, "running command ")+len("running command "):]
			runningFrag = runningFrag[:strings.Index(runningFrag, " · the live root")]
			if configuredFrag == runningFrag {
				t.Errorf("configured and running fragments are identical strings %q:\n%s",
					configuredFrag, line)
			}
		})
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

// stubRootProgramResolution replaces the repository-config resolution the drift
// worker runs with one that returns instantly and cannot fail. The real
// resolution reads the checkout through bounded filesystem and git probes; when
// one of those errors on a loaded runner the worker records a retry backoff
// (rootProgramDriftConfigInspectionInterval) instead of a verdict, so the warning
// the test waits for is not late, it is deferred by product timing the test must
// not depend on (#6126). overrides is the resolved program_overrides map (nil for
// none, so a bare agent name resolves to itself).
//
// The worker is joined before the original is restored: it is a goroutine that
// reads this variable and can outlive the test body.
func stubRootProgramResolution(t *testing.T, manager *Manager, overrides map[string]string) {
	t.Helper()
	previous := resolveRootProgramConfigForInspection
	resolveRootProgramConfigForInspection = func(*config.RepoContext, *config.Config) (*config.ResolvedConfig, error) {
		resolved := config.DefaultConfig()
		resolved.ProgramOverrides = overrides
		return &config.ResolvedConfig{Config: *resolved}, nil
	}
	t.Cleanup(func() {
		manager.waitRootProgramDriftInspections()
		resolveRootProgramConfigForInspection = previous
	})
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
