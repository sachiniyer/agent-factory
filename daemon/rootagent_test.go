package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// All tests here are hermetic (#1106 hard rule): temp AGENT_FACTORY_HOME,
// the in-process fake tmux backend, no real daemon, no real ~/.agent-factory.

// rootTestConfig returns a config that opts repoPath into a root agent.
func rootTestConfig(repoPath string, rc config.RootAgentConfig) *config.Config {
	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{repoPath: rc}
	return cfg
}

// findRootInstance returns the manager's live root instance for the repo, or
// nil.
func findRootInstance(t *testing.T, manager *Manager, repoPath string) *session.Instance {
	t.Helper()
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.instances[daemonInstanceKey(repo.ID, session.RootSessionTitle)]
}

// TestEnsureRootAgentsCreatesInPlaceRoot: a configured repo with no root gets
// one created in place, with the default root profile — resolved claude with
// --dangerously-skip-permissions ensured — and auto_yes on.
func TestEnsureRootAgentsCreatesInPlaceRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("expected 1 create, got %d", len(*seen))
	}
	opts := (*seen)[0]
	if opts.Title != session.RootSessionTitle {
		t.Fatalf("expected title %q, got %q", session.RootSessionTitle, opts.Title)
	}
	if !opts.InPlace {
		t.Fatalf("root agent must be created in place (the #1107 --here shape)")
	}
	if !strings.Contains(opts.Program, "--dangerously-skip-permissions") {
		t.Fatalf("default root profile must carry --dangerously-skip-permissions, got %q", opts.Program)
	}
	if !opts.AutoYes {
		t.Fatalf("default root profile must enable auto_yes")
	}
	if findRootInstance(t, manager, repoPath) == nil {
		t.Fatalf("root instance not registered with the manager")
	}
}

// TestEnsureRootAgentsHonorsProfileOverrides: an explicit program is used
// verbatim and auto_yes=false is respected.
func TestEnsureRootAgentsHonorsProfileOverrides(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	// A full custom command (still claude-flavored so the fake backend's
	// "❯" ready prompt matches and the create never waits out the 60s
	// readiness timeout).
	customProgram := "/opt/claude --model opus"
	autoYes := false
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{
		Program: customProgram,
		AutoYes: &autoYes,
	}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("expected 1 create, got %d", len(*seen))
	}
	if (*seen)[0].Program != customProgram {
		t.Fatalf("explicit root program must be used verbatim, got %q", (*seen)[0].Program)
	}
	if (*seen)[0].AutoYes {
		t.Fatalf("auto_yes=false in the root profile must be respected")
	}
}

// TestEnsureRootAgentsAdoptsLiveRoot: with a live root already present —
// whatever created it — ensure is a strict no-op. Never kill/recreate a live
// root (#1106 adopt-never-clobber rule).
func TestEnsureRootAgentsAdoptsLiveRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()
	if len(*seen) != 1 {
		t.Fatalf("expected initial create, got %d", len(*seen))
	}
	first := findRootInstance(t, manager, repoPath)
	if first == nil {
		t.Fatalf("root instance missing after first ensure")
	}

	for _, status := range []session.Status{session.Running, session.Ready, session.Loading} {
		first.SetStatus(status)
		manager.EnsureRootAgents()
		if len(*seen) != 1 {
			t.Fatalf("ensure over a live root (status %v) must be a no-op, got %d creates", status, len(*seen))
		}
		if got := findRootInstance(t, manager, repoPath); got != first {
			t.Fatalf("ensure over a live root (status %v) must keep the same instance", status)
		}
	}
}

// TestEnsureRootAgentsHealsDeadRoot: a root whose status went Dead (tmux
// vanished — the #1104 outage class) is reaped and re-created in place.
func TestEnsureRootAgentsHealsDeadRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()
	first := findRootInstance(t, manager, repoPath)
	if first == nil {
		t.Fatalf("root instance missing after first ensure")
	}

	first.SetStatus(session.Dead)
	manager.EnsureRootAgents()

	if len(*seen) != 2 {
		t.Fatalf("expected a re-create after the root went Dead, got %d creates", len(*seen))
	}
	healed := findRootInstance(t, manager, repoPath)
	if healed == nil {
		t.Fatalf("root instance missing after heal")
	}
	if healed == first {
		t.Fatalf("heal must replace the dead instance, not resurrect the same object")
	}
	if healed.GetStatus() == session.Dead {
		t.Fatalf("healed root must not be Dead")
	}

	// Exactly one persisted "root" record — the heal replaced, not duplicated.
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	data, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		t.Fatalf("loadRepoInstanceData: %v", err)
	}
	roots := 0
	for _, d := range data {
		if d.Title == session.RootSessionTitle {
			roots++
		}
	}
	if roots != 1 {
		t.Fatalf("expected exactly 1 persisted root record after heal, got %d", roots)
	}
}

