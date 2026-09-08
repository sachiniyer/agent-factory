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
func (m *Manager) deliverManualAccountMission(instance *session.Instance, mission string) error {
	// An outgoing wall is not evidence about the replacement identity. Its ledger
	// observation remains durable, while the replacement is independently probed.
	instance.ClearLimitReached()
	err := task.WaitForReadyAndSendPrompt(context.Background(), instance, mission)
	var limitErr *task.LimitReachedError
	if errors.As(err, &limitErr) {
		m.accountLimitMu.Lock()
		parkErr := instance.ParkManualAccountSwapAtLimit(limitErr.ResetAt)
		m.accountLimitMu.Unlock()
		return fmt.Errorf("incoming account reached a usage limit before delivery; mission remains pending: %w", errors.Join(err, parkErr))
	}
	return err
}
