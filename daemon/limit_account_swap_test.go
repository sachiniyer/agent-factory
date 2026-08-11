package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func configureLimitAccountCandidate(t *testing.T, manager *Manager, name string) {
	t.Helper()
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentaccount.Register(home, "claude", name); err != nil {
		t.Fatalf("register account: %v", err)
	}
	configPath := filepath.Join(home, config.TomlConfigFileName)
	contents := "limit_account_candidates = [\"" + name + "\"]\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	manager.Config().LimitAccountCandidates = []string{name}
}

func writeLimitAccountCandidates(t *testing.T, contents string) {
	t.Helper()
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestAdmitAccountSwapRefusesUnreadablePersonalIdentityPolicy(t *testing.T) {
	manager, _, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")

	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(home, config.ProjectRegistryDirName)
	if err := os.WriteFile(registryPath, []byte("not a project registry"), 0o600); err != nil {
		t.Fatalf("corrupt project registry: %v", err)
	}
	if _, err := config.ListProjects(); err == nil {
		t.Fatal("precondition: corrupt project registry must be unreadable")
	}

	if err := inst.BeginLimitResume(); err != nil {
		t.Fatal(err)
	}
	defer inst.EndLimitResume()
	swap, err := manager.admitAccountSwap(inst, manager.Config())
	if err == nil {
		t.Fatalf("automatic identity decision inherited global candidate despite unreadable personal policy: %+v", swap)
	}
	if swap != nil {
		t.Fatalf("automatic identity decision returned a swap with unresolved personal policy: %+v", swap)
	}
}

func TestResumeLimitedSessions_SwapsAmbientSessionToConfiguredUnblockedAccount(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")

	// The account choice is immediate; it must not wait for the old identity's
	// future reset window.
	advance(time.Second)
	manager.ResumeLimitedSessions()

	_, respawns, prompts := backend.snapshot()
	if respawns != 1 {
		t.Fatalf("account replacement respawns = %d, want 1", respawns)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], `claude account "work"`) ||
		!strings.Contains(prompts[0], "finish the migration") {
		t.Fatalf("swap prompt must name the identity change and retain the task, got %q", prompts)
	}
	account, automatic := inst.AccountSelection()
	if account != "work" || !automatic {
		t.Fatalf("account selection = (%q, %v), want scheduler-selected work", account, automatic)
	}
	if inst.LimitReached() {
		t.Fatal("successful account replacement must clear the old limit wall")
	}
}

func TestResumeLimitedSessions_AllConfiguredAccountsLimitedKeepsWaiting(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "wait", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")

	otherBackend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	other := registerStarted(t, manager, repoID, inst.Path, "work-limited", otherBackend, true, session.Running)
	other.Account = "work"
	other.SetLimitReached(base.Add(time.Hour))

	advance(time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("subject resumed onto an exhausted candidate: %v", prompts)
	}
	if account, _ := inst.AccountSelection(); account != "" {
		t.Fatalf("subject account changed to %q with no unblocked candidate", account)
	}
}