// TestEnsureRootAgentsRespectsUserKill: an explicit KillSession of the root
// suppresses re-creation for the rest of the daemon's life; a fresh manager
// (daemon restart) re-asserts the configured root. This is the conservative
// #1108-adjacent shape: respect the explicit kill until daemon restart.
func TestEnsureRootAgentsRespectsUserKill(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	cfg := rootTestConfig(repoPath, config.RootAgentConfig{})

	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()
	if len(*seen) != 1 {
		t.Fatalf("expected initial create, got %d", len(*seen))
	}

	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if err := manager.KillSession(KillSessionRequest{Title: session.RootSessionTitle, RepoID: repo.ID}); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	manager.EnsureRootAgents()
	manager.EnsureRootAgents()
	if len(*seen) != 1 {
		t.Fatalf("ensure must respect an explicit root kill until daemon restart, got %d creates", len(*seen))
	}
	if findRootInstance(t, manager, repoPath) != nil {
		t.Fatalf("killed root must stay gone")
	}

	// A daemon restart (fresh manager over the same home) re-asserts the
	// configured root: the suppression is deliberately in-memory only.
	restarted, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager (restart): %v", err)
	}
	restarted.EnsureRootAgents()
	if len(*seen) != 2 {
		t.Fatalf("a restarted daemon must re-create the configured root, got %d creates", len(*seen))
	}
}

// TestEnsureRootAgentsKeepsRetryingAndHeals is the #1122 outage regression
// test: a failing entry must keep being retried PAST the escalation threshold
// (never permanently dropped — that is what left roots down for hours after
// the 2026-07-03 tmux-server outage), and the first attempt after the cause
// clears must heal the root with no daemon restart.
func TestEnsureRootAgentsKeepsRetryingAndHeals(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	// Zero backoff so every EnsureRootAgents call is an attempt.
	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })

	badPath := filepath.Join(t.TempDir(), "repo") // does not exist yet
	if err := os.MkdirAll(badPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manager, err := NewManager(rootTestConfig(badPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	attempts := rootEnsureEscalationThreshold + 3
	for i := 0; i < attempts; i++ {
		manager.EnsureRootAgents()
	}

	manager.mu.Lock()
	st := manager.rootEnsureStates[badPath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatalf("expected ensure state for %q", badPath)
	}
	if st.consecutiveFailures != attempts {
		t.Fatalf("ensure must keep attempting past the escalation threshold: want %d failures, got %d", attempts, st.consecutiveFailures)
	}

	// The cause clears: the directory becomes a real git repo (stands in for
	// "the tmux server outage ended"). The very next pass must heal.
	for _, args := range [][]string{
		{"init", badPath},
		{"-C", badPath, "config", "user.email", "test@example.com"},
		{"-C", badPath, "config", "user.name", "Test User"},
		{"-C", badPath, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	manager.EnsureRootAgents()

	if findRootInstance(t, manager, badPath) == nil {
		t.Fatalf("first ensure pass after the cause cleared must create the root without a daemon restart")
	}
	manager.mu.Lock()
	failures := st.consecutiveFailures
	manager.mu.Unlock()
	if failures != 0 {
		t.Fatalf("healing must reset the failure counter, got %d", failures)
	}
}

// TestEnsureRootAgentsBacksOffBetweenFailures: after one failure the next
// pass inside the backoff window must not attempt again.
func TestEnsureRootAgentsBacksOffBetweenFailures(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = time.Hour
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })

	badPath := t.TempDir()
	manager, err := NewManager(rootTestConfig(badPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	manager.EnsureRootAgents()
	manager.EnsureRootAgents()
	manager.EnsureRootAgents()

	manager.mu.Lock()
	st := manager.rootEnsureStates[badPath]
	manager.mu.Unlock()
	if st == nil || st.consecutiveFailures != 1 {
		t.Fatalf("passes inside the backoff window must not re-attempt: want 1 failure, got %+v", st)
	}
}

// TestCreateSessionRejectsReservedRootTitle: the daemon chokepoint every
// creation surface funnels through (TUI, CLI, API, task spawns, DeliverPrompt
// auto-creates) rejects the reserved title, case-insensitively.
func TestCreateSessionRejectsReservedRootTitle(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	for _, title := range []string{"root", "Root", "ROOT", " root "} {
		_, err := manager.CreateSession(CreateSessionRequest{
			Title:    title,
			RepoPath: repoPath,
			Program:  "claude",
		})
		if err == nil {
			t.Fatalf("expected reserved title %q to be rejected", title)
		}
		if !strings.Contains(err.Error(), "reserved") || !strings.Contains(err.Error(), "root_agents") {
			t.Fatalf("rejection for %q must name the reservation and the root_agents opt-in, got: %v", title, err)
		}
	}
}

// TestCreateSessionTitleBaseRootSkipsReserved: a derived title (TitleBase,
// used by task spawns) never lands on the reserved name — it skips to the
// next suffix instead of erroring, so a task named "root" still runs.
func TestCreateSessionTitleBaseRootSkipsReserved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(CreateSessionRequest{
		TitleBase: "root",
		RepoPath:  repoPath,
		Program:   "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession with TitleBase root: %v", err)
	}
	if data.Title != "root-2" {
		t.Fatalf("derived title must skip the reserved name to root-2, got %q", data.Title)
	}
}

// TestCreateSessionRPCCannotSetAllowReserved proves the reservation cannot be
// bypassed over the control socket: allowReserved is unexported, gob never
// carries it, so an RPC CreateSession for "root" is rejected even though the
// in-process ensure path can create it.
func TestCreateSessionRPCCannotSetAllowReserved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
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
	err = callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:         "root",
		RepoPath:      repoPath,
		Program:       "claude",
		allowReserved: true, // never crosses gob; must not matter
	}, &resp)
	if err == nil {
		t.Fatalf("reserved title must be rejected over RPC")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reservation error over RPC, got: %v", err)
	}
}

