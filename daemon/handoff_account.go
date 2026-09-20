package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Tests pause a request before it acquires the target lock to exercise agent drift.
var testHookHandoffAccountBeforeTargetLock = func() {}

func (m *Manager) handoffAccount(req HandoffSessionRequest, instance *session.Instance, repoID string) (HandoffSessionResponse, error) {
	key := daemonInstanceKey(repoID, instance.Title)
	testHookHandoffAccountBeforeTargetLock()
	unlock := m.lockTarget(repoID, instance.Title)
	defer unlock()
	op := m.opLockFor(key)
	if !op.TryLock() {
		return HandoffSessionResponse{}, fmt.Errorf("session %q is busy with another operation", instance.Title)
	}
	defer op.Unlock()
	// Re-read the slot while both lifecycle locks are held. A kill or archive
	// may have claimed the key while this request waited for the target lock,
	// and the caller's pointer may have been replaced or removed in the meantime.
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing {
		return HandoffSessionResponse{}, fmt.Errorf("session %q is being killed/archived", instance.Title)
	}
	if current != instance {
		return HandoffSessionResponse{}, fmt.Errorf("session %q was replaced or removed", instance.Title)
	}
	instance = current
	// A local account handoff can rebuild a vanished persisted worktree through
	// the shared respawn path. Acquire repository admission before the handoff can
	// commit an identity or stop a pane; a timeout therefore remains an untouched,
	// retryable refusal rather than a half-applied replacement.
	worktreeAdmission, err := m.lockLocalWorktreeAdmissionWithin(repoID, instance.Title, "hand off", instance)
	if err != nil {
		return HandoffSessionResponse{}, err
	}
	if worktreeAdmission != nil {
		defer worktreeAdmission.Unlock()
	}
	// An account-only request follows the agent selected by the preceding
	// operation, including a handoff that finished while we waited for the lock.
	target := strings.TrimSpace(req.To)
	if target == "" {
		target = instance.CurrentAgentName()
	}
	if !tmux.IsSupportedProgram(target) {
		return HandoffSessionResponse{}, fmt.Errorf("unknown agent %q", target)
	}
	if !instance.Capabilities().Handoff {
		return HandoffSessionResponse{}, session.ErrHandoffUnsupported
	}
	outgoing := instance.CurrentAgentName()
	from, _ := instance.AccountSelection()
	var swap *autoAccountSwap
	// The committed-transaction test compares the request's target to BOTH
	// spellings of the committed target: the agent the pane now runs (outgoing)
	// covers the --account-only retry whose target defaulted to it, and
	// i.Program covers a redirected swap whose recorded enum differs — a retry
	// of `--to aider --account work` still says aider while the pane runs codex
	// (#4430 review round 3). The raw-enum spelling is a retry ONLY while its
	// transaction is still pending: once an override edit resolves that enum
	// to another agent, the same request is a new cross-agent handoff, and the
	// enum match must not swallow it into a committed-replay or a no-op
	// refusal (#4430 review round 4).
	if from == strings.TrimSpace(req.Account) && (target == outgoing || target == instance.AgentProgram()) {
		// The request names the identity a committed swap already recorded —
		// the retry the pending-swap refusal advertises (#4393), not a no-op.
		// Finish the recorded transaction, whose stored mission and durable
		// (from, to) pair a fresh admission would overwrite.
		if swap = committedAccountSwap(instance); swap != nil {
			if strings.TrimSpace(req.Brief) != "" {
				// The committed transaction already owns the mission it delivers; a
				// replacement brief cannot amend it, and silently dropping one the
				// operator typed is worse than refusing.
				return HandoffSessionResponse{}, fmt.Errorf("session %q has a committed account swap to %s whose recorded mission is what the retry delivers; retry without --brief", instance.Title, accountSwapIdentity(target, swap.to))
			}
		} else if strings.TrimSpace(req.To) == "" ||
			session.HandoffTargetIsCurrent(outgoing, target, session.HandoffEffectiveAgentForPath(instance.Path, target)) {
			// No committed transaction: a no-op when the request named no
			// target (an account-only re-request of the account already in
			// use), or when the request's RESOLVED target is the running
			// identity. An enum that still matches i.Program but resolves
			// elsewhere after an override edit is a real cross-agent request
			// and falls through to admission.
			return HandoffSessionResponse{}, fmt.Errorf("session %q already uses %s account %q", instance.Title, target, from)
		}
	}
	if swap == nil {
		reason := session.HandoffReasonManual
		if instance.LimitReached() {
			reason = session.HandoffReasonUsageLimit
		}
		// The transaction carries the enum and its resolved namespace
		// separately: agent stays the requested target because the program
		// side still resolves ITS override — `program_overrides.aider =
		// "/custom/codex --flag"` launches the custom command, which
		// program_overrides.codex does not name (#4430 review round 2) — while
		// accountAgent is the agent the command actually launches, the only
		// namespace its registry can answer Selected in. The committed and
		// scheduler paths derive the same namespace from the command
		// (accountSwapAgent, AgentForCommand). It is resolved inside
		// evaluateManualAccountSwap rather than here: the authoritative pass
		// runs under the project-config lock, so the namespace it consults is
		// recomputed from the same configuration the locked admission freezes
		// (#4430 review round 3).
		swap = &autoAccountSwap{
			manual: true, promptOverride: req.Brief, from: from, to: strings.TrimSpace(req.Account),
			fromAgent: outgoing, agent: target, reason: reason,
			accountOnly: strings.TrimSpace(req.To) == "",
		}
	}
	outcome, err := m.resumeFromLimitLockedOutcome(repoID, key, instance, instance.Title, swap)
	response := HandoffSessionResponse{OK: true, From: swap.fromAgent, To: target, FromAccount: swap.from, ToAccount: swap.to, HeadSHA: swap.headSHA}
	if err != nil {
		if outcome == resumePerformed || isMutationCommitted(err) {
			return response, err
		}
		return HandoffSessionResponse{}, err
	}
	if outcome == resumeNotPerformed {
		return HandoffSessionResponse{}, fmt.Errorf("session %q is no longer available to hand off", instance.Title)
	}
	return response, nil
}

