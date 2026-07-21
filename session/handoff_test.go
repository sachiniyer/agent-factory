package session

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
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
	inst.SetStartedForTest(true)
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

func TestSwapAgentProgram_RejectsArchivedSessionWithoutMutatingRecord(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.SetStatusForTest(Archived)
	inst.SetStartedForTest(false)

	_, err := inst.SwapAgentProgram(tmux.ProgramGemini, HandoffReasonManual, "sha", false)
	if err == nil {
		t.Fatal("SwapAgentProgram accepted an archived row")
	}
	if got := inst.AgentProgram(); got != tmux.ProgramClaude {
		t.Fatalf("Program = %q after refusal, want %q", got, tmux.ProgramClaude)
	}
	if ledger := inst.Handoffs(); len(ledger) != 0 {
		t.Fatalf("archived refusal wrote %d handoff records, want 0", len(ledger))
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

// A base-to-HEAD diff contains only committed work. When the outgoing agent has
// dirty files, the incoming agent must be pointed at both status (including
// untracked files) and a working-tree diff or the most recent work is invisible.
func TestMissionBrief_DirtyWorkIncludesWorkingTreeCommands(t *testing.T) {
	brief := MissionBrief{
		From: tmux.ProgramClaude,
		To:   tmux.ProgramCodex,
		Work: git.WorkSummary{
			Branch:     "agent/in-progress",
			BaseSHA:    "abc123",
			Commits:    2,
			DirtyFiles: 3,
		},
	}
	rendered := brief.Render()
	for _, command := range []string{"git diff abc123...HEAD", "git status --short", "git diff HEAD"} {
		if !strings.Contains(rendered, command) {
			t.Fatalf("dirty handoff brief omits %q; the incoming agent would see only committed work:\n%s", command, rendered)
		}
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

// TestCurrentAgentNameSurvivesUndetectableCommand pins the identity rule that
// the handoff surfaces depend on, using the state that actually broke: a live
// session whose command af cannot identify.
//
// program_overrides may point an agent enum at any command, and af identifies
// an agent by command-token BASENAME — so a wrapper script named anything other
// than the agent resolves to "" by design (configuration.md). Every earlier
// handoff test bound no tmux session at all, so ResolvedAgent fell back to the
// stored enum and the same-agent guard looked correct while being untested for
// the one configuration that defeats it.
//
// Found by driving the real TUI: the picker offered "claude" for a session
// already running claude, and the guard passed it — a self-handoff that stops a
// working agent and restarts it with no conversation.
func TestCurrentAgentNameSurvivesUndetectableCommand(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		"af_handoff_wrapper", "/home/dev/bin/my-claude-wrapper.sh", nil, nil))

	if got := inst.ResolvedAgent(); got != "" {
		t.Fatalf("precondition failed: ResolvedAgent()=%q, want \"\" — this test needs a command af cannot identify", got)
	}
	if got := inst.CurrentAgentName(); got != tmux.ProgramClaude {
		t.Fatalf("CurrentAgentName()=%q, want %q: a session configured as claude is claude even behind a wrapper script",
			got, tmux.ProgramClaude)
	}
	if err := inst.ValidateHandoffTarget(tmux.ProgramClaude); err == nil {
		t.Fatal("same-agent handoff accepted behind a wrapper script: the swap would stop a working agent and restart it with no conversation")
	}
}

// TestCurrentAgentNamePrefersTheRunningCommand is the other half of the rule.
// When af CAN identify the running command it wins over the stored enum: an
// override pointing "claude" at codex really is running codex, so handing that
// session "to codex" is the no-op the guard must catch.
func TestCurrentAgentNamePrefersTheRunningCommand(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		"af_handoff_mismatch", "/usr/local/bin/codex --sandbox", nil, nil))

	if got := inst.CurrentAgentName(); got != tmux.ProgramCodex {
		t.Fatalf("CurrentAgentName()=%q, want %q: the running command outranks the configured enum", got, tmux.ProgramCodex)
	}
	if err := inst.ValidateHandoffTarget(tmux.ProgramCodex); err == nil {
		t.Fatal("handoff to the agent already running accepted")
	}
	if err := inst.ValidateHandoffTarget(tmux.ProgramClaude); err != nil {
		t.Fatalf("handoff to the configured-but-not-running agent rejected: %v", err)
	}
}

// TestSwapAgentProgramRecordsResolvedOutgoingAgent locks the ledger to the same
// identity resolver as the guard. The ledger's outgoing agent is what a
// hand-back reads to restore the previous agent, so a "" there strands the
// session's own history.
func TestSwapAgentProgramRecordsResolvedOutgoingAgent(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Tabs[0].Conversation = AgentConversationData{}
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		"af_handoff_ledger", "/home/dev/bin/my-claude-wrapper.sh", nil, nil))

	entry, err := inst.SwapAgentProgram(tmux.ProgramCodex, HandoffReasonManual, "abc123", false)
	if err != nil {
		t.Fatalf("SwapAgentProgram: %v", err)
	}
	if entry.From.Agent != tmux.ProgramClaude {
		t.Fatalf("ledger recorded outgoing agent %q, want %q", entry.From.Agent, tmux.ProgramClaude)
	}
}

// TestMissionBrief_ReadsAsEnglishForEveryReason covers a copy defect found by
// driving a real handoff and reading what the incoming agent was actually sent:
// "It was being done by claude, which stopped because it hit manual."
//
// The HandoffReason* constants are ledger LABELS, and the brief interpolated one
// straight into a sentence. "usage limit" happened to read acceptably there,
// which is why it survived review — the manual reason, the one the only shipped
// trigger produces, did not.
func TestMissionBrief_ReadsAsEnglishForEveryReason(t *testing.T) {
	for _, tc := range []struct {
		reason   string
		wantSub  string
		bannedIn string
	}{
		{HandoffReasonManual, "handed over to you", "hit manual"},
		{HandoffReasonUsageLimit, "hit its usage limit", "hit usage limit."},
		{"", "stopped before the work was finished", "hit ."},
	} {
		inst := handoffTestInstance(t, tmux.ProgramClaude)
		rendered := inst.BuildMissionBrief(tmux.ProgramGemini, "", tc.reason).Render()

		if !strings.Contains(rendered, tc.wantSub) {
			t.Fatalf("reason %q: brief lacks %q\n%s", tc.reason, tc.wantSub, rendered)
		}
		if strings.Contains(rendered, tc.bannedIn) {
			t.Fatalf("reason %q: brief still contains the malformed clause %q\n%s", tc.reason, tc.bannedIn, rendered)
		}
	}
}
