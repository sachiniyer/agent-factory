package session

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// handoffTestInstance builds a started instance running `program`, with an agent
// tab carrying a recorded conversation.
//
// Deliberately NOT claude→codex. claude is SupportedPrograms[0] and the repo's
// default agent, and codex is [1] — an implementation that hardcoded either
// index, or fell back to the config default, would pass a claude→codex test
// while being completely wrong. gemini is [3], so the assertions below only hold
// if the target actually flows through (#1997's lesson: a test that names the
// right value is decorative when that value is also the default).
func handoffTestInstance(t *testing.T, program string) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{Title: "handoff-subject", Path: t.TempDir(), Program: program})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(NewFakeBackend())
	inst.Tabs = []*Tab{{
		ID:   "tab-1",
		Name: "agent",
		Kind: TabKindAgent,
		Conversation: AgentConversationData{
			Agent:       program,
			ID:          "conv-outgoing-42",
			CaptureKind: ConversationCaptureInjected,
		},
	}}
	return inst
}

func TestSwapAgentProgram_RewritesProgramAndRecordsLedger(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)

	entry, err := inst.SwapAgentProgram(tmux.ProgramGemini, HandoffReasonUsageLimit, "abc123def456", false)
	if err != nil {
		t.Fatalf("SwapAgentProgram: %v", err)
	}

	if got := inst.AgentProgram(); got != tmux.ProgramGemini {
		t.Fatalf("Program = %q, want %q — the swap must rewrite the program the next spawn resolves from", got, tmux.ProgramGemini)
	}
	if entry.To != tmux.ProgramGemini {
		t.Fatalf("entry.To = %q, want %q", entry.To, tmux.ProgramGemini)
	}
	if entry.From.Agent != tmux.ProgramClaude {
		t.Fatalf("entry.From.Agent = %q, want %q", entry.From.Agent, tmux.ProgramClaude)
	}
	// The outgoing conversation id is the whole reason the ledger holds a
	// conversation rather than just an agent name: it is what makes a hand-back
	// land on the ORIGINAL thread instead of the provider's latest-in-cwd guess.
	if entry.From.ID != "conv-outgoing-42" {
		t.Fatalf("entry.From.ID = %q, want the outgoing conversation id to be preserved in the ledger", entry.From.ID)
	}
	if entry.HeadSHA != "abc123def456" {
		t.Fatalf("entry.HeadSHA = %q, want the branch tip recorded as the attribution boundary", entry.HeadSHA)
	}
	if entry.Reason != HandoffReasonUsageLimit {
		t.Fatalf("entry.Reason = %q, want %q", entry.Reason, HandoffReasonUsageLimit)
	}
	if entry.Automatic {
		t.Fatal("entry.Automatic = true; the prompted path is the only one built (design D1)")
	}

	// The live conversation slot must be cleared: it describes the agent running
	// NOW, and the incoming agent has no conversation yet. Leaving the outgoing
	// id there would hand a gemini pane a claude conversation id.
	if conv := inst.AgentConversation(); !conv.Empty() {
		t.Fatalf("Tab.Conversation = %+v, want cleared so the incoming agent starts its own", conv)
	}

	ledger := inst.Handoffs()
	if len(ledger) != 1 {
		t.Fatalf("ledger has %d entries, want 1", len(ledger))
	}
	if ledger[0] != entry {
		t.Fatalf("ledger[0] = %+v, want the returned entry %+v", ledger[0], entry)
	}
}

func TestSwapAgentProgram_AppendsRatherThanReplaces(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)

	if _, err := inst.SwapAgentProgram(tmux.ProgramGemini, HandoffReasonUsageLimit, "sha-1", false); err != nil {
		t.Fatalf("first swap: %v", err)
	}
	if _, err := inst.SwapAgentProgram(tmux.ProgramAider, HandoffReasonManual, "sha-2", false); err != nil {
		t.Fatalf("second swap: %v", err)
	}

	ledger := inst.Handoffs()
	if len(ledger) != 2 {
		t.Fatalf("ledger has %d entries, want 2 — the ledger is append-only history, not a single slot", len(ledger))
	}
	if ledger[0].To != tmux.ProgramGemini || ledger[1].To != tmux.ProgramAider {
		t.Fatalf("ledger order = [%s, %s], want [gemini, aider] oldest-first", ledger[0].To, ledger[1].To)
	}
	// The second entry's outgoing agent must be the FIRST swap's target, not the
	// original agent: each entry records the swap it performed.
	if ledger[1].From.Agent != tmux.ProgramGemini {
		t.Fatalf("ledger[1].From.Agent = %q, want %q", ledger[1].From.Agent, tmux.ProgramGemini)
	}
}