func TestAccountSwapOpportunityRetainsEarlierAccountLimitEvidence(t *testing.T) {
	manager, _, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Time{})
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"work", "personal"} {
		if _, err := agentaccount.Register(home, tmux.ProgramClaude, name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	writeLimitAccountCandidates(t, "limit_account_candidates = [\"work\", \"personal\"]\n")
	manager.Config().LimitAccountCandidates = []string{"work", "personal"}

	// Model two completed moves without involving transport: ambient -> work,
	// then work -> personal. The work wall must remain evidence after its own
	// replacement succeeds and clears the session's current limit liveness.
	requireSwapSelection := func(from, to string) {
		t.Helper()
		if err := inst.BeginLimitResume(); err != nil {
			t.Fatal(err)
		}
		if _, err := inst.SelectAccountAutomatically(from, to); err != nil {
			t.Fatal(err)
		}
		inst.EndLimitResume()
		if !inst.ClearPendingAccountSwap(from, to) {
			t.Fatalf("clear pending %q -> %q", from, to)
		}
		inst.ClearLimitReached()
	}
	requireSwapSelection("", "work")
	inst.SetLimitReached(time.Time{})
	requireSwapSelection("work", "personal")
	inst.SetLimitReached(time.Time{})

	swap, err := manager.accountSwapOpportunityFromFacts(inst, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if swap != nil {
		t.Fatalf("known-limited work account was selected again after moving to personal: %#v", swap)
	}
}

func TestAccountSwapOpportunity_RetainsObservationAfterSessionKill(t *testing.T) {
	base := nowFunc()
	manager, repoID, target, _ := newAutoResumeManager(t, "", true, "continue", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")

	observer := registerStarted(t, manager, repoID, target.Path, "work-limited",
		session.NewFakeBackend(), true, session.Running)
	observer.Account = "work"
	observer.SetLimitReached(base.Add(time.Hour))
	if _, err := manager.KillSession(KillSessionRequest{Title: observer.Title, RepoID: repoID}); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	restarted, err := NewManager(manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	swap, err := restarted.accountSwapOpportunityFromFacts(target, restarted.Config())
	if err != nil {
		t.Fatal(err)
	}
	if swap != nil {
		t.Fatalf("deleted session's known-limited account became eligible after restart: %#v", swap)
	}
}

func TestAccountSwapOpportunity_UsesHistoricalObservationAfterAgentHandoff(t *testing.T) {
	base := nowFunc()
	manager, repoID, target, _ := newAutoResumeManager(t, "", true, "continue", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")

	observer := registerStarted(t, manager, repoID, target.Path, "handed-off",
		session.NewFakeBackend(), true, session.Running)
	observer.Account = "work"
	observer.SetLimitReached(base.Add(time.Hour))
	observer.ClearLimitReached()
	observer.Program = tmux.ProgramCodex

	swap, err := manager.accountSwapOpportunityFromFacts(target, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if swap != nil {
		t.Fatalf("historical Claude limit evidence vanished after handoff to Codex: %#v", swap)
	}
}

func TestAccountSwapOpportunity_UsesObservationFromUnloadablePersistedSession(t *testing.T) {
	installInstantBackend(t)
	manager, repoID, target, _ := newAutoResumeManager(t, "", true, "continue", time.Time{})
	configureLimitAccountCandidate(t, manager, "work")

	observer := registerStarted(t, manager, repoID, target.Path, "unloadable-work-limit",
		session.NewFakeBackend(), true, session.Running)
	observer.Account = "work"
	observer.SetLimitReached(time.Time{})
	manager.persistInstance(repoID, observer)

	// A restart can parse this row but fail to materialize its Instance. The raw
	// durable observation must still exclude the exhausted identity even though
	// the row contributes nothing to the manager's in-memory instance map.
	failLoadFor(t, observer.Title)
	loaded, _, err := refreshDaemonInstances(nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded[daemonInstanceKey(repoID, observer.Title)] != nil {
		t.Fatal("fixture observation row unexpectedly materialized")
	}
	restarted, err := NewManager(manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	restarted.mu.Lock()
	restarted.instances = loaded
	restarted.mu.Unlock()

	swap, err := restarted.accountSwapOpportunityFromFacts(target, restarted.Config())
	if err != nil {
		t.Fatal(err)
	}
	if swap != nil {
		t.Fatalf("unloadable session's known-limited account became eligible after restart: %#v", swap)
	}
}

func TestResumeFromLimitPromotesPendingClaudeConversationBeforeClear(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "continue", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	backend.sendPromptErr = errors.New("hold the durable marker")

	advance(time.Second)
	manager.ResumeLimitedSessions()
	data := inst.ToInstanceData()
	if data.PendingAccountSwap == nil || data.PendingAccountSwap.ConversationID == "" {
		t.Fatalf("committed Claude swap did not persist its replacement conversation: %+v", data.PendingAccountSwap)
	}
	want := data.PendingAccountSwap.ConversationID
	if conv := inst.AgentConversation(); conv.Agent != tmux.ProgramClaude || conv.ID != want {
		t.Fatalf("failed delivery did not promote durable pending conversation: %+v, want Claude %s", conv, want)
	}

	backend.mu.Lock()
	backend.sendPromptErr = nil
	backend.mu.Unlock()
	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatal(err)
	}
	if conv := inst.AgentConversation(); conv.Agent != tmux.ProgramClaude || conv.ID != want {
		t.Fatalf("settled replacement conversation = %+v, want Claude %s", conv, want)
	}
	if _, _, pending := inst.PendingAccountSwap(); pending {
		t.Fatal("successful retry retained pending marker")
	}
}

func TestResumeLimitedSessionsCapturesReplacementCodexConversation(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", false, "continue", base.Add(time.Hour))
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	accountHome, err := agentaccount.Register(home, tmux.ProgramCodex, "work")
	if err != nil {
		t.Fatal(err)
	}
	writeLimitAccountCandidates(t, "limit_account_candidates = [\"work\"]\n")
	manager.Config().LimitAccountCandidates = []string{"work"}
	inst.Program = tmux.ProgramCodex
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramCodex))
	worktree := filepath.Join(t.TempDir(), "codex-worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(
		inst.Path, worktree, inst.Title, "codex-branch", "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetGitWorktreeForTest(gw)
	backend.onRespawn = func(*session.Instance) {
		writeDaemonCodexRolloutFileWithCwd(t, accountHome,
			"rollout-2026-08-10T12-00-00-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", worktree)
	}

	advance(time.Second)
	manager.ResumeLimitedSessions()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conv := inst.AgentConversation(); conv.Agent == tmux.ProgramCodex &&
			conv.ID == "019f386f-7206-7fc2-803b-f7045e07a242" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("replacement Codex conversation was never captured: %+v", inst.AgentConversation())
}

func TestResumeFromLimit_LiveCommittedCodexSwapRecapturesBeforeClearingMarker(t *testing.T) {
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Time{})
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	accountHome, err := agentaccount.Register(home, tmux.ProgramCodex, "work")
	if err != nil {
		t.Fatal(err)
	}
	inst.Program = tmux.ProgramCodex
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramCodex))
	worktree := filepath.Join(t.TempDir(), "codex-worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(
		inst.Path, worktree, inst.Title, "codex-branch", "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetGitWorktreeForTest(gw)

	// Model the restart window: the account selection and replacement are live,
	// but the old daemon exited before its asynchronous capture could persist the
	// new Codex conversation id.
	if err := inst.BeginLimitResume(); err != nil {
		t.Fatal(err)
	}
	if err := inst.ValidateAccountSwap("work"); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.SelectAccountAutomatically("", "work"); err != nil {
		t.Fatal(err)
	}
	inst.EndLimitResume()
	backend.onRespawn = func(*session.Instance) {
		writeDaemonCodexRolloutFileWithCwd(t, accountHome,
			"rollout-2026-08-10T12-00-00-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", worktree)
	}

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatal(err)
	}
	_, respawns, _ := backend.snapshot()
	if respawns != 1 {
		t.Fatalf("live committed Codex replacement respawns = %d, want 1 recapture launch", respawns)
	}
	if conv := inst.AgentConversation(); conv.Agent != tmux.ProgramCodex ||
		conv.ID != "019f386f-7206-7fc2-803b-f7045e07a242" {
		t.Fatalf("committed replacement cleared without durable Codex conversation: %+v", conv)
	}
	if _, _, pending := inst.PendingAccountSwap(); pending {
		t.Fatal("durably captured committed replacement retained pending marker")
	}
}

func TestResumeLimitedSessions_ExplicitAccountPinIsNeverOverridden(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "stay pinned", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "personal")
	inst.Account = "work"
	inst.SetLimitReached(base.Add(time.Hour))

	advance(time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("explicitly pinned session resumed on another identity: %v", prompts)
	}
	account, automatic := inst.AccountSelection()
	if account != "work" || automatic {
		t.Fatalf("explicit pin changed to (%q, auto=%v)", account, automatic)
	}
}

func TestResumeFromLimit_CommittedSwapStillDeliversNoticeAfterOptOut(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	backend.sendPromptErr = errors.New("prompt transport failed")

	advance(time.Second)
	manager.ResumeLimitedSessions()
	account, automatic := inst.AccountSelection()
	if account != "work" || !automatic || !inst.LimitReached() {
		t.Fatalf("failed prompt left selection=(%q, %v), limited=%v; want committed work replacement still parked", account, automatic, inst.LimitReached())
	}

	// The move is already committed. Removing the opt-in stops future choices,
	// but cannot make this identity change silent by abandoning its notice.
	writeLimitAccountCandidates(t, "limit_account_candidates = []\n")
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, "accounts", "claude", "work")); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.sendPromptErr = nil
	backend.mu.Unlock()
	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatalf("manual retry of committed replacement: %v", err)
	}

	_, respawns, prompts := backend.snapshot()
	if respawns != 1 {
		t.Fatalf("committed replacement respawned %d times, want 1", respawns)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], `claude account "work"`) {
		t.Fatalf("committed identity change lost its in-session notice after opt-out: %q", prompts)
	}
}