// Manual admission uses the same registered and limit evidence sources as the
// scheduler, under its account-limit fence, without requiring rotation policy.
func (m *Manager) admitManualAccountSwap(instance *session.Instance, swap *autoAccountSwap) (*autoAccountSwap, error) {
	return m.evaluateManualAccountSwap(instance, swap, true)
}

// checkManualAccountSwap preserves domain refusals that do not depend on the
// project-config lock, without recording a launch plan or authorizing mutation.
// A successful result is only advisory: admission repeats the whole proof under
// the personal-policy lock immediately before the identity checkpoint.
func (m *Manager) checkManualAccountSwap(instance *session.Instance, swap *autoAccountSwap) error {
	_, err := m.evaluateManualAccountSwap(instance, swap, false)
	return err
}

func (m *Manager) evaluateManualAccountSwap(instance *session.Instance, swap *autoAccountSwap, recordLaunch bool) (*autoAccountSwap, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return nil, err
	}
	// Re-resolve the account namespace on every pass: this function runs once
	// unlocked (an advisory precheck) and once under the project-config lock
	// (locked admission), and program_overrides may change between them — a
	// namespace resolved at request time would have Selected, limit-ledger
	// checks, and messages consulting a registry the frozen launch command no
	// longer belongs to (#4430 review round 3). The credential-boundary parser
	// answers "" for a command it cannot prove, which no registry can serve —
	// refuse it directly rather than fall back to the requested enum and
	// report the wrong agent.
	//
	// The program selection is decided HERE, once, and handed to both the
	// launch preflight and the identity commit — the swap's target for a
	// cross-agent handoff, the recorded program for a same-agent or
	// account-only one — so the namespace consulted here is the one the frozen
	// launch plan proves, even when overrides point at each other (aider→codex
	// beside codex→aider). ManualAccountSwapProgram documents the rule.
	program, crossAgent := instance.ManualAccountSwapProgram(swap.agent, swap.accountOnly)
	swap.crossAgent = crossAgent
	swap.accountAgent = session.HandoffEffectiveAgentForPath(instance.Path, program)
	if swap.accountAgent == "" {
		return nil, fmt.Errorf("cannot hand %q off to %s with account %q: the program it resolves to cannot carry an account scope",
			instance.Title, swap.agent, swap.to)
	}
	if _, err := agentaccount.Selected(home, swap.accountNamespace(), swap.to); err != nil {
		return nil, err
	}
	limited, err := m.limitedAccountsForSwap(swap.accountNamespace(), loadAccountLimitEvidenceForSwap)
	if err != nil {
		return nil, err
	}
	for _, name := range limited {
		if name == swap.to {
			return nil, fmt.Errorf("%s account %q is currently at its usage limit; choose another account or wait for its limit to reset", swap.accountNamespace(), swap.to)
		}
	}
	if instanceHasVSCodeTab(instance) {
		return nil, fmt.Errorf("cannot switch accounts for %q while it has a VS Code tab", instance.Title)
	}
	validate := instance.CheckManualAccountSwap
	if recordLaunch {
		validate = instance.ValidateManualAccountSwap
	}
	if err := validate(swap.to, swap.agent, crossAgent); err != nil {
		return nil, err
	}
	admitted := *swap
	admitted.previousAccount, admitted.previousAuto = instance.AccountSelection()
	admitted.previousAccountAgent = instance.AccountAgent()
	return &admitted, nil
}

// A manual swap can checkpoint a healthy row before its new process starts.
// Its pending transaction, rather than invented quota evidence, makes that row
// recoverable even with automatic rotation disabled.
func accountSwapResumeEligible(instance *session.Instance) bool {
	if instance.StartupStateUnknown() {
		return false
	}
	if instance.LimitReached() {
		return true
	}
	manual, _ := instance.PendingManualAccountSwap()
	live := instance.GetLiveness()
	return manual && (live == session.LiveRunning || live == session.LiveReady)
}

// accountSwapScheduledResumeEligible applies the delivery contract to automatic
// recovery. An explicit retry remains the operator's override after inspecting
// the pane, but the scheduler may retry only when delivery was not attempted or
// was positively observed absent; could-not-confirm may already have submitted.
func accountSwapScheduledResumeEligible(instance *session.Instance) bool {
	return accountSwapResumeEligible(instance) &&
		!instance.PendingManualAccountSwapDeliveryUnconfirmed()
}