// TestDeliverPromptCannotAutoCreateRoot: the create-or-send path can still
// SEND into a live root (that is how tasks reach it) but must not auto-create
// one where the ensure loop owns creation.
func TestDeliverPromptCannotAutoCreateRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, err = manager.DeliverPrompt(DeliverPromptRequest{
		Title:    session.RootSessionTitle,
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "hello",
	})
	if err == nil {
		t.Fatalf("DeliverPrompt must not auto-create the reserved root session")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reservation error, got: %v", err)
	}
}

// TestRunDaemonEnsuresRootAgent drives the wiring end-to-end: a daemon
// started with a root_agents entry creates the root session from its poll
// loop without any RPC asking for it. Hermetic — temp home, fake backend,
// stubbed legacy-unit sweep, private socket under the temp home.
func TestRunDaemonEnsuresRootAgent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	stubLegacyUnitSweep(t)
	repoPath := setupControlRepo(t)

	cfg := rootTestConfig(repoPath, config.RootAgentConfig{})
	cfg.DaemonPollInterval = 50

	done := make(chan error, 1)
	go func() { done <- RunDaemon(cfg) }()
	t.Cleanup(func() {
		if _, err := RequestShutdown(); err != nil {
			t.Logf("RequestShutdown: %v", err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("RunDaemon did not exit within 5s of Shutdown")
		}
	})

	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var resp SnapshotResponse
		if err := callDaemonNoEnsure("Snapshot", SnapshotRequest{RepoID: repo.ID}, &resp); err == nil {
			for _, inst := range resp.Instances {
				if inst.Title == session.RootSessionTitle {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon did not ensure the root agent within 5s")
}

// TestDeliverPromptSendsIntoEnsuredRoot: once the ensure loop created the
// root, prompt delivery to it works — the path the captain-events watch task
// uses.
func TestDeliverPromptSendsIntoEnsuredRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	status, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:    session.RootSessionTitle,
		RepoPath: repoPath,
		Prompt:   "hello root",
	})
	if err != nil {
		t.Fatalf("DeliverPrompt into live root: %v", err)
	}
	if status != "sent" {
		t.Fatalf("expected status \"sent\" into the live root, got %q", status)
	}
}