func TestResumeFromLimit_LiveCommittedSwapDoesNotClearWithMissingSibling(t *testing.T) {
	manager, repoID, inst, _ := newAutoResumeManager(t, "", true, "finish", time.Time{})
	configureLimitAccountCandidate(t, manager, "work")

	worktreePath := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(
		inst.Path, worktreePath, inst.Title, "retry-branch", "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	const agentName = "af_pending_swap"
	executor := tabNameKeyedExec(map[string]bool{agentName: true})
	pty := tabPtyFactory{t: t, cmdExec: executor}
	agent := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, executor)
	inst.SetGitWorktreeForTest(gw)
	inst.SetTmuxSession(agent)
	if _, err := inst.AddProcessTab("make", "build"); err != nil {
		t.Fatal(err)
	}
	data := inst.ToInstanceData()
	if len(data.Tabs) != 2 {
		t.Fatalf("fixture tabs = %d, want agent plus sibling", len(data.Tabs))
	}
	siblingName := data.Tabs[1].TmuxName
	if state, err := tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		siblingName, "make", pty, executor).Close(); state != tmux.PaneStateKnown || err != nil {
		t.Fatalf("make sibling absent: state=%v err=%v", state, err)
	}
	if inst.TabAlive(1) {
		t.Fatal("fixture sibling must be absent while the replacement agent answers live")
	}

	requirePendingSwap := func() {
		t.Helper()
		if _, to, pending := inst.PendingAccountSwap(); !pending || to != "work" {
			t.Fatalf("pending account swap = (%q, %v), want work", to, pending)
		}
	}
	if err := inst.BeginLimitResume(); err != nil {
		t.Fatal(err)
	}
	if err := inst.ValidateAccountSwap("work"); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.SelectAccountAutomatically("", "work"); err != nil {
		t.Fatal(err)
	}
	inst.EndLimitResume()
	requirePendingSwap()

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err == nil {
		t.Fatal("live agent with a missing expected sibling completed the pending account swap")
	}
	requirePendingSwap()
}

