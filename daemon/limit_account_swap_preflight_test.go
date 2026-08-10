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
