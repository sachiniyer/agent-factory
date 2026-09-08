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
func (m *Manager) deliverManualAccountMission(repoID, key string, instance *session.Instance, mission string) (bool, bool, error) {
	// An outgoing wall is not evidence about the replacement identity. Its ledger
	// observation remains durable, while the replacement is independently probed.
	instance.ClearLimitReached()
	err := task.WaitForReadyAndSendPrompt(context.Background(), instance, mission)
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
	return false, false, err
}