func TestResumeLimitedSessions_CommittedSwapFinishesAfterAutoResumeOptOut(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	backend.sendPromptErr = errors.New("prompt transport failed")

	advance(time.Second)
	manager.ResumeLimitedSessions()
	if _, _, pending := inst.PendingAccountSwap(); !pending {
		t.Fatal("failed delivery lost the committed account swap")
	}
	manager.Config().LimitAutoResume = false
	backend.mu.Lock()
	backend.sendPromptErr = nil
	backend.mu.Unlock()
	advance(limitResumeBackoffBase)
	manager.ResumeLimitedSessions()

	_, _, prompts := backend.snapshot()
	if len(prompts) != 1 || !strings.Contains(prompts[0], `claude account "work"`) {
		t.Fatalf("committed swap did not finish after opt-out: %q", prompts)
	}
	if _, _, pending := inst.PendingAccountSwap(); pending {
		t.Fatal("successful delivery retained the pending swap")
	}
}

func TestRefreshStatuses_PendingAccountSwapKeepsLimitStateForDelivery(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &deadTmuxBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "pending-swap", backend, true, session.Running)
	inst.SetLimitReached(time.Now().Add(time.Hour))
	if err := inst.BeginLimitResume(); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.SelectAccountAutomatically("", "work"); err != nil {
		t.Fatal(err)
	}
	inst.EndLimitResume()

	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLimitReached {
		t.Fatalf("pending swap liveness after status poll = %v, want LimitReached", got)
	}
	if _, to, pending := inst.PendingAccountSwap(); !pending || to != "work" {
		t.Fatalf("status poll consumed pending swap = (%q, %v)", to, pending)
	}
}

