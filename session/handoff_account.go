package session

import (
	"fmt"
	"time"
)

// BeginManualAccountSwap raises the existing account-replacement fence after
// validating the handoff lifecycle, including healthy and limit-blocked rows.
func (i *Instance) BeginManualAccountSwap() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	view := i.lifecycleViewLocked()
	if pending := i.pendingAccountSwap; pending != nil && pending.Manual && pending.To == i.Account {
		// Recovery owns this exact committed transaction, including healthy checkpoints.
		view.PendingAccountSwap = false
	}
	if err := view.ValidateRuntimeAction(RuntimeActionHandoff); err != nil {
		return err
	}
	return i.transitionLocked(BeginRespawn())
}

// SelectAccountForHandoff commits the same identity transaction as automatic
// rotation, retaining an explicit pin and its durable delivery obligation.
func (i *Instance) SelectAccountForHandoff(from, name, agent, reason, head, mission string) (HandoffSwap, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning {
		return HandoffSwap{}, fmt.Errorf("account handoff requires the replacement fence")
	}
	entry, err := i.recordHandoffSwapLocked(agent, reason, head, false)
	if err != nil {
		return HandoffSwap{}, err
	}
	entry.FromAccount, entry.ToAccount = from, name
	i.Tabs[0].Handoffs[len(i.Tabs[0].Handoffs)-1] = entry.AgentHandoff
	if _, err := i.selectAccountLocked(from, name, false); err != nil {
		return HandoffSwap{}, err
	}
	i.pendingAccountSwap.Manual = true
	i.pendingAccountSwap.Mission = mission
	return entry, nil
}

func (i *Instance) PendingManualAccountSwap() (bool, string) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.pendingAccountSwap == nil {
		return false, ""
	}
	return i.pendingAccountSwap.Manual, i.pendingAccountSwap.Mission
}

// RecordPendingManualAccountSwapMissionDelivery records a prompt verdict on the
// exact committed transaction whose mission was attempted. The pair check keeps
// a stale delivery return from changing a newer handoff's retry policy.
func (i *Instance) RecordPendingManualAccountSwapMissionDelivery(from, to string, status PromptDeliveryStatus) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || !pending.Manual || pending.From != from || pending.To != to {
		return fmt.Errorf("manual account swap from %q to %q is no longer pending", from, to)
	}
	if !pending.ReplacementPanesStarted {
		return fmt.Errorf("manual account swap from %q to %q has no replacement panes", from, to)
	}
	if !status.Valid() {
		status = PromptCouldNotConfirm
	}
	if pending.MissionDeliveryStatus != status {
		pending.MissionDeliveryStatus = status
		i.touchLocked()
	}
	return nil
}

// PendingManualAccountSwapDeliveryUnconfirmed reports whether the replacement
// runtime may already have received its pending mission. The verdict lives on
// the transaction rather than the session-wide latest prompt, so an unrelated
// prompt cannot authorize redelivery. An operator may inspect the pane and use
// the explicit retry even when automatic recovery stays suppressed.
func (i *Instance) PendingManualAccountSwapDeliveryUnconfirmed() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.pendingAccountSwap == nil || !i.pendingAccountSwap.Manual ||
		!i.pendingAccountSwap.ReplacementPanesStarted || i.pendingAccountSwap.MissionDeliveryStatus == "" {
		return false
	}
	return i.pendingAccountSwap.MissionDeliveryStatus != PromptNotDelivered
}

// ParkManualAccountSwapAtLimit attributes a readiness wall to the replacement
// identity without releasing the account transaction's fence or its mission.
func (i *Instance) ParkManualAccountSwapAtLimit(resetAt time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning || i.pendingAccountSwap == nil || !i.pendingAccountSwap.Manual {
		return fmt.Errorf("manual account limit requires the pending replacement fence")
	}
	lv, op, prevReset := i.lifecycleStateLocked()
	i.liveness = LiveLimitReached
	i.limitResetAt = resetAt
	if agent := i.currentAgentNameLocked(); i.limitAgent != agent {
		i.limitAgent = agent
		i.touchLocked()
	}
	if i.limitAccount != i.Account {
		i.limitAccount = i.Account
		i.touchLocked()
	}
	// Readiness found the incoming identity's wall before mission submission,
	// which is positive non-delivery evidence for this transaction. Replace any
	// earlier ambiguity so the scheduler may resume it after the recorded reset.
	if i.pendingAccountSwap.MissionDeliveryStatus != PromptNotDelivered {
		i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered
		i.touchLocked()
	}
	i.recordAccountLimitObservationLocked(i.currentAgentNameLocked(), i.Account, resetAt)
	i.noteStateChangeLocked(lv, op, prevReset)
	return nil
}
