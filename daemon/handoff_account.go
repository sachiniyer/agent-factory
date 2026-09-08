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
	swap := &autoAccountSwap{manual: true, promptOverride: req.Brief, from: from, to: strings.TrimSpace(req.Account), agent: target, reason: reason}
	outgoing := instance.CurrentAgentName()
	outcome, err := m.resumeFromLimitLockedOutcome(repoID, key, instance, instance.Title, swap)
	if err != nil {
		return HandoffSessionResponse{}, err
	}
	if outcome == resumeNotPerformed {
		return HandoffSessionResponse{}, fmt.Errorf("session %q is no longer available to hand off", instance.Title)
	}
	return HandoffSessionResponse{OK: true, From: outgoing, To: target, FromAccount: from, ToAccount: swap.to, HeadSHA: swap.headSHA}, nil
}

// Manual admission uses the same registered and limit evidence sources as the
// scheduler, under its account-limit fence, without requiring rotation policy.
func (m *Manager) admitManualAccountSwap(instance *session.Instance, swap *autoAccountSwap) (*autoAccountSwap, error) {
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
	if err := instance.ValidateManualAccountSwap(swap.to, swap.agent); err != nil {
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
	if instance.LimitReached() {
		return true
	}
	manual, _ := instance.PendingManualAccountSwap()
	live := instance.GetLiveness()
	return manual && (live == session.LiveRunning || live == session.LiveReady)
}
