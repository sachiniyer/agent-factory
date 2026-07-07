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

// TestRootAgentProgramOverrideMatrix pins the #1116-class fix in the root
// profile: the claude-only --dangerously-skip-permissions flag is ensured
// only when the RESOLVED command actually runs claude. A program_overrides
// entry pointing "claude" at a non-claude program (e.g. the play-test
// sandbox's "bash") must launch verbatim — the appended flag would make it
// exit instantly and the root agent would flap forever.
func TestRootAgentProgramOverrideMatrix(t *testing.T) {
	tests := []struct {
		name     string
		override string // program_overrides["claude"]
		want     string
	}{
		{"bare enum appends flag", "claude", "claude --dangerously-skip-permissions"},
		{"claude path override appends flag", "/opt/claude-next/bin/claude --model opus",
			"/opt/claude-next/bin/claude --model opus --dangerously-skip-permissions"},
		{"flag already present not duplicated", "claude --dangerously-skip-permissions",
			"claude --dangerously-skip-permissions"},
		{"non-agent override left verbatim (#1116)", "bash", "bash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := setupControlRepo(t)
			// Every row pins an explicit override entry: with none present,
			// config loading auto-detects a machine-local claude path into
			// ProgramOverrides, which would make the row nondeterministic.
			cfg := config.DefaultConfig()
			cfg.ProgramOverrides = map[string]string{"claude": tt.override}
			if err := config.SaveConfig(cfg); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}
			got := rootAgentProgram(repoPath, config.RootAgentConfig{})
			if got != tt.want {
				t.Fatalf("rootAgentProgram = %q, want %q", got, tt.want)
			}
		})
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
		first.SetStatusForTest(status)
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

	first.SetStatusForTest(session.Dead)
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

// TestEnsureRootAgentsHealsLostRoot mirrors the Dead heal for the Lost status
// (#1108): the liveness probe records an outage-vanished root as Lost now, and
// the ensure loop must treat it exactly like Dead — reap and re-create in
// place — or the #1128 root self-heal would silently regress.
func TestEnsureRootAgentsHealsLostRoot(t *testing.T) {
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

	first.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()

	if len(*seen) != 2 {
		t.Fatalf("expected a re-create after the root went Lost, got %d creates", len(*seen))
	}
	healed := findRootInstance(t, manager, repoPath)
	if healed == nil {
		t.Fatalf("root instance missing after heal")
	}
	if healed == first {
		t.Fatalf("heal must replace the lost instance, not resurrect the same object")
	}
	if got := healed.GetStatus(); got == session.Lost || got == session.Dead {
		t.Fatalf("healed root must be live, got %v", got)
	}
}

// TestEnsureRootAgentsDoesNotAdoptArchivedRoot (#1028): an Archived root is
// inert (no tmux), so the ensure loop must NOT adopt it as live — it must reap
// and re-create in place, exactly like Dead/Lost. Archiving the reserved root
// is rejected upstream by ArchiveSession, so this is the defensive backstop for
// the adopt-never-clobber condition.
func TestEnsureRootAgentsDoesNotAdoptArchivedRoot(t *testing.T) {
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

	first.SetStatusForTest(session.Archived)
	manager.EnsureRootAgents()

	if len(*seen) != 2 {
		t.Fatalf("expected a re-create after the root went Archived (never adopted), got %d creates", len(*seen))
	}
	healed := findRootInstance(t, manager, repoPath)
	if healed == nil {
		t.Fatalf("root instance missing after heal")
	}
	if healed == first {
		t.Fatalf("an archived root must be reaped and replaced, not adopted in place")
	}
	if got := healed.GetStatus(); got == session.Archived {
		t.Fatalf("healed root must be live, got %v", got)
	}
}

