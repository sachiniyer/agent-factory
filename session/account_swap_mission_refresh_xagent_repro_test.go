package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Cross-agent account swap's re-launch re-rendered its replacement's mission as
// a same-agent carry failure. refreshPendingManualAccountSwapMission rebuilt
// the brief with BuildMissionBrief(CurrentAgentName(), ...) — and
// BuildMissionBrief sets From = CurrentAgentName too — so on the refresh path
// the brief always had From == To with CrossAgent unset, even for a cross-agent
// swap whose committed conversation gets re-minted (e.g. after a /clear in an
// af-owned worktree). Render's same-agent branch then narrated "af tried to
// carry the previous conversation over to the new account", fabricating a
// transcript carry across the agent boundary (agent-handoff D2: a cross-agent
// handoff never transfers a transcript).
//
// The fix threads the pending record's own durable facts into the refresh — the
// handoff ledger's From.Agent (outgoing agent), the pending record's
// AccountAgent/Program (incoming namespace), and its pinned CrossAgent — the
// same three the daemon threaded onto the commit-time brief at #4778. The
// refresh stops re-deriving identity from CurrentAgentName, which at refresh
// time may already answer the incoming runtime the checkpoint installed (so
// From == To) or be empty after teardown (so From == "" hits the "another
// agent" branch).

// committedXAgentManualPendingSwap builds an isolated in-memory instance whose
// pendingAccountSwap mimics a committed manual cross-agent swap from outgoing
// to incoming, with its trailing handoff ledger entry naming outgoing as the
// OUTGOING agent. mission is the pre-refresh stored mission; id is the fresh
// conversation id the commit recorded; oldRecord reports whether the durable
// CrossAgent field is set, simulating a record written before the fix.
func committedXAgentManualPendingSwap(t *testing.T, outgoing, incoming, id, mission string, oldRecord bool) *Instance {
	t.Helper()
	inst := accountSwapTestInstanceWithProgram(outgoing)
	inst.mu.Lock()
	inst.pendingAccountSwap = &AccountSwapData{
		Manual:         true,
		From:           "old",
		To:             "work",
		Mission:        mission,
		AccountAgent:   incoming,
		Program:        incoming,
		ConversationID: id,
		CarryFallback:  staleCommittedConversationReason,
	}
	if !oldRecord {
		inst.pendingAccountSwap.CrossAgent = true
	}
	if len(inst.Tabs) > 0 {
		inst.Tabs[0].Handoffs = append(inst.Tabs[0].Handoffs, AgentHandoff{
			From:        AgentConversationData{Agent: outgoing},
			To:          incoming,
			FromAccount: "old",
			ToAccount:   "work",
			Reason:      HandoffReasonManual,
		})
	}
	inst.mu.Unlock()
	return inst
}

// assertXAgentMission asserts the mission Render returns the cross-agent
// handoff attribution and no carry language. A cross-agent handoff never
// transfers a transcript (agent-handoff D2), so the notice must never narrate
// a carry the swap never attempted.
func assertCrossAgentMission(t *testing.T, mission, outgoing string) {
	t.Helper()
	require.Contains(t, mission, "It was being done by "+outgoing,
		"the cross-agent mission names the outgoing agent")
	require.Contains(t, mission, "was handed over to you before the work was finished",
		"the cross-agent mission says the handoff reason")
	require.Contains(t, mission, "Its conversation is not available to you",
		"the cross-agent mission says the outgoing transcript is not reachable")
	require.NotContains(t, mission, "af tried to carry",
		"a cross-agent handoff never attempts a transcript carry, so the notice must not claim one")
	require.NotContains(t, mission, "This is a fresh conversation after an account handoff",
		"the same-agent fresh-conversation framing must not appear on a cross-agent swap")
}

