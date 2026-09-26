package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// TestAccountSwapCarryIntendedJudgesOnlyTheCheapPreconditions covers the
// confirm-time carry-eligibility signal the TUI branches its consent copy on
// (#4367/#4504). It is a NECESSARY, not sufficient, signal, so every case the
// daemon would still refuse on filesystem grounds (a stale transcript, a
// missing account home, a copy failure) is OUT OF SCOPE here — those are
// covered by planAccountSwapCarry's own tests. This test pins the preconditions
// the confirming user CAN know: same resolved identity, a carry-supporting
// provider, and a recorded conversation for that provider.
func TestAccountSwapCarryIntendedJudgesOnlyTheCheapPreconditions(t *testing.T) {
	const carryID = "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d"
	claudeOutgoing := AgentConversationData{Agent: tmux.ProgramClaude, ID: carryID}
	codexOutgoing := AgentConversationData{Agent: tmux.ProgramCodex, ID: carryID}

	// The carry-succeeds narrowing the consent copy is wrong for: a same-agent
	// claude swap with a recorded conversation the daemon will attempt to carry.
	require.True(t, AccountSwapCarryIntended(tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, claudeOutgoing),
		"a same-agent claude swap with a recorded conversation is carry-intended")
	require.True(t, AccountSwapCarryIntended(tmux.ProgramCodex, tmux.ProgramCodex, tmux.ProgramCodex, tmux.ProgramCodex, codexOutgoing),
		"a same-agent codex swap with a recorded conversation is carry-intended")

	// A program_overrides redirect makes the target ENUM differ from the
	// resolved identity: aider redirected to codex beside a codex pane is a
	// same-agent swap by RESOLVED identity, so it is carry-intended despite
	// target == "aider" != current == "codex" — the same reason the picker does
	// not classify by enum (#4430 review).
	require.True(t, AccountSwapCarryIntended(tmux.ProgramCodex, tmux.ProgramAider, tmux.ProgramCodex, tmux.ProgramCodex, codexOutgoing),
		"a redirected same-resolved-identity swap is carry-intended")

	// Cross-agent handoffs never carry: providers cannot read each other's
	// transcripts, and admission classifies these as cross-agent.
	require.False(t, AccountSwapCarryIntended(tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramCodex, tmux.ProgramClaude, claudeOutgoing),
		"a cross-agent swap is never carry-intended")

	// Gemini records no conversation id af can address, so carry does not
	// apply even for a same-agent swap with a recorded-looking conversation.
	require.False(t, AccountSwapCarryIntended(tmux.ProgramGemini, tmux.ProgramGemini, tmux.ProgramGemini, tmux.ProgramGemini,
		AgentConversationData{Agent: tmux.ProgramGemini, ID: carryID}),
		"gemini is not a carry-supporting agent")

	// No recorded conversation -> the daemon has nothing to carry, so the
	// replacement starts fresh. This is the case the consent copy's fresh-start
	// clause is the truth for.
	require.False(t, AccountSwapCarryIntended(tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, AgentConversationData{}),
		"no recorded conversation means no carry is attempted")
	require.False(t, AccountSwapCarryIntended(tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude,
		AgentConversationData{Agent: tmux.ProgramClaude}),
		"a recorded agent with no id is not a conversation the daemon can carry")

	// An opaque target (targetResolved == "") is never carry-intended: the
	// daemon cannot prove it launches a carry-supporting provider, so the
	// replacement starts fresh — even when the enum and recorded program make
	// it look same-agent. The opaque branch of HandoffTargetIsCurrent admits
	// the self-handoff on the recorded enum, so this guard is what keeps a
	// wrapper-backed self-handoff off the carry-intended branch.
	require.False(t, AccountSwapCarryIntended(tmux.ProgramClaude, tmux.ProgramClaude, "", tmux.ProgramClaude, claudeOutgoing),
		"an opaque target resolution is never carry-intended")

	// A recorded conversation for a different provider than the target resolves
	// to is not carryable onto the replacement: the daemon refuses
	// (outgoing.Agent != req.agent), so the consent copy must not promise it.
	require.False(t, AccountSwapCarryIntended(tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, codexOutgoing),
		"a conversation recorded for a different provider is not carry-intended")
}

// TestAccountSwapCarryIntendedFallsBackToTheEnumForAnUnresolvedTarget locks the
// older-daemon / unscoped-first-frame behaviour: a nil-style resolvedAgents map
// (the target absent) falls back to the enum, so a same-agent claude swap stays
// carry-intended exactly as the picker's resolvedFor fallback classifies it.
func TestAccountSwapCarryIntendedFallsBackToTheEnumForAnUnresolvedTarget(t *testing.T) {
	const carryID = "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d"
	outgoing := AgentConversationData{Agent: tmux.ProgramClaude, ID: carryID}
	require.True(t, AccountSwapCarryIntended(tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, outgoing),
		"targetResolved == target keeps a same-agent claude swap carry-intended")
}