// TestEnsureRootAgentsUserKillHealsAfterGraceWindow is the #1223 case-(b)
// regression: an explicit KillSession of the root is honored only briefly —
// within rootKillHealDelay the ensure loop leaves it down, room for a manual
// restart or a deliberate stop — but a still-configured root then SELF-HEALS
// once the grace window elapses, with NO daemon restart. Config (root_agents),
// not a runtime kill-tombstone, is the source of truth for an always-on root.
// The outage this fixes: root killed 10:39, dead until an 11:02 daemon restart
// (~23 min), because the old kill tombstone was permanent.
func TestEnsureRootAgentsUserKillHealsAfterGraceWindow(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	cfg := rootTestConfig(repoPath, config.RootAgentConfig{})

	// Deterministic injected clock — advance it instead of sleeping through the
	// real grace window.
	base := time.Unix(1_700_000_000, 0)
	clock := base
	origNow := nowFunc
	nowFunc = func() time.Time { return clock }
	t.Cleanup(func() { nowFunc = origNow })

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

	// Inside the grace window: the kill is honored, no re-create.
	clock = base.Add(rootKillHealDelay - time.Second)
	manager.EnsureRootAgents()
	manager.EnsureRootAgents()
	if len(*seen) != 1 {
		t.Fatalf("ensure must honor the kill inside the grace window, got %d creates", len(*seen))
	}
	if findRootInstance(t, manager, repoPath) != nil {
		t.Fatalf("root must stay down inside the grace window")
	}

	// Grace window elapsed: a still-configured root self-heals — NO daemon
	// restart, unlike the pre-fix behavior.
	clock = base.Add(rootKillHealDelay + time.Second)
	manager.EnsureRootAgents()
	if len(*seen) != 2 {
		t.Fatalf("configured root must self-heal after the grace window without a daemon restart, got %d creates", len(*seen))
	}
	if findRootInstance(t, manager, repoPath) == nil {
		t.Fatalf("root instance must be back after self-heal")
	}

	// The kill tombstone is cleared, so a later pass just adopts the live root
	// rather than churning creates.
	manager.mu.Lock()
	_, stillKilled := manager.rootKilledAt[repo.ID]
	manager.mu.Unlock()
	if stillKilled {
		t.Fatalf("kill tombstone must be cleared after self-heal")
	}
	manager.EnsureRootAgents()
	if len(*seen) != 2 {
		t.Fatalf("healed root must be adopted, not re-created, got %d creates", len(*seen))
	}
}

// TestKillDeadRootDoesNotDeleteSelfHealedRoot pins #1266: a KillSession that
// resolved dead root-A must not delete a newly self-healed root-B that reused
// the reserved title while the stale kill was waiting on the session op lock.
func TestKillDeadRootDoesNotDeleteSelfHealedRoot(t *testing.T) {
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
		t.Fatalf("expected initial root create, got %d", len(*seen))
	}
	rootA := findRootInstance(t, manager, repoPath)
	if rootA == nil {
		t.Fatal("root-A missing after initial ensure")
	}
	rootA.SetStatusForTest(session.Dead)

	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	opLock := manager.opLockFor(key)
	opLock.Lock()

	killDone := make(chan error, 1)
	go func() {
		killDone <- manager.KillSession(KillSessionRequest{Title: session.RootSessionTitle, RepoID: repo.ID})
	}()
	waitUntil(t, 5*time.Second, "KillSession to resolve root-A and wait on the op lock", func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		_, killing := manager.killsInFlight[key]
		return killing
	})

	storage, err := session.NewStorage(config.LoadState(), repo.ID)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	deleted, err := storage.DeleteInstanceByStableID(session.RootSessionTitle, rootA.ID)
	if err != nil || !deleted {
		t.Fatalf("delete root-A during self-heal: deleted=%v err=%v", deleted, err)
	}
	manager.mu.Lock()
	delete(manager.instances, key)
	manager.mu.Unlock()

	rootBData, err := manager.CreateSession(CreateSessionRequest{
		Title:         session.RootSessionTitle,
		RepoPath:      repo.Root,
		Program:       "claude",
		InPlace:       true,
		allowReserved: true,
	})
	if err != nil {
		t.Fatalf("self-heal create root-B: %v", err)
	}
	if rootBData.ID == "" || rootBData.ID == rootA.ID {
		t.Fatalf("root-B must have a fresh stable ID, rootA=%q rootB=%q", rootA.ID, rootBData.ID)
	}
	rootB := findRootInstance(t, manager, repoPath)
	if rootB == nil || rootB.ID != rootBData.ID {
		t.Fatalf("root-B not registered after self-heal: got %+v want ID %q", rootB, rootBData.ID)
	}

	opLock.Unlock()
	select {
	case err := <-killDone:
		if err != nil {
			t.Fatalf("stale KillSession returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale KillSession did not finish")
	}

	if got := findRootInstance(t, manager, repoPath); got != rootB {
		t.Fatalf("stale kill must leave self-healed root-B registered, got %+v want %+v", got, rootB)
	}
	data, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		t.Fatalf("loadRepoInstanceData: %v", err)
	}
	if len(data) != 1 || data[0].Title != session.RootSessionTitle || data[0].ID != rootBData.ID {
		t.Fatalf("persisted root after stale kill = %+v, want only root-B ID %q", data, rootBData.ID)
	}
	manager.mu.Lock()
	_, killed := manager.rootKilledAt[repo.ID]
	manager.mu.Unlock()
	if killed {
		t.Fatal("stale kill of root-A must not start the root-B kill grace window")
	}
}