// A committed cross-agent manual swap (codex->claude) whose re-launch re-mints
// its committed Claude conversation id re-renders its stored mission through
// Render's cross-agent branch, not the same-agent carry-failure branch.
// Before the fix the refresh rebuilt the brief with From == To == codex, so
// the delivered mission claimed "af tried to carry the previous conversation
// over to the new account" — a carry a cross-agent swap structurally never
// attempts.
func TestRefreshPendingManualAccountSwapMission_CrossAgentRendersCrossAgentBranch(t *testing.T) {
	inst := committedXAgentManualPendingSwap(t, tmux.ProgramCodex, tmux.ProgramClaude,
		"f1d2c3b4-a5e6-4f7a-8b9c-0d1e2f3a4b5c", "old mission", false)
	_, before := inst.PendingManualAccountSwap()

	inst.refreshPendingManualAccountSwapMission("old", "work")

	_, after := inst.PendingManualAccountSwap()
	require.NotEqual(t, before, after, "the refresh rewrote the stored mission")
	assertCrossAgentMission(t, after, tmux.ProgramCodex)
	require.Equal(t, staleCommittedConversationReason, inst.pendingAccountSwap.CarryFallback,
		"the refresh does not change the record's own conversation outcome")
}

// Regression guard: a same-agent manual swap's refresh must keep Render on the
// same-agent carry-failure branch. The fix changes the refresh's identity
// derivation but its result for a same-agent record is From == To == the
// recorded agent with CrossAgent unset, so the sameAgent gate still routes the
// fresh-conversation-after-an-account-handoff framing as before (the original
// same-agent mission refresh test pins the same text through the respawn path
// in account_swap_carry_test.go::TestRespawnForAccountSwapDemotesACarryThatCanNoLongerLand).
func TestRefreshPendingManualAccountSwapMission_SameAgentPreservesCarryFailureBranch(t *testing.T) {
	inst := committedXAgentManualPendingSwap(t, tmux.ProgramClaude, tmux.ProgramClaude,
		"f1d2c3b4-a5e6-4f7a-8b9c-0d1e2f3a4b5c", "old mission", true)
	// The ledger records the same-agent outgoing agent (claude), so the
	// refresh's recovered From == To == "claude" with CrossAgent unset: the
	// same three facts the buggy refresh already produced for a same-agent
	// swap, now from the durable record.
	inst.mu.Lock()
	if len(inst.Tabs) > 0 && len(inst.Tabs[0].Handoffs) > 0 {
		inst.Tabs[0].Handoffs[len(inst.Tabs[0].Handoffs)-1].To = tmux.ProgramClaude
	}
	inst.mu.Unlock()

	inst.refreshPendingManualAccountSwapMission("old", "work")

	_, after := inst.PendingManualAccountSwap()
	require.Contains(t, after, "This is a fresh conversation after an account handoff.",
		"the same-agent carry-failure framing must still render")
	require.Contains(t, after, "af tried to carry the previous conversation over to the new account, but",
		"the same-agent refresh still says a carry was attempted")
	require.NotContains(t, after, "It was being done by",
		"the cross-agent attribution sentence is not used for a same-agent swap")
}

// Backward compatibility: a pending record written before this fix (no durable
// CrossAgent field) admits no frozen verdict. For a cross-agent swap the
// recovered outgoing agent differs from the incoming effective agent — the
// HandoffTargetIsCurrent refusal that admitted it — so the OR-derivation
// reconstructs CrossAgent=true from the same identities ManualAccountSwapProgram
// compared at commit. This keeps the bug open for a record the prior fix never
// reached.
func TestRefreshPendingManualAccountSwapMission_LegacyCrossAgentRecordDerivesCrossAgent(t *testing.T) {
	inst := committedXAgentManualPendingSwap(t, tmux.ProgramCodex, tmux.ProgramClaude,
		"f1d2c3b4-a5e6-4f7a-8b9c-0d1e2f3a4b5c", "old mission", true)
	require.False(t, inst.pendingAccountSwap.CrossAgent,
		"precondition: a legacy record carries no durable CrossAgent field")

	inst.refreshPendingManualAccountSwapMission("old", "work")

	_, after := inst.PendingManualAccountSwap()
	assertCrossAgentMission(t, after, tmux.ProgramCodex)
}

