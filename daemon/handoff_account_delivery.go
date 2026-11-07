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
	// Positive non-delivery evidence admits automatic recovery. Retire it on disk
	// before touching the composer: a crash during submission then reloads an
	// ambiguous verdict and cannot authorize the same mission a second time. An
	// explicit operator retry follows the same boundary after pane inspection.
	// The displaced attempt instant retires with the verdict (#6798): a stale
	// timestamp must never re-arm the re-verification watch against an attempt
	// that is no longer pending.
	previousStatus, previousAttempt, err := instance.BeginPendingManualAccountSwapMissionDelivery(swap.from, swap.to)
	if err != nil {
		return false, false, err
	}
	if err := m.persistSettlement(repoID, key, instance); err != nil {
		// The attempt never began, so positive non-delivery remains truthful. The
		// outer failure path checkpoints this restoration and leaves no send behind.
		// Status and attempt instant restore as one pair so the record reads
		// exactly as evidence armed it before the boundary (#6798).
		_ = instance.RestorePendingManualAccountSwapMissionDelivery(
			swap.from, swap.to, previousStatus, previousAttempt,
		)
		return false, false, fmt.Errorf("could not record the account-handoff mission attempt before submission; mission was not submitted: %w", err)
	}
	// An outgoing wall is not evidence about the replacement identity. Its ledger
	// observation remains durable, while the replacement is independently probed.
	instance.ClearLimitReached()
	// The wait runs on the established replacement pane — the swap committed
	// before this attempt — so a residue verdict is judged normally, matching
	// the false the incoming-wall park below passes (#6972).
	status, err := task.WaitForReadyAndSendPromptWithStatus(context.Background(), instance, mission, false)
	err = handoffDeliveryResultError(status, err)
	var limitErr *task.LimitReachedError
	if errors.As(err, &limitErr) {
		m.accountLimitMu.Lock()
		parkErr := instance.ParkManualAccountSwapAtLimit(limitErr.ResetAt, limitErr.Evidence,
			session.LimitBanner{Prefix: limitErr.BannerPrefix, Count: limitErr.BannerCount}, false)
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