// TestEnsureRootAgentsBusyReapSkipDoesNotCreateOrBackoff pins the Greptile
// review on #1272: if the dead-root reap cannot take the per-session op lock,
// ensure must wait for the next tick instead of falling through to CreateSession
// and recording a backoff-causing create failure against the still-owned title.
func TestEnsureRootAgentsBusyReapSkipDoesNotCreateOrBackoff(t *testing.T) {
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
		t.Fatalf("expected initial root create, got %d", len(*seen))
	}
	rootA := findRootInstance(t, manager, repoPath)
	if rootA == nil {
		t.Fatal("root-A missing after initial ensure")
	}
	rootA.SetStatusForTest(session.Dead)

	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	opLock := manager.opLockFor(key)
	opLock.Lock()

	manager.EnsureRootAgents()
	if len(*seen) != 1 {
		t.Fatalf("busy reap skip must not create another root, got %d creates", len(*seen))
	}
	if got := findRootInstance(t, manager, repoPath); got != rootA {
		t.Fatalf("busy reap skip must leave root-A in place, got %+v want %+v", got, rootA)
	}
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("expected root ensure state")
	}
	if st.consecutiveFailures != 0 || !st.nextAttempt.IsZero() {
		t.Fatalf("busy reap skip must not record an ensure failure/backoff: failures=%d next=%v", st.consecutiveFailures, st.nextAttempt)
	}

	opLock.Unlock()
	manager.EnsureRootAgents()
	if len(*seen) != 2 {
		t.Fatalf("next tick after lock release must heal immediately, got %d creates", len(*seen))
	}
	rootB := findRootInstance(t, manager, repoPath)
	if rootB == nil || rootB == rootA {
		t.Fatalf("root must be reaped and replaced after lock release, got %+v", rootB)
	}
}

// TestEnsureRootAgentsDoesNotHealUnconfiguredRoot pins that the self-heal is
// gated on config: a repo NOT in root_agents is never visited by the ensure
// loop, so a killed (or absent) root there is never auto-created — even long
// past the grace window. Removing a repo from root_agents is the ONLY permanent
// stop, and this proves the loop does not spin-spawn roots for unconfigured
// repos (#1223).
func TestEnsureRootAgentsDoesNotHealUnconfiguredRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	base := time.Unix(1_700_000_000, 0)
	clock := base
	origNow := nowFunc
	nowFunc = func() time.Time { return clock }
	t.Cleanup(func() { nowFunc = origNow })

	// No root_agents entry for this repo.
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	// A kill records a tombstone even for an unconfigured repo (harmless
	// bookkeeping — see KillSession).
	manager.mu.Lock()
	manager.rootKilledAt[repo.ID] = base
	manager.mu.Unlock()

	// Well past the grace window, the unconfigured repo is still never visited.
	clock = base.Add(rootKillHealDelay + time.Hour)
	manager.EnsureRootAgents()
	manager.EnsureRootAgents()
	if len(*seen) != 0 {
		t.Fatalf("ensure must never create a root for a repo absent from root_agents, got %d creates", len(*seen))
	}
	if findRootInstance(t, manager, repoPath) != nil {
		t.Fatalf("unconfigured repo must have no root instance")
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
