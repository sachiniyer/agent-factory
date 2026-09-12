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
	from, _ := instance.AccountSelection()
	if from == strings.TrimSpace(req.Account) && target == instance.CurrentAgentName() {
		return HandoffSessionResponse{}, fmt.Errorf("session %q already uses %s account %q", instance.Title, target, from)
	}
	reason := session.HandoffReasonManual
	if instance.LimitReached() {
		reason = session.HandoffReasonUsageLimit
	}
	outgoing := instance.CurrentAgentName()
	swap := &autoAccountSwap{
		manual: true, promptOverride: req.Brief, from: from, to: strings.TrimSpace(req.Account),
		fromAgent: outgoing, agent: target, reason: reason,
	}
	outcome, err := m.resumeFromLimitLockedOutcome(repoID, key, instance, instance.Title, swap)
	response := HandoffSessionResponse{OK: true, From: outgoing, To: target, FromAccount: from, ToAccount: swap.to, HeadSHA: swap.headSHA}
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
	if _, err := agentaccount.Selected(home, swap.agent, swap.to, ""); err != nil {
		return nil, err
	}
	limited, err := m.limitedAccountsForSwap(swap.agent, loadAccountLimitEvidenceForSwap)
	if err != nil {
		return nil, err
	}
	for _, name := range limited {
		if name == swap.to {
			return nil, fmt.Errorf("%s account %q is currently at its usage limit; choose another account or wait for its limit to reset", swap.agent, swap.to)
		}
	}
	if instanceHasVSCodeTab(instance) {
		return nil, fmt.Errorf("cannot switch accounts for %q while it has a VS Code tab", instance.Title)
	}
	validate := instance.CheckManualAccountSwap
	if recordLaunch {
		validate = instance.ValidateManualAccountSwap
	}
	if err := validate(swap.to, swap.agent); err != nil {
		return nil, err
	}
	admitted := *swap
	admitted.previousAccount, admitted.previousAuto = instance.AccountSelection()
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
