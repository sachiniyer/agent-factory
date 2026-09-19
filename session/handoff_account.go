package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// ValidateHandoffRuntimeAction evaluates RuntimeActionHandoff for a request
// that names its target. A committed account swap fences every other lifecycle
// action, but a request carrying that swap's own account and agent is the retry
// the refusal's message advertises — the pending-swap axis alone cannot refuse
// it (#4393). Every other axis still applies, and a request naming a different
// account or agent remains a new transaction the pending swap still owns.
func (i *Instance) ValidateHandoffRuntimeAction(agent, account string) error {
	i.mu.RLock()
	defer i.mu.RUnlock()
	view := i.lifecycleViewLocked()
	if view.PendingAccountSwap && i.pendingAccountSwapRetryTargetLocked(agent, account) {
		view.PendingAccountSwap = false
	}
	err := view.ValidateRuntimeAction(RuntimeActionHandoff)
	if err == nil || !view.PendingAccountSwap {
		return err
	}
	// The pending-swap axis refused because the request named a different agent
	// or account than the committed swap recorded. When an explicit --to <agent>
	// does not name the agent this swap will run, put that agent in the message:
	// in the pre-relaunch window this PR targets the bound pane still reports the
	// outgoing agent, so the visible agent is exactly the wrong answer, and the
	// bare "retry that account swap" remedy is the very request the refusal just
	// denied. Naming the agent is what makes the retry self-explanatory.
	pending := i.pendingAccountSwap
	if pending == nil || strings.TrimSpace(account) != pending.To {
		return err
	}
	committed := i.committedAgentNameLocked()
	if got := strings.TrimSpace(agent); committed != "" && got != "" && got != committed {
		return fmt.Errorf("session %q has a committed account swap awaiting its replacement notice and task; retry that account swap with --to %s (the agent this swap will run), not %q", i.Title, committed, got)
	}
	return err
}

// pendingAccountSwapRetryTargetLocked reports whether agent and account name
// the committed swap's own target: the account the identity checkpoint already
// moved this session to and — when the request names an agent — the agent the
// record already runs. Callers hold i.mu.
func (i *Instance) pendingAccountSwapRetryTargetLocked(agent, account string) bool {
	pending := i.pendingAccountSwap
	if pending == nil || pending.To != i.Account || strings.TrimSpace(account) != pending.To {
		return false
	}
	if agent = strings.TrimSpace(agent); agent != "" {
		return agent == i.committedAgentNameLocked()
	}
	return true
}

// committedAgentNameLocked names the agent the committed manual account swap
// recorded, the same way committedAccountSwap's manual branch does — through
// sessionenv.AgentForCommand(i.Program). A committed cross-agent manual swap
// rewrites i.Program to the incoming agent at the identity checkpoint, but the
// bound tmux pane keeps reporting the outgoing agent until setLaunchProgram
// relaunches it. The live pane is therefore the wrong source for matching a
// retry's --to <target> in the pre-relaunch window: the committed record names
// the target the recovery path will actually run, and committedAccountSwap
// already re-derives its agent from that record rather than the live pane.
//
// Automatic swaps do not rewrite i.Program or append a ledger entry, so the
// live pane remains the correct source for them; fall back to
// currentAgentNameLocked() when the pending swap is not manual, or when the
// committed record is a wrapper the literal classifier cannot resolve (a
// same-agent swap whose configured command af cannot identify as a single
// agent invocation). Callers hold i.mu.
func (i *Instance) committedAgentNameLocked() string {
	if i.pendingAccountSwap != nil && i.pendingAccountSwap.Manual {
		if agent := sessionenv.AgentForCommand(i.Program); agent != "" {
			return agent
		}
	}
	return i.currentAgentNameLocked()
}

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