func TestValidateHandoffTarget(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)

	if err := inst.ValidateHandoffTarget(""); err == nil {
		t.Fatal("empty target accepted; --to is required")
	}
	if err := inst.ValidateHandoffTarget("not-an-agent"); err == nil {
		t.Fatal("unknown agent accepted; the target must be a supported agent")
	}
	if err := inst.ValidateHandoffTarget(tmux.ProgramClaude); err == nil {
		t.Fatal("same-agent handoff accepted; swapping claude for claude is a no-op that would still stop and restart the agent")
	}
	if err := inst.ValidateHandoffTarget(tmux.ProgramGemini); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
}

func TestRevertHandoff_RestoresProgramAndConversation(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)

	entry, err := inst.SwapAgentProgram(tmux.ProgramGemini, HandoffReasonUsageLimit, "sha", false)
	if err != nil {
		t.Fatalf("SwapAgentProgram: %v", err)
	}
	if err := inst.RevertHandoff(entry); err != nil {
		t.Fatalf("RevertHandoff: %v", err)
	}

	if got := inst.AgentProgram(); got != tmux.ProgramClaude {
		t.Fatalf("Program = %q after revert, want %q — a record that disagrees with the running pane mis-resolves every later respawn", got, tmux.ProgramClaude)
	}
	if conv := inst.AgentConversation(); conv.ID != "conv-outgoing-42" {
		t.Fatalf("Conversation = %+v after revert, want the outgoing conversation restored", conv)
	}
	if ledger := inst.Handoffs(); len(ledger) != 0 {
		t.Fatalf("ledger has %d entries after revert, want 0 — a swap that never happened must not be recorded", len(ledger))
	}
}

func TestRevertHandoff_RefusesWhenNotTheLastEntry(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)

	stale, err := inst.SwapAgentProgram(tmux.ProgramGemini, HandoffReasonUsageLimit, "sha-1", false)
	if err != nil {
		t.Fatalf("first swap: %v", err)
	}
	if _, err := inst.SwapAgentProgram(tmux.ProgramAider, HandoffReasonManual, "sha-2", false); err != nil {
		t.Fatalf("second swap: %v", err)
	}

	if err := inst.RevertHandoff(stale); err == nil {
		t.Fatal("reverting a non-trailing entry succeeded; that would truncate a later swap's record")
	}
	if ledger := inst.Handoffs(); len(ledger) != 2 {
		t.Fatalf("ledger has %d entries, want both retained after a refused revert", len(ledger))
	}
}

func TestMissionBrief_CarriesGoalAndPointsAtTheDiff(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Prompt = "fix the flaky retry test"

	brief := inst.BuildMissionBrief(tmux.ProgramGemini, "", HandoffReasonUsageLimit)
	rendered := brief.Render()

	if !strings.Contains(rendered, "fix the flaky retry test") {
		t.Fatalf("brief omits the stored goal; the mission is the entire state transfer.\n%s", rendered)
	}
	if !strings.Contains(rendered, tmux.ProgramClaude) {
		t.Fatalf("brief does not name the outgoing agent, so the new agent cannot tell whose work it inherits.\n%s", rendered)
	}
	if !strings.Contains(rendered, HandoffReasonUsageLimit) {
		t.Fatalf("brief does not say why the predecessor stopped; without it the work reads as abandoned or broken.\n%s", rendered)
	}
	// The brief must not merely re-send the stored prompt (which is what the
	// #1146 resume path does). If handoff ever degrades to that, the new agent
	// gets a bare task with no signal that it is INHERITING work — and starts over.
	if strings.TrimSpace(rendered) == inst.Prompt {
		t.Fatal("brief is just the stored prompt; a handoff must tell the incoming agent it is continuing someone else's work")
	}
	if !strings.Contains(rendered, "Do not start over") {
		t.Fatalf("brief omits the continue-don't-restart instruction.\n%s", rendered)
	}
}

func TestMissionBrief_OverrideWinsOverStoredPrompt(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Prompt = "the stale create-time prompt"

	brief := inst.BuildMissionBrief(tmux.ProgramGemini, "finish the retry test only", HandoffReasonManual)
	rendered := brief.Render()

	if !strings.Contains(rendered, "finish the retry test only") {
		t.Fatalf("brief ignores the operator-supplied override.\n%s", rendered)
	}
	if strings.Contains(rendered, "the stale create-time prompt") {
		t.Fatalf("brief carries BOTH the override and the stored prompt; two goals in one brief is the blended-context hazard #2013 names.\n%s", rendered)
	}
}

// A session with no stored prompt must not have a goal invented for it. The
// brief says it has none — an agent handed a fabricated goal would pursue the
// fabrication.
func TestMissionBrief_NoGoalIsStatedNotInvented(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Prompt = ""

	rendered := inst.BuildMissionBrief(tmux.ProgramGemini, "", HandoffReasonUsageLimit).Render()

	if !strings.Contains(rendered, "No goal was recorded") {
		t.Fatalf("brief does not admit it has no goal.\n%s", rendered)
	}
	if strings.Contains(rendered, "The original goal:") {
		t.Fatalf("brief renders an empty goal section.\n%s", rendered)
	}
}
