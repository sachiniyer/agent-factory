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
	"github.com/sachiniyer/agent-factory/session"
)

func configureLimitAccountCandidate(t *testing.T, name string) {
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

func TestResumeLimitedSessions_SwapsAmbientSessionToConfiguredUnblockedAccount(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(time.Hour))
	configureLimitAccountCandidate(t, "work")

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
	configureLimitAccountCandidate(t, "work")

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

func TestResumeLimitedSessions_ExplicitAccountPinIsNeverOverridden(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "stay pinned", base.Add(time.Hour))
	configureLimitAccountCandidate(t, "personal")
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
	configureLimitAccountCandidate(t, "work")
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
