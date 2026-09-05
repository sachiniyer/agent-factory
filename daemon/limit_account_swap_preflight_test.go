package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestPreflightAccountSwapCandidatesUsesTheFirstProvenIdentity(t *testing.T) {
	swap := &autoAccountSwap{to: "broken", candidates: []string{"broken", "work"}}
	var attempted []string
	admitted, err := preflightAccountSwapCandidates(swap, func(candidate string) error {
		attempted = append(attempted, candidate)
		if candidate == "broken" {
			return errors.New("credential directory is unusable")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("later explicitly authorized candidate was not tried: %v (attempted %q)", err, attempted)
	}
	if admitted.to != "work" {
		t.Fatalf("admitted account = %q, want first proven candidate work (attempted %q)", admitted.to, attempted)
	}
}

func TestAdmitAccountSwapRefusesVSCodeIntegratedShells(t *testing.T) {
	manager, _, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	inst.Tabs = append(inst.Tabs, &session.Tab{ID: "editor", Name: "editor", Kind: session.TabKindVSCode})
	if err := inst.BeginLimitResume(); err != nil {
		t.Fatal(err)
	}
	defer inst.EndLimitResume()
	swap, err := manager.admitAccountSwap(inst, manager.Config())
	if err == nil {
		t.Fatalf("VS Code integrated shell was admitted without identity proof: %+v", swap)
	}
	if !strings.Contains(err.Error(), "VS Code") || !strings.Contains(err.Error(), "cannot prove") {
		t.Fatalf("VS Code refusal did not name the unprovable boundary: %v", err)
	}
	if swap != nil {
		t.Fatalf("refused VS Code boundary returned a swap: %+v", swap)
	}
}

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

func TestResumeLimitedSessions_FixedRetryFallbackKeepsConfiguredCadence(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, inst, backend := newAutoResumeManager(
		t, "30m", true, "continue on the original account", time.Time{})
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

	// Candidate preflight fails immediately. When the independent fixed fallback
	// becomes due, that same refusal yields to an ordinary resume.
	manager.ResumeLimitedSessions()
	advance(30 * time.Minute)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 || prompts[0] != "continue on the original account" {
		t.Fatalf("fixed fallback did not resume the original identity: prompts=%q", prompts)
	}

	manager.mu.Lock()
	nextAttempt := manager.limitResumeStates[stableSessionKey(repoID, inst)].nextAttempt
	manager.mu.Unlock()
	want := base.Add(60 * time.Minute)
	if !nextAttempt.Equal(want) {
		t.Fatalf("next retry after fixed fallback = %v, want configured cadence at %v", nextAttempt, want)
	}
}