// Secondary consequence: a same-agent swap whose live identity tiers are gone
// at refresh time (a teardown) previously rendered the "It was being done by
// another agent" branch, because CurrentAgentName() answered "" and Render's
// same-agent gate requires From != "". With the fix, From is recovered from
// the handoff ledger (which survives teardown) so it equals To (the recorded
// namespace), and the same-agent branch is rendered as it would be when the
// live identity was still present.
func TestRefreshPendingManualAccountSwapMission_LiveIdentityEmptyRendersFromLedger(t *testing.T) {
	inst := committedXAgentManualPendingSwap(t, tmux.ProgramClaude, tmux.ProgramClaude,
		"f1d2c3b4-a5e6-4f7a-8b9c-0d1e2f3a4b5c", "old mission", true)
	// Simulate the teardown the daemon sees at re-plan time: no live tmux
	// binding, no runtime record, no configured enum matters — the resolver
	// answers "" for every tier, exactly as it does for an off-box session
	// whose runtime is gone.
	inst.mu.Lock()
	inst.Tabs[0].tmux = nil
	inst.runtimeProgram = ""
	inst.Program = ""
	if len(inst.Tabs) > 0 && len(inst.Tabs[0].Handoffs) > 0 {
		inst.Tabs[0].Handoffs[len(inst.Tabs[0].Handoffs)-1].To = tmux.ProgramClaude
	}
	inst.mu.Unlock()
	require.Empty(t, inst.CurrentAgentName(),
		"precondition: the live identity resolver answers nothing")
	require.Empty(t, inst.AgentProgram(),
		"precondition: the configured enum is gone too")

	inst.refreshPendingManualAccountSwapMission("old", "work")

	_, after := inst.PendingManualAccountSwap()
	require.NotContains(t, after, "It was being done by another agent",
		"the recovered From closes the same-agent gate whose From == \"\" check the teardown had broken")
	require.Contains(t, after, "This is a fresh conversation after an account handoff.",
		"the same-agent swap renders the same-agent fresh framing, not the cross-agent attribution")
}

