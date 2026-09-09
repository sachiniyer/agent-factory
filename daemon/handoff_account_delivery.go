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
func (m *Manager) deliverManualAccountMission(repoID, key string, instance *session.Instance, swap *autoAccountSwap, mission string) (bool, bool, error) {
	// An outgoing wall is not evidence about the replacement identity. Its ledger
	// observation remains durable, while the replacement is independently probed.
	instance.ClearLimitReached()
	status, err := task.WaitForReadyAndSendPromptWithStatus(context.Background(), instance, mission)
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
