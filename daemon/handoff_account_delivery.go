package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// Manual account handoffs share ordinary handoff's readiness/trust contract.
// The caller retains PendingAccountSwap until delivery and durable settlement;
// retries re-enter this same path, including after a daemon restart.
// The booleans report whether this function owned the readiness settlement and
// whether that write landed, so the caller neither repeats nor masks it.
//
// operatorInitiated reports whether the caller is the explicit ResumeFromLimit RPC
// (exempt from the attach-lease recheck) or the automated scheduler (subject to it).
func (m *Manager) deliverManualAccountMission(repoID, key string, instance *session.Instance, swap *autoAccountSwap, mission string, operatorInitiated bool) (bool, bool, error) {
	// Positive non-delivery evidence admits automatic recovery. Retire it on disk
	// before touching the composer: a crash during submission then reloads an
	// ambiguous verdict and cannot authorize the same mission a second time. An
	// explicit operator retry follows the same boundary after pane inspection.
	previousStatus, err := instance.BeginPendingManualAccountSwapMissionDelivery(swap.from, swap.to)
	if err != nil {
		return false, false, err
	}
	if err := m.persistSettlement(repoID, key, instance); err != nil {
		// The attempt never began, so positive non-delivery remains truthful. The
		// outer failure path checkpoints this restoration and leaves no send behind.
		_ = instance.RestorePendingManualAccountSwapMissionDelivery(
			swap.from, swap.to, previousStatus,
		)
		return false, false, fmt.Errorf("could not record the account-handoff mission attempt before submission; mission was not submitted: %w", err)
	}
	// An outgoing wall is not evidence about the replacement identity. Its ledger
	// observation remains durable, while the replacement is independently probed.
	// Capture the liveness and reset time before ClearLimitReached so a lease
	// deferral inside the readiness wait can restore the row a TUI attached to
	// (#5422 review). accountSwapScheduledResumeEligible admits a committed manual
	// swap whose row is LiveRunning/LiveReady rather than limit-blocked, so
	// ClearLimitReached is a no-op there and LimitResetAt returns no reset; an
	// unconditional re-park would mislabel that healthy row as quota-blocked.
	originalLiveness := instance.GetLiveness()
	resetAt, _ := instance.LimitResetAt()
	instance.ClearLimitReached()
	status, err := task.WaitForReadyAndSubmitPrompt(context.Background(), instance, mission, func() error {
		// Recheck the attach lease immediately before the composer is touched (#5422
		// review): the send-boundary recheck in resumeFromLimitLockedOutcome ran before
		// this function, but the durable settlement write and the readiness wait inside
		// WaitForReadyAndSubmitPrompt can both outlast it. A TUI can acquire or renew
		// the lease during either, and WaitForReadyAndSendPromptWithStatus would submit
		// the mission without another attach check. The operatorInitiated RPC is exempt,
		// exactly like the send-boundary recheck. A deferral restores the pending mission
		// delivery state and re-parks the limit so the scheduler re-fires on the next tick.
		testHookResumeBeforeManualMissionSend()
		if !operatorInitiated && m.isPollPaused(repoID, instance.Title, instance.ID) {
			return errAttachPauseDeferred
		}
		return nil
	})
	// A lease deferral before the composer is not a send error: the mission was not
	// submitted, so restore the attempt marker the begin boundary staged and re-park
	// the limit ClearLimitReached cleared above. The caller translates the sentinel to
	// a clean resumeDeferredByAttachPause so the scheduler re-fires on the next tick.
	// Restore the liveness a TUI attached to: a committed manual swap whose row was
	// LiveRunning/LiveReady (not limit-blocked) must not be re-parked to
	// LiveLimitReached with a zero reset, which would mislabel a healthy handoff as
	// quota-blocked (#5422 review). Persist the restored verdict and re-parked/healthy
	// row so a daemon restart does not reload the PromptCouldNotConfirm marker the
	// durable settlement write staged before the deferral: without it,
	// accountSwapScheduledResumeEligible classifies the row as a possibly delivered
	// mission and automatic recovery stays disabled (#5422 review).
	if errors.Is(err, errAttachPauseDeferred) {
		_ = instance.RestorePendingManualAccountSwapMissionDelivery(swap.from, swap.to, previousStatus)
		if originalLiveness == session.LiveRunning || originalLiveness == session.LiveReady {
			_ = instance.Transition(session.ObserveLiveness(originalLiveness))
		} else {
			_ = instance.ReparkLimitUnderResumeFence(resetAt)
		}
		if persistErr := m.persistSettlement(repoID, key, instance); persistErr != nil {
			m.warn().Printf("limit resume for %q: could not checkpoint the restored manual mission delivery deferral: %v", instance.Title, persistErr)
		}
		return false, false, errAttachPauseDeferred
	}
	err = handoffDeliveryResultError(status, err)
	var limitErr *task.LimitReachedError
	if errors.As(err, &limitErr) {
		m.accountLimitMu.Lock()
		parkErr := instance.ParkManualAccountSwapAtLimit(limitErr.ResetAt)
		m.accountLimitMu.Unlock()
		return false, false, fmt.Errorf("incoming account reached a usage limit before delivery; mission remains pending: %w", errors.Join(err, parkErr))
	}
	if errors.Is(err, task.ErrAgentReadiness) {
		// A replacement that vanished before readiness is not safe to retry: a
		// detached child may still own the worktree. Keep the durable mission, but
		// make the row inert until an operator inspects the pane and worktree.
		instance.MarkStartupStateUnknown()
		settleErr := m.persistSettlement(repoID, key, instance)
		return true, settleErr == nil, errors.Join(err, settleErr)
	}
	if errors.Is(err, task.ErrPromptDelivery) {
		// Bind the runtime's exact verdict to this transaction before the
		// settlement write; later ordinary prompts update only session-wide
		// evidence and cannot change this mission's retry policy.
		missionErr := instance.RecordPendingManualAccountSwapMissionDelivery(
			swap.from, swap.to, status,
		)
		return false, false, errors.Join(err, missionErr)
	}
	return false, false, err
}
