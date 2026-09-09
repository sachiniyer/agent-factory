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

// BeginPendingManualAccountSwapMissionDelivery closes the durable replay gate
// before a delivery attempt touches the composer. A crash after this boundary
// reloads ambiguity, never the positive non-delivery verdict that admitted the
// attempt.
func (i *Instance) BeginPendingManualAccountSwapMissionDelivery(from, to string) (PromptDeliveryStatus, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || !pending.Manual || pending.From != from || pending.To != to {
		return "", fmt.Errorf("manual account swap from %q to %q is no longer pending", from, to)
	}
	if !pending.ReplacementPanesStarted {
		return "", fmt.Errorf("manual account swap from %q to %q has no replacement panes", from, to)
	}
	previous := pending.MissionDeliveryStatus
	if previous != PromptCouldNotConfirm {
		pending.MissionDeliveryStatus = PromptCouldNotConfirm
		i.touchLocked()
	}
	return previous, nil
}

// RestorePendingManualAccountSwapMissionDelivery rolls back a begin boundary
// only when persistence failed before submission began. Empty is allowed here:
// an initial delivery with no prior attempt has no verdict to restore.
func (i *Instance) RestorePendingManualAccountSwapMissionDelivery(from, to string, status PromptDeliveryStatus) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || !pending.Manual || pending.From != from || pending.To != to {
		return fmt.Errorf("manual account swap from %q to %q is no longer pending", from, to)
	}
	if status != "" && !status.Valid() {
		return fmt.Errorf("invalid manual account swap mission delivery status %q", status)
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
// prompt cannot authorize redelivery.
func (i *Instance) PendingManualAccountSwapDeliveryUnconfirmed() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingManualAccountSwapDeliveryUnconfirmedLocked()
}

func (i *Instance) pendingManualAccountSwapDeliveryUnconfirmedLocked() bool {
	if i.pendingAccountSwap == nil || !i.pendingAccountSwap.Manual ||
		!i.pendingAccountSwap.ReplacementPanesStarted || i.pendingAccountSwap.MissionDeliveryStatus == "" {
		return false
	}
	return i.pendingAccountSwap.MissionDeliveryStatus != PromptNotDelivered
}

// CanRetryPendingManualAccountSwapDelivery reports whether an operator can
// inspect a known replacement pane and explicitly override ambiguous delivery.
// Startup-unknown is inert because there is no confirmed runtime to inspect.
func (i *Instance) CanRetryPendingManualAccountSwapDelivery() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	knownLive := i.liveness == LiveRunning || i.liveness == LiveReady
	return knownLive && i.inFlightOp == OpNone && !i.startupStateUnknown && !i.userKilled &&
		i.pendingManualAccountSwapDeliveryUnconfirmedLocked()
}

// ReconcileAccountHandoffSnapshot mirrors the daemon-owned account identity and
// pending delivery transaction onto an existing client projection.
func (i *Instance) ReconcileAccountHandoffSnapshot(account string, auto bool, pending *AccountSwapData) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Account == account && i.accountAutoSelected == auto && accountSwapDataEqual(i.pendingAccountSwap, pending) {
		return false
	}
	i.Account = account
	i.accountAutoSelected = auto
	i.pendingAccountSwap = cloneAccountSwapData(pending)
	i.touchLocked()
	return true
}

func accountSwapDataEqual(a, b *AccountSwapData) bool {
	if a == nil || b == nil {
		return a == b
	}
	aOriginal, bOriginal := a.OriginalStartupStateUnknown, b.OriginalStartupStateUnknown
	aCopy, bCopy := *a, *b
	aCopy.OriginalStartupStateUnknown = nil
	bCopy.OriginalStartupStateUnknown = nil
	if aCopy != bCopy {
		return false
	}
	if aOriginal == nil || bOriginal == nil {
		return aOriginal == nil && bOriginal == nil
	}
	return *aOriginal == *bOriginal
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
