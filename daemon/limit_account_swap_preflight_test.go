package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestResumeLimitedSessions_UnusableCandidatesFallBackWhenResetDue(t *testing.T) {
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(
		t, "", true, "continue the original task", base.Add(-limitResumeGrace-time.Second))
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"broken", "work"} {
		if _, err := agentaccount.Register(home, tmux.ProgramClaude, name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	writeLimitAccountCandidates(t, "limit_account_candidates = [\"broken\", \"work\"]\n"+
		"[program_overrides]\nclaude = \"claude --continue\"\n")
	manager.Config().LimitAccountCandidates = []string{"broken", "work"}

	manager.ResumeLimitedSessions()

	_, respawns, prompts := backend.snapshot()
	if respawns != 0 || len(prompts) != 1 || prompts[0] != "continue the original task" {
		t.Fatalf("due fallback = respawns %d prompts %q, want live ordinary resume with original task", respawns, prompts)
	}
	if account, automatic := inst.AccountSelection(); account != "" || automatic {
		t.Fatalf("unusable candidates changed identity to (%q, %v), want ambient", account, automatic)
	}
}

func TestResumeLimitedSessions_CandidateBackoffCannotDelayOrdinaryDeadline(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	ordinaryDue := base.Add(5 * time.Second)
	manager, repoID, inst, backend := newAutoResumeManager(
		t, "", true, "continue on the original account", ordinaryDue.Add(-limitResumeGrace))
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentaccount.Register(home, tmux.ProgramClaude, "broken"); err != nil {
		t.Fatal(err)
	}
	writeLimitAccountCandidates(t, "limit_account_candidates = [\"broken\"]\n"+
		"[program_overrides]\nclaude = \"claude --continue\"\n")
	manager.Config().LimitAccountCandidates = []string{"broken"}

	// The immediate swap attempt fails preflight and arms the 10-second base
	// backoff, while the independent ordinary reset deadline is only 5 seconds
	// away.
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("failed candidate preflight resumed early: %q", prompts)
	}
	manager.mu.Lock()
	nextAttempt := manager.limitResumeStates[stableSessionKey(repoID, inst)].nextAttempt
	manager.mu.Unlock()
	if !nextAttempt.After(ordinaryDue) {
		t.Fatalf("fixture candidate backoff %v does not cross ordinary deadline %v", nextAttempt, ordinaryDue)
	}

	advance(6 * time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 || prompts[0] != "continue on the original account" {
		t.Fatalf("candidate retry backoff delayed the ordinary deadline: prompts=%q", prompts)
	}
	if account, automatic := inst.AccountSelection(); account != "" || automatic {
		t.Fatalf("ordinary fallback changed identity to (%q, %v), want ambient", account, automatic)
	}
}