// End-to-end reproduction: a committed cross-agent manual swap codex->claude
// whose committed Claude conversation is judged abandoned in an af-owned
// worktree (the committed transcript is filed, and a newer one filed later in
// the same directory — the same verdict machinery
// committedConversationLaunch uses) re-mints its committed conversation id via
// the pending record, and the refresh renders the cross-agent mission the
// daemon would render at commit time, never the same-agent carry-failure
// framing the buggy refresh produced.
func TestRefreshPendingManualAccountSwapMission_CrossAgentRemintIntegration(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	// The cross-agent replacement program is claude; preflight the swap with a
	// stub binary the manual path requires (the daemon's preflight does the
	// same with a real binary).
	bin := t.TempDir()
	for _, name := range []string{tmux.ProgramClaude, tmux.ProgramCodex} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{
		tmux.ProgramCodex:  tmux.ProgramCodex,
		tmux.ProgramClaude: tmux.ProgramClaude,
	}
	require.NoError(t, config.SaveConfig(cfg))
	target := registerAccount(t, tmux.ProgramClaude, "work")

	layout := exclusiveLayout(t)
	inst := accountSwapTestInstanceWithProgram(tmux.ProgramCodex)
	inst.Path = layout.repo
	gw, err := sessiongit.NewGitWorktreeFromStorage(layout.repo, layout.worktree, inst.Title, "main", "", layout.external, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	// Commit a fresh cross-agent manual swap codex->claude into "work". The
	// commit's brief is the cross-agent handoff attribution, the same shape
	// the daemon threaded at #4778.
	require.NoError(t, inst.ValidateManualAccountSwap("work", tmux.ProgramClaude, true))
	require.Nil(t, inst.accountSwapLaunch.carry,
		"a cross-agent swap never carries a transcript across the agent boundary")
	require.Empty(t, inst.accountSwapLaunch.carryFallback,
		"a cross-agent brief keeps its own wording, never a carry-fallback clause")
	commitBrief := MissionBrief{
		From:       tmux.ProgramCodex,
		To:         tmux.ProgramClaude,
		Goal:       "finish the migration",
		CrossAgent: true,
		Reason:     HandoffReasonManual,
	}
	committedMission := commitBrief.Render()
	require.Contains(t, committedMission, "It was being done by "+tmux.ProgramCodex,
		"precondition: the commit-time mission is the cross-agent brief")
	_, err = inst.SelectAccountForHandoff("", "work", tmux.ProgramClaude, tmux.ProgramClaude,
		true, HandoffReasonManual, "", committedMission)
	require.NoError(t, err)
	pending := inst.ToInstanceData().PendingAccountSwap
	require.NotNil(t, pending)
	require.True(t, pending.Manual)
	require.NotEmpty(t, pending.ConversationID, "precondition: a fresh cross-agent swap commits its replacement's conversation id")
	require.Empty(t, pending.CarriedConversationID, "precondition: a cross-agent swap carries nothing")
	require.Equal(t, tmux.ProgramClaude, pending.AccountAgent,
		"precondition: the committed incoming namespace is pinned on the durable record")
	require.True(t, pending.CrossAgent,
		"precondition: the committed cross-agent verdict is pinned on the durable record")
	require.Equal(t, tmux.ProgramCodex, func() string {
		h, ok := inst.LastHandoff()
		require.True(t, ok)
		return h.From.Agent
	}(), "precondition: the durability ledger recorded the OUTGOING agent at commit")
	committed := pending.ConversationID

	// The re-launch verdict the daemon's retry re-plan runs: Claude filed the
	// committed conversation, and a newer one beside it in an af-owned worktree
	// is evidence the session moved on (recordedClaudeAbandoned) — the same
	// machinery TestCommittedSwapRelaunchStartsOnAnIDRemintedAfterTheLaunch
	// uses to drive a remint there.
	worktree := inst.GetWorktreePath()
	require.True(t, inst.newerConversationIsEvidence(worktree),
		"precondition: af owns this worktree exclusively, so a newer conversation in it is evidence the session moved on")
	fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Hour))
	fileClaudeTranscript(t, target, worktree, newerConversationID, time.Now().Add(-time.Minute))

	// The restart's view: only the durable record survives the in-memory
	// plan. The retry re-plans from that record, the re-mint branch mints a
	// fresh id in place of the abandoned committed one, and the refresh
	// re-renders the stored mission.
	inst.accountSwapLaunch = nil
	require.NoError(t, inst.ValidateAccountSwap("work"), "the committed retry re-plans its replacement")
	refreshedPending := inst.ToInstanceData().PendingAccountSwap
	require.NotEqual(t, committed, refreshedPending.ConversationID,
		"precondition: the committed id was re-minted as the abandoned verdict requires")
	require.Equal(t, staleCommittedConversationReason, refreshedPending.CarryFallback,
		"precondition: the record explains why the reminted replacement starts fresh")
	_, refreshedMission := inst.PendingManualAccountSwap()
	assertCrossAgentMission(t, refreshedMission, tmux.ProgramCodex)
}

// Canary: with the durable CrossAgent field absent (legacy record) and the
// recovered From equal to the recovered To for what is in fact a same-agent
// swap, the OR-derivation keeps SameAgent rather than fabricating a
// cross-agent verdict. The HandoffTargetIsCurrent frame agrees — for every
// record the field could have been written for, the recovered From vs
// AccountAgent comparison cannot be inverted.
func TestRefreshPendingManualAccountSwapMission_LegacySameAgentRecordStaysSameAgent(t *testing.T) {
	inst := committedXAgentManualPendingSwap(t, tmux.ProgramClaude, tmux.ProgramClaude,
		"f1d2c3b4-a5e6-4f7a-8b9c-0d1e2f3a4b5c", "old mission", true)
	inst.mu.Lock()
	if len(inst.Tabs) > 0 && len(inst.Tabs[0].Handoffs) > 0 {
		inst.Tabs[0].Handoffs[len(inst.Tabs[0].Handoffs)-1].To = tmux.ProgramClaude
	}
	inst.mu.Unlock()
	require.False(t, inst.pendingAccountSwap.CrossAgent,
		"precondition: a legacy record carries no durable CrossAgent field")

	inst.refreshPendingManualAccountSwapMission("old", "work")

	_, after := inst.PendingManualAccountSwap()
	require.Contains(t, after, "af tried to carry the previous conversation over to the new account, but",
		"the OR-derivation does not invent a cross-agent verdict for a same-agent legacy record")
	require.NotContains(t, after, "It was being done by",
		"a same-agent legacy record never collapses onto the cross-agent branch")
}