func TestRefreshStatuses_PendingAccountSwapDoesNotSuppressNonLimitRows(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &deadTmuxBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "stale-pending-swap", backend, true, session.Running)
	inst.SetLimitReached(time.Now().Add(time.Hour))
	if err := inst.BeginLimitResume(); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.SelectAccountAutomatically("", "work"); err != nil {
		t.Fatal(err)
	}
	inst.EndLimitResume()
	inst.ClearLimitReached()

	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("non-limit row with stale pending marker after status poll = %v, want Lost", got)
	}
}

func TestPrepareRuntimeForAccountSwap_AbsentAgentStillStopsLiveSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(
		repoPath, worktreePath, "retry", "retry-branch", "", false, true)
	if err != nil {
		t.Fatal(err)
	}

	const agentName = "af_swap_retry"
	executor := tabNameKeyedExec(map[string]bool{agentName: true})
	pty := tabPtyFactory{t: t, cmdExec: executor}
	agent := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, executor)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "retry", Path: repoPath, Program: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	inst.SetBackend(&session.LocalBackend{})
	inst.SetGitWorktreeForTest(gw)
	inst.SetTmuxSession(agent)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	if _, err := inst.AddProcessTab("make", "build"); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.AddVSCodeTab("editor"); err != nil {
		t.Fatal(err)
	}
	if state, err := agent.Close(); state != tmux.PaneStateKnown || err != nil {
		t.Fatalf("make agent absent: state=%v err=%v", state, err)
	}
	if inst.TabAlive(0) || !inst.TabAlive(1) {
		t.Fatal("fixture must have an absent agent and a live sibling")
	}
	inst.SetLimitReached(time.Time{})
	if err := inst.BeginLimitResume(); err != nil {
		t.Fatal(err)
	}

	manager := &Manager{
		instances: map[string]*session.Instance{"key": inst},
		vscode:    newVSCodeSupervisor(),
	}
	registerVSCodeMarker(manager, "key")
	if err := manager.prepareRuntimeForAccountSwap("repo", "key", inst); err != nil {
		t.Fatal(err)
	}
	if inst.TabAlive(1) {
		t.Fatal("an absent agent pane must not let a still-live sibling bypass account-swap teardown")
	}
	if vscodeServerRegistered(manager, "key") {
		t.Fatal("an account swap left the daemon-owned VS Code process on the old identity")
	}
}

func TestAccountSwapOpportunity_UsesThePollsFrozenGlobalConfig(t *testing.T) {
	manager, _, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := agentaccount.Register(home, "claude", name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	frozen := manager.Config()
	frozen.LimitAccountCandidates = []string{"first"}
	writeLimitAccountCandidates(t, "limit_account_candidates = [\"second\"]\n")

	swap, err := manager.accountSwapOpportunityFromFacts(inst, frozen)
	if err != nil {
		t.Fatal(err)
	}
	if swap == nil || swap.to != "first" {
		t.Fatalf("swap from frozen config = %#v, want first", swap)
	}
}
