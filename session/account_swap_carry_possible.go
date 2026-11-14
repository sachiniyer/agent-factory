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
// already holds — is AccountSwapCarryRefusalTo's, which the engine asks once
// it has chosen the target. A conversation can still change between either
// answer and the move's own plan. The Codex checks read a store snapshot
// shared for carryAheadStoreTTL (carryAheadStores), so the answer can lag the
// store by that much.
//
// It reads the provider's store, so it must never run under a manager lock;
// AccountSwapCarryRecorded is the in-memory precondition for that.
func (i *Instance) AccountSwapCarryRefusal() string {
	return carryRefusalCode(i.accountSwapCarryRefusalReason())
}

// AccountSwapCarryRefusalTo is require_carry's answer against one target
// account: "" when a same-agent swap of this session into target would carry
// its conversation, else the carry refusal code naming why not. It runs the
// swap's carry plan against target, so on top of every source check
// AccountSwapCarryRefusal makes, it makes every check the live plan makes on
// the target's home: a copy of the conversation filed there under another
// spelling of the worktree's path or another rollout, a thread/revert rollout
// of it. The handoff engine asks it for the account Decide chose, after the
// target-blind answer confirmed the source side, so a refusal only the
// target's home can show defers the move instead of starting it fresh — the
// fallback require_carry exists to refuse (#7273).
//
// The target's Codex store is read from a snapshot shared the same way as the
// source's, with the same carryAheadStoreTTL lag; the move's own plan walks
// both stores live.
func (i *Instance) AccountSwapCarryRefusalTo(target string) string {
	return carryRefusalCode(i.carryAheadReason(target, false))
}

// accountSwapCarryRefusalReason is the prose AccountSwapCarryRefusal codes:
// why the carry would not happen, or "".
func (i *Instance) accountSwapCarryRefusalReason() string {
	return i.carryAheadReason("", true)
}

// carryAheadReason is the prose of require_carry's answer: against target, or
// with sourceOnly, against no target at all.
func (i *Instance) carryAheadReason(target string, sourceOnly bool) string {
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
		target:     target,
		sourceOnly: sourceOnly,
		stores:     carryAheadStores,
		// The live swap's own judgement of a newer conversation (#7227).
		newerIsEvidence: i.newerConversationIsEvidence,
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
