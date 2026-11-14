package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// AccountSwapCarryRefusal is require_carry's check-ahead answer: "" when a
// same-agent account swap of this session would carry its conversation rather
// than start fresh, else the carry refusal code (handoff.Carry*) naming why
// not. It answers by running the swap's own carry plan (planAccountSwapCarry)
// without a target, so every check the live swap makes on the outgoing side
// runs here too: a recorded conversation of the provider's own, a source home
// af can resolve, a transcript there it can locate (a single plain rollout,
// not compressed, not continued by a thread/revert rollout), a recorded
// conversation still the newest in its worktree, and a launch command that can
// resume a named conversation (#7152).
//
// What only a target can answer — its home, a rollout of the thread it
// already holds — is judged when the move plans against that target, and a
// conversation can still change between this answer and that plan. The Codex
// checks read a source store snapshot shared for carryAheadStoreTTL
// (carryAheadStores), so the answer can lag the store by that much.
//
// It reads the provider's store, so it must never run under a manager lock;
// AccountSwapCarryRecorded is the in-memory precondition for that.
func (i *Instance) AccountSwapCarryRefusal() string {
	return carryRefusalCode(i.accountSwapCarryRefusalReason())
}

// accountSwapCarryRefusalReason is the prose AccountSwapCarryRefusal codes:
// why the carry would not happen, or "".
func (i *Instance) accountSwapCarryRefusalReason() string {
	// The same program a same-agent swap plans with: the runtime af recorded
	// at launch, else the pane's frozen command (validateAccountSwapPlan).
	i.mu.RLock()
	program := strings.TrimSpace(i.runtimeProgram)
	ts := i.tmuxLocked()
	account := i.Account
	outgoing := i.reportedConversation
	if len(i.Tabs) > 0 {
		outgoing = i.Tabs[0].Conversation
	}
	i.mu.RUnlock()
	if program == "" && ts != nil {
		program = strings.TrimSpace(ts.Program())
	}
	if program == "" {
		return carryNoLaunchCommandReason
	}
	agent := tmux.DetectAgentFromCommand(program)
	if !carrySupportedAgent(agent) {
		return fmt.Sprintf(carryUnsupportedFormat, agent)
	}
	carry, reason := planAccountSwapCarry(accountSwapCarryRequest{
		agent:      agent,
		outgoing:   outgoing,
		current:    account,
		program:    program,
		workDir:    i.GetWorktreePath(),
		sourceOnly: true,
		stores:     carryAheadStores,
		// The live swap's own judgement of a newer conversation (#7227).
		newerIsEvidence: i.newerConversationCountsAgainstCarry,
	})
	return carryAheadRefusal(carry, reason, program)
}

// carryAheadRefusal is the check-ahead's verdict on a plan: "" only for a
// carry that will happen. A plan that carries nothing is a refusal whatever
// its reason says — the planner's (nil, "") means no carry applies, and
// require_carry must never read that as confirmed (#7156 review P3-3).
func carryAheadRefusal(carry *conversationCarry, reason, program string) string {
	if carry == nil {
		if reason == "" {
			return carryNothingAppliesReason
		}
		return reason
	}
	if _, _, ok := carry.launch(program); !ok {
		return carryUnresumableReason
	}
	return ""
}
