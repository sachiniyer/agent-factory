package daemon

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestPreflightAccountSwapCandidates_SkipsUnusableCandidate(t *testing.T) {
	swap := &autoAccountSwap{to: "broken", candidates: []string{"broken", "work"}}
	var tried []string
	err := preflightAccountSwapCandidates(swap, func(candidate string) error {
		tried = append(tried, candidate)
		if candidate == "broken" {
			return errors.New("missing credential boundary")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tried, []string{"broken", "work"}) || swap.to != "work" {
		t.Fatalf("preflight tried %q and selected %q, want [broken work] then work", tried, swap.to)
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
