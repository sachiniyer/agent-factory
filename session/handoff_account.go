package session

import (
	"fmt"
	"strings"
	"time"
)

// ValidateHandoffRuntimeAction evaluates RuntimeActionHandoff for a request
// that names its target. A committed account swap fences every other lifecycle
// action, but a request carrying that swap's own account and agent is the retry
// the refusal's message advertises — the pending-swap axis alone cannot refuse
// it (#4393). Every other axis still applies, and a request naming a different
// account or agent remains a new transaction the pending swap still owns.
func (i *Instance) ValidateHandoffRuntimeAction(agent, account string) error {
	i.mu.RLock()
	view := i.lifecycleViewLocked()
	if view.PendingAccountSwap && i.pendingAccountSwapRetryTargetLocked(agent, account) {
		// Recovery owns this exact committed transaction, including the
		// post-commit blind-teardown wedge. The latter marks
		// StartupStateUnknown and clears Started while preserving the committed
		// swap; this retry is the inspection the marker waits for, so suppress
		// those gates alongside the pending-swap one. RebuildTeardownPending
		// must lift too: the retry's respawn is the teardown's OWNER — the gate
		// exists to fence NEW bindings, not the path that discharges the
		// obligation (#5371 review).
		view.PendingAccountSwap = false
		view.StartupStateUnknown = false
		view.Started = true
		view.RebuildTeardownPending = false
	}
	i.mu.RUnlock()
	return view.ValidateRuntimeAction(RuntimeActionHandoff)
}

// pendingAccountSwapRetryTargetLocked reports whether agent and account name
// the committed swap's own target: the account the identity checkpoint already
// moved this session to and — when the request names an agent — a target the
// committed transaction already is. That is the requested enum in i.Program OR
// the resolved agent the pane now runs: a program_overrides redirect makes the
// two differ (`--to aider` recording Program=aider while launching codex), and
// a retry may spell the committed identity either way (#4430 review round 3).
// Callers hold i.mu.
func (i *Instance) pendingAccountSwapRetryTargetLocked(agent, account string) bool {
	pending := i.pendingAccountSwap
	if pending == nil || pending.To != i.Account || strings.TrimSpace(account) != pending.To {
		return false
	}
	if agent = strings.TrimSpace(agent); agent != "" {
		if agent == i.Program || agent == i.currentAgentNameLocked() {
			return true
		}
		// The committed transaction's own target spelling: a same-agent alias
		// (`--to aider` where aider resolves to the running codex) keeps
		// i.Program on the established label while the ledger records To=aider,
		// so the retry matches neither identity above without this (#4430
		// review round 6). The trailing ledger entry IS this transaction's —
		// a pending manual swap exists only because its commit appended it.
		if pending.Manual && len(i.Tabs) > 0 {
			h := i.Tabs[0].Handoffs
			return len(h) > 0 && h[len(h)-1].To == agent
		}
		return false
	}
	return true
}

// PendingAccountSwapTarget reports the agent enum the committed manual swap was
// requested under — the trailing ledger entry's To — or "" when no committed
// manual transaction is pending. A same-agent alias (`--to aider` whose
// override resolves to the running codex) keeps Program on the established
// label while the transaction's true target lives only here, so retry and
// replay detection must consult it as well as the two identity spellings
// (#4430 review round 6).
func (i *Instance) PendingAccountSwapTarget() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	pending := i.pendingAccountSwap
	if pending == nil || !pending.Manual || pending.To != i.Account || len(i.Tabs) == 0 {
		return ""
	}
	h := i.Tabs[0].Handoffs
	if len(h) == 0 {
		return ""
	}
	return h[len(h)-1].To
}

// BeginManualAccountSwap raises the existing account-replacement fence after
// validating the handoff lifecycle, including healthy and limit-blocked rows.
func (i *Instance) BeginManualAccountSwap() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	view := i.lifecycleViewLocked()
	if pending := i.pendingAccountSwap; pending != nil && pending.Manual && pending.To == i.Account {
		// Recovery owns this exact committed transaction, including healthy
		// checkpoints and the post-commit blind-teardown wedge. The latter
		// (ErrAccountSwapAgentTeardownBlind) marks StartupStateUnknown and
		// clears Started while preserving the committed swap, so the explicit
		// retry — the inspection the marker waits for — must reach the alreadySet
		// recovery branches of resumeFromLimitLockedOutcome. Suppress the gates
		// that exist to hold automatic retries back until that inspection lands
		// — including RebuildTeardownPending, whose retrying respawn IS this
		// path's destination (#5371 review).
		view.PendingAccountSwap = false
		view.StartupStateUnknown = false
		view.Started = true
		view.RebuildTeardownPending = false
	}
	if err := view.ValidateRuntimeAction(RuntimeActionHandoff); err != nil {
		return err
	}
	return i.transitionLocked(BeginRespawn())
}

// SelectAccountForHandoff commits the same identity transaction as automatic
// rotation, retaining an explicit pin and its durable delivery obligation.
// target and effectiveAgent travel separately for the same reason they do in
// recordHandoffSwapLocked: an override can redirect the launch — the ledger's
// To and the recorded Program name the enum the operator asked for, while the
// scope judgment is made on the agent the command resolves to (#4430 review).
// crossAgent is the decision ValidateManualAccountSwap froze the launch plan
// under; passing the same value keeps the record describing that launch.
func (i *Instance) SelectAccountForHandoff(from, name, target, effectiveAgent string, crossAgent bool, reason, head, mission string) (HandoffSwap, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning {
		return HandoffSwap{}, fmt.Errorf("account handoff requires the replacement fence")
	}
	entry, err := i.recordHandoffSwapLocked(target, effectiveAgent, crossAgent, reason, head, false)
	if err != nil {
		return HandoffSwap{}, err
	}
	entry.FromAccount, entry.ToAccount = from, name
	i.Tabs[0].Handoffs[len(i.Tabs[0].Handoffs)-1] = entry.AgentHandoff
	if _, err := i.selectAccountLocked(from, name, effectiveAgent, false); err != nil {
		return HandoffSwap{}, err
	}
	i.pendingAccountSwap.Manual = true
	i.pendingAccountSwap.Mission = mission
	// The namespace the account was just selected in travels with the durable
	// transaction: post-commit recovery must answer it even after a restart
	// under changed program_overrides, when neither the pane's (rewritten)
	// program metadata nor a fresh resolution can still prove it (#4430 review).
	i.pendingAccountSwap.AccountAgent = effectiveAgent
	return entry, nil
}

// PendingAccountSwapAgent reports the account namespace the committed manual
// swap's replacement account was selected in, or "" when the pending record
// predates the field or is not a committed manual swap. Recovery prefers it
// over any re-derivation: it is the one answer a config flip plus daemon
// restart cannot move (#4430 review).
func (i *Instance) PendingAccountSwapAgent() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.pendingAccountSwap == nil || !i.pendingAccountSwap.Manual {
		return ""
	}
	return i.pendingAccountSwap.AccountAgent
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
	if pending.MissionDeliveryStatus != status || !pending.MissionDeliveryAttemptAt.Equal(i.lastPromptAttemptAt) {
		pending.MissionDeliveryStatus = status
		pending.MissionDeliveryAttemptAt = i.lastPromptAttemptAt
		i.touchLocked()
	}
	return nil
}

// BeginPendingManualAccountSwapMissionDelivery closes the durable replay gate
// before a delivery attempt touches the composer. A crash after this boundary
// reloads ambiguity, never the positive non-delivery verdict that admitted the
// attempt.
//
// The displaced verdict's attempt instant retires with it (#6798), the same
// contract the automatic notice's begin already carries: notice evidence is
// only ever armed against the timestamp of the attempt that is actually
// pending, and a positively-missed attempt's timestamp never counts. Without
// the retire a crash after the boundary reloads (could-not-confirm, STALE
// attempt instant), the re-verification watch arms against the missed
// attempt's timestamp, and a turn begun after it — with no relation to the
// never-submitted retry — satisfies the evidence contract and retires a
// mission that was never sent. It returns the displaced pair so a failed
// persistence can restore them atomically.
func (i *Instance) BeginPendingManualAccountSwapMissionDelivery(from, to string) (PromptDeliveryStatus, time.Time, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || !pending.Manual || pending.From != from || pending.To != to {
		return "", time.Time{}, fmt.Errorf("manual account swap from %q to %q is no longer pending", from, to)
	}
	if !pending.ReplacementPanesStarted {
		return "", time.Time{}, fmt.Errorf("manual account swap from %q to %q has no replacement panes", from, to)
	}
	previous := pending.MissionDeliveryStatus
	previousAttempt := pending.MissionDeliveryAttemptAt
	if previous != PromptCouldNotConfirm || !previousAttempt.IsZero() {
		pending.MissionDeliveryStatus = PromptCouldNotConfirm
		pending.MissionDeliveryAttemptAt = time.Time{}
		i.touchLocked()
	}
	return previous, previousAttempt, nil
}

// RestorePendingManualAccountSwapMissionDelivery rolls a begin boundary back
// to the exact verdict/attempt pair it displaced when persistence failed
// before submission began — status AND instant together, so the restored
// record is exactly what evidence armed against before the boundary (#6798).
// Empty status is allowed here: an initial delivery with no prior attempt has
// no verdict to restore.
func (i *Instance) RestorePendingManualAccountSwapMissionDelivery(from, to string, status PromptDeliveryStatus, attemptedAt time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || !pending.Manual || pending.From != from || pending.To != to {
		return fmt.Errorf("manual account swap from %q to %q is no longer pending", from, to)
	}
	if status != "" && !status.Valid() {
		return fmt.Errorf("invalid manual account swap mission delivery status %q", status)
	}
	if pending.MissionDeliveryStatus != status || !pending.MissionDeliveryAttemptAt.Equal(attemptedAt) {
		pending.MissionDeliveryStatus = status
		pending.MissionDeliveryAttemptAt = attemptedAt
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
	return i.pendingAccountSwap != nil && i.pendingAccountSwap.Manual &&
		i.pendingAccountSwapDeliveryUnconfirmedLocked()
}

// pendingAccountSwapDeliveryUnconfirmedLocked is the same verdict read for a
// pending swap of either kind. An automatic swap's durable verdict gates its
// scheduled retry the same way a manual swap's does (#6767): only positive
// non-delivery — or a never-attempted send, the empty verdict — may authorize
// the scheduler's resend; every other outcome may already have submitted and
// belongs to evidence or the operator. Caller holds i.mu.
func (i *Instance) pendingAccountSwapDeliveryUnconfirmedLocked() bool {
	pending := i.pendingAccountSwap
	if pending == nil || !pending.ReplacementPanesStarted || pending.MissionDeliveryStatus == "" {
		return false
	}
	return pending.MissionDeliveryStatus != PromptNotDelivered
}

// PendingAccountSwapDeliveryUnconfirmed reports whether a committed account
// swap of either kind may already have delivered its replacement notice (#6767).
func (i *Instance) PendingAccountSwapDeliveryUnconfirmed() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingAccountSwapDeliveryUnconfirmedLocked()
}

// PendingAccountSwapNoticeDelivery reports the committed swap's durable
// notice-delivery verdict and the send-attempt instant that produced it — the
// pair the daemon's re-verification watch re-reads before retiring the
// obligation on pane evidence. ok is false when no verdict has been recorded.
func (i *Instance) PendingAccountSwapNoticeDelivery() (from, to string, status PromptDeliveryStatus, attemptedAt time.Time, ok bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	pending := i.pendingAccountSwap
	if pending == nil || pending.MissionDeliveryStatus == "" {
		return "", "", "", time.Time{}, false
	}
	return pending.From, pending.To, pending.MissionDeliveryStatus, pending.MissionDeliveryAttemptAt, true
}

// BeginPendingAccountSwapNoticeDelivery is the automatic swap's half of
// BeginPendingManualAccountSwapMissionDelivery (#6767): the same durable
// replay gate — an ambiguous verdict replacing whatever admitted this
// attempt — written before the composer is touched, so a crash reloads
// could-not-confirm rather than permission to send the notice a second time.
// It returns the displaced verdict AND its attempt instant so a failed
// persistence can restore the pair atomically.
func (i *Instance) BeginPendingAccountSwapNoticeDelivery(from, to string) (PromptDeliveryStatus, time.Time, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || pending.From != from || pending.To != to {
		return "", time.Time{}, fmt.Errorf("account swap from %q to %q is no longer pending", from, to)
	}
	if !pending.ReplacementPanesStarted {
		return "", time.Time{}, fmt.Errorf("account swap from %q to %q has no replacement panes", from, to)
	}
	previous := pending.MissionDeliveryStatus
	previousAttempt := pending.MissionDeliveryAttemptAt
	if previous != PromptCouldNotConfirm || !previousAttempt.IsZero() {
		pending.MissionDeliveryStatus = PromptCouldNotConfirm
		pending.MissionDeliveryAttemptAt = time.Time{}
		i.touchLocked()
	}
	return previous, previousAttempt, nil
}

// RestorePendingAccountSwapNoticeDelivery rolls a begin boundary back to the
// exact verdict/attempt pair it displaced when persistence failed before
// submission began. Empty status is allowed for an initial attempt.
func (i *Instance) RestorePendingAccountSwapNoticeDelivery(from, to string, status PromptDeliveryStatus, attemptedAt time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || pending.From != from || pending.To != to {
		return fmt.Errorf("account swap from %q to %q is no longer pending", from, to)
	}
	if status != "" && !status.Valid() {
		return fmt.Errorf("invalid account swap notice delivery status %q", status)
	}
	if pending.MissionDeliveryStatus != status || !pending.MissionDeliveryAttemptAt.Equal(attemptedAt) {
		pending.MissionDeliveryStatus = status
		pending.MissionDeliveryAttemptAt = attemptedAt
		i.touchLocked()
	}
	return nil
}

// RecordPendingAccountSwapNoticeDelivery binds the send's exact verdict to the
// committed transaction, stamping the attempt instant the session's prompt
// evidence already recorded — the boundary the re-verification watch binds
// turn evidence to (#6767).
func (i *Instance) RecordPendingAccountSwapNoticeDelivery(from, to string, status PromptDeliveryStatus) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || pending.From != from || pending.To != to {
		return fmt.Errorf("account swap from %q to %q is no longer pending", from, to)
	}
	if !pending.ReplacementPanesStarted {
		return fmt.Errorf("account swap from %q to %q has no replacement panes", from, to)
	}
	if !status.Valid() {
		status = PromptCouldNotConfirm
	}
	if pending.MissionDeliveryStatus != status || !pending.MissionDeliveryAttemptAt.Equal(i.lastPromptAttemptAt) {
		pending.MissionDeliveryStatus = status
		pending.MissionDeliveryAttemptAt = i.lastPromptAttemptAt
		i.touchLocked()
	}
	return nil
}

// pendingSwapIncomingWallLocked reports whether the row's recorded limit wall
// is a fresh wall of the committed swap's INCOMING identity — the replacement
// the swap moved this session to (#6799). Either kind can be parked there:
// ParkManualAccountSwapAtLimit before mission submission,
// ParkAutomaticAccountSwapAtLimit when an automatic retry's readiness wait
// found the replacement's own quota, and ParkCommittedAccountSwapAtLimit when
// a committed swap's pre-spawn wall re-check found the committed identity
// limited between the commit's evidence read and the launch (#6925).
//
// Two tests must both hold. ATTRIBUTION: the wall matches on both axes it
// carries — the account label (limit_account == pending.To) and the agent
// namespace (limit_agent == pending.AccountAgent): a cross-agent manual swap
// may reuse the outgoing label in a different provider's namespace (Claude
// `work` → Codex `work`), where the account axis alone misattributes the
// outgoing provider's wall to the incoming identity. An automatic swap never
// crosses agents and carries no AccountAgent — as do records written before
// the field — so both fall back to the account axis alone. RECENCY: the
// wall's sighting must postdate the send being retired (#6921 review) — a
// wall recorded BEFORE the confirmed/settled attempt is stale evidence: the
// send demonstrably got past it, and keeping it lets the scheduler resume a
// working agent. A pending record with no attempt instant has no send for
// the wall to be stale against, so attribution alone decides.
func (i *Instance) pendingSwapIncomingWallLocked(pending *AccountSwapData) bool {
	if i.limitAccount != pending.To ||
		(pending.AccountAgent != "" && i.limitAgent != pending.AccountAgent) {
		return false
	}
	if pending.MissionDeliveryAttemptAt.IsZero() {
		return true
	}
	return i.incomingLimitObservedAtLocked().After(pending.MissionDeliveryAttemptAt)
}

// incomingLimitObservedAtLocked is the recorded sighting instant of the row's
// current limit wall — the newest ObservedAt among the durable
// account-limit observation rows carrying the same (limitAgent, limitAccount)
// attribution. Zero when the ledger has no matching row (ambient walls,
// which the ledger skips, and rows written before the field existed): an
// unprovable sighting orders before any recorded send, so the wall reads as
// stale rather than fresh (#6921 review). Caller holds i.mu.
func (i *Instance) incomingLimitObservedAtLocked() time.Time {
	var observedAt time.Time
	for idx := range i.accountLimitObservations {
		o := &i.accountLimitObservations[idx]
		if o.Agent == i.limitAgent && o.Account == i.limitAccount &&
			o.ObservedAt.After(observedAt) {
			observedAt = o.ObservedAt
		}
	}
	return observedAt
}

// SettlePendingAccountSwapNoticeDelivery retires the committed swap's notice
// obligation on POSITIVE pane evidence — a turn the replacement agent provably
// began after this notice's send attempt (#6767) — rather than on an
// operator's attestation the way ConfirmPendingAccountSwapDelivery does.
// A recorded PromptDelivered verdict settles identically: it IS the proof, and
// the pending record its crash-window settle never reached would otherwise
// outlive the evidence for it. attemptedAt is the compare-and-set: the
// evidence is bound to exactly the send that produced the recorded verdict,
// so a newer attempt (its own verdict already written or in flight) refuses
// the stale evidence rather than being settled by it. The caller feeds the
// daemon's re-verification watch, which never arms startup-unknown rows, so
// unlike the operator confirm this needs no pane-probe bookkeeping — the
// evidence IS the runtime proof.
func (i *Instance) SettlePendingAccountSwapNoticeDelivery(from, to string, attemptedAt time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || pending.From != from || pending.To != to {
		return fmt.Errorf("account swap from %q to %q is no longer pending", from, to)
	}
	if !pending.ReplacementPanesStarted {
		return fmt.Errorf("account swap from %q to %q has no replacement panes; nothing could have been delivered", from, to)
	}
	if !confirmableHandoffDelivery(pending.MissionDeliveryStatus) {
		return fmt.Errorf("account swap from %q to %q has no evidence-settleable notice delivery (status %q)", from, to, pending.MissionDeliveryStatus)
	}
	if !pending.MissionDeliveryAttemptAt.Equal(attemptedAt) {
		return fmt.Errorf("account swap from %q to %q has moved past the notice attempt this evidence belongs to", from, to)
	}
	if i.userKilled {
		return fmt.Errorf("session %q has a pending kill", i.Title)
	}
	if i.liveness == LiveLost || i.liveness == LiveDead || i.liveness == LiveArchived {
		return fmt.Errorf("session %q has no live runtime to settle against (liveness %v); restore owns this row", i.Title, i.liveness)
	}
	if i.inFlightOp != OpNone {
		return fmt.Errorf("session %q is busy (%v)", i.Title, i.inFlightOp)
	}
	lv, op, resetAt := i.lifecycleStateLocked()
	i.pendingAccountSwap = nil
	i.accountSwapLaunch = nil
	i.taskRunIdleEdgeHeld = false
	i.taskRunTurnGateHeld = false
	if i.liveness == LiveLimitReached {
		// The parked wall splits on attribution, not on the swap's kind
		// (#6799): a wall recorded against the INCOMING identity is still the
		// honest state — the agent really is quota-blocked, the row already
		// publishes not-idle to fleet watch, and ResumeLimitedSessions only
		// owns rows that still carry it. Evidence that the incoming agent
		// took its notice does not prove its quota recovered. Any other
		// parked wall is the OUTGOING identity's — stale the moment the
		// notice lands — and retires the way the delivered send's own
		// success path settles it.
		if !i.pendingSwapIncomingWallLocked(pending) {
			i.assignLivenessLocked(LiveRunning)
			i.limitResetAt = time.Time{}
			i.limitAgent = ""
			i.limitAccount = ""
		}
	} else {
		// A confirmed notice is work the agent already has (#5023): publish
		// working and leave the settle back to Ready to the monitor's own
		// pane evidence.
		_ = i.transitionLocked(ObserveLiveness(LiveRunning))
	}
	i.touchLocked()
	i.noteStateChangeLocked(lv, op, resetAt)
	return nil
}

// CanRetryPendingManualAccountSwapDelivery reports whether an operator can
// inspect a known replacement pane and explicitly override ambiguous delivery.
//
// The limit wall is admitted here even though it is excluded from the
// agent-handoff twin (CanRetryPendingHandoffMissionDelivery): the daemon's
// manual-swap ResumeFromLimit fork re-pastes the pending mission
// (deliverManualAccountMission), so the row's restart IS a resend. Admitting
// LiveLimitReached routes the row to the honest "Retry send — submit the
// pending mission again" picker label instead of the agent-handoff-twin's
// "Resume from limit — the pending mission is not resent" label, which is
// true only for the agent-handoff arm whose daemon sends the session goal
// (SendPromptWithEvidence) without resending.
//
// Startup-unknown is admitted for the same reason: the daemon's
// committed-swap bypass (committedAccountSwap) admits the explicit
// ResumeFromLimit RPC, and BeginManualAccountSwap skips the startup check —
// the marker suppresses only automatic retries until the operator inspects,
// and the explicit retry IS that inspection. Refusing it here would offer
// the picker's no-resend label on a row the daemon resends (#6922 review).
func (i *Instance) CanRetryPendingManualAccountSwapDelivery() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	knownLive := i.liveness == LiveRunning || i.liveness == LiveReady || i.liveness == LiveLimitReached
	return (knownLive || i.startupStateUnknown) && i.inFlightOp == OpNone && !i.userKilled &&
		i.pendingManualAccountSwapDeliveryUnconfirmedLocked()
}

// CanRetryPendingAccountSwapNoticeDelivery is the automatic swap's half of
// CanRetryPendingManualAccountSwapDelivery (#6800): the daemon's
// ResumeFromLimit fork re-submits the committed notice
// (deliverAutomaticAccountNotice → SendPromptWithEvidence), so a parked row
// whose notice verdict is ambiguous is an honest resend — not the no-resend
// restart the agent-handoff arm performs.
//
// The admission mirrors the daemon's, not the manual twin's:
// resumeFromLimitLockedOutcome returns unperformed for a non-limited
// automatic swap, so only the limit wall admits — a running/ready row would
// offer a resend the daemon will not perform. Startup-unknown AT the wall is
// still admitted: the committed-swap bypass and BeginLimitResume's
// committed-swap exemption resend the notice there, so the explicit retry is
// again the inspection the marker waits for. Positive non-delivery stays
// with the scheduler and every other row keeps its owner.
func (i *Instance) CanRetryPendingAccountSwapNoticeDelivery() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness == LiveLimitReached && i.inFlightOp == OpNone && !i.userKilled &&
		i.pendingAutomaticAccountSwapDeliveryUnconfirmedLocked()
}

func (i *Instance) pendingAutomaticAccountSwapDeliveryUnconfirmedLocked() bool {
	return i.pendingAccountSwap != nil && !i.pendingAccountSwap.Manual &&
		i.pendingAccountSwapDeliveryUnconfirmedLocked()
}

// CanConfirmPendingAccountSwapDelivery reports whether an operator can
// retire a pending account swap's mission on the attestation that it
// already landed — the same "it is already running" exit the agent-handoff
// path gained in #4429, and the exit an ambiguous AUTOMATIC notice needs when
// its re-verification window closed unanswered or never armed (#6767). Unlike
// the retry predicate this DOES admit startup-unknown and orphaned-fence rows:
// the daemon probes the pane before honoring the attestation, which is the
// runtime proof those rows were missing.
func (i *Instance) CanConfirmPendingAccountSwapDelivery() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	knownLive := i.liveness == LiveRunning || i.liveness == LiveReady || i.liveness == LiveLimitReached
	dead := i.liveness == LiveLost || i.liveness == LiveDead || i.liveness == LiveArchived
	return !i.userKilled && !dead && (i.startupStateUnknown || knownLive) &&
		(i.inFlightOp == OpNone || i.inFlightOp == OpRespawning) &&
		i.pendingAccountSwap != nil &&
		i.pendingAccountSwap.ReplacementPanesStarted &&
		confirmableHandoffDelivery(i.pendingAccountSwap.MissionDeliveryStatus)
}

// ConfirmPendingAccountSwapDelivery retires the pending account swap on the
// operator's attestation that its mission already landed (#4429; any swap kind
// since #6767). It is the account-swap half of ConfirmPendingHandoffDelivery:
// the daemon has already probed the pane alive, so this method re-checks the
// durable facts and then clears the transaction and its admitted launch plan,
// lifts any orphaned replacement fence, and resolves a startup-unknown flag
// the probe just disproved — all in one critical section so no later reader
// can rebuild the wedge.
//
// The attestation also decides the row's liveness (#5023): a mission the
// replacement agent already received means the agent HAS work, including on a
// row a failed delivery left reading idle — clearing the marker there would
// publish a record indistinguishable from an ordinary settled-idle session,
// and fleet watch would emit the working → idle edge an automated driver acts
// on. The row therefore settles on LiveRunning, the same state a successfully
// delivered prompt leaves, and only the status monitor's own idle observation
// — real pane evidence — moves it back.
//
// A row parked at its usage-limit wall splits on the wall's ATTRIBUTION, not
// the swap's kind (#6799). Either kind may be parked at the INCOMING
// identity's own wall — a manual swap whose replacement hit its quota before
// mission submission (ParkManualAccountSwapAtLimit), an automatic swap whose
// retry's readiness wait found the replacement's own quota
// (ParkAutomaticAccountSwapAtLimit), or a committed swap whose pre-spawn wall
// re-check found the committed identity limited before launch
// (ParkCommittedAccountSwapAtLimit, #6925) — and LiveLimitReached is still the
// honest state, it already publishes not-idle to fleet watch, and
// ResumeLimitedSessions only owns rows that still carry it — flattening it
// would strand a session whose poll is paused by an attach. The wall is the
// incoming identity's only when its attribution matches on BOTH axes — the
// account label (limit_account == pending.To) and the agent namespace
// (limit_agent == pending.AccountAgent): a cross-agent manual swap may reuse
// the outgoing label in a different provider's namespace (Claude `work` →
// Codex `work`), where the account axis alone misattributes the outgoing
// provider's wall to the incoming identity. Any OTHER parked wall is the
// OUTGOING/ambient identity's (the #6764 shape): the confirm nils the
// PendingAccountSwap pointer that AccountLimitEvidenceFromData guards
// against re-attributing that wall to the committed replacement, so the
// wall retires with the transaction — a confirmed notice already runs on
// the incoming identity's own quota.
func (i *Instance) ConfirmPendingAccountSwapDelivery(from, to string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || pending.From != from || pending.To != to {
		return fmt.Errorf("account swap from %q to %q is no longer pending", from, to)
	}
	if !pending.ReplacementPanesStarted {
		return fmt.Errorf("account swap from %q to %q has no replacement panes; nothing could have been delivered", from, to)
	}
	if !confirmableHandoffDelivery(pending.MissionDeliveryStatus) {
		return fmt.Errorf("account swap from %q to %q has no ambiguous delivery to confirm (status %q); retry-limit owns the resend", from, to, pending.MissionDeliveryStatus)
	}
	if i.userKilled {
		return fmt.Errorf("session %q has a pending kill", i.Title)
	}
	if i.liveness == LiveLost || i.liveness == LiveDead || i.liveness == LiveArchived {
		return fmt.Errorf("session %q has no live runtime to confirm against (liveness %v); restore owns this row", i.Title, i.liveness)
	}
	if i.inFlightOp != OpNone && i.inFlightOp != OpRespawning {
		return fmt.Errorf("session %q is busy (%v)", i.Title, i.inFlightOp)
	}
	lv, op, resetAt := i.lifecycleStateLocked()
	i.resolveStartupStateLocked()
	if i.inFlightOp == OpRespawning {
		// The daemon holds the op lock while calling, so a respawn fence still
		// up here is orphaned bookkeeping — a crashed/abandoned resume — not a
		// live operation. ClearOp preserves liveness and releases the row.
		if err := i.transitionLocked(ClearOp()); err != nil {
			return err
		}
	}
	// Retire the launch plan with the transaction, as ClearPendingAccountSwap
	// does: tabSpawnBlockedLocked refuses on either, so leaving the plan behind
	// would keep new tabs refused until the daemon restarted.
	i.stampHandoffConversationLocked()
	i.pendingAccountSwap = nil
	i.accountSwapLaunch = nil
	// The confirmed mission is work the incoming agent already has (#5023):
	// publish working — the same liveness a delivered prompt produces — and
	// leave the settle back to Ready to the monitor's own pane evidence. A
	// limit-blocked row splits on the wall's attribution alone (doc comment
	// above): either kind parked at the incoming identity's own wall keeps
	// it, while a wall recorded against any other identity retires with the
	// transaction it justified.
	if !pending.Manual {
		i.taskRunIdleEdgeHeld = false
		i.taskRunTurnGateHeld = false
	}
	if i.liveness == LiveLimitReached {
		if !i.pendingSwapIncomingWallLocked(pending) {
			i.assignLivenessLocked(LiveRunning)
			i.limitResetAt = time.Time{}
			i.limitAgent = ""
			i.limitAccount = ""
		}
	} else {
		_ = i.transitionLocked(ObserveLiveness(LiveRunning))
	}
	i.touchLocked()
	i.noteStateChangeLocked(lv, op, resetAt)
	return nil
}

// ReconcileAccountHandoffSnapshot mirrors the daemon-owned account identity —
// name, the namespace it was selected in, auto flag — and pending delivery
// transaction onto an existing client projection.
func (i *Instance) ReconcileAccountHandoffSnapshot(account, accountAgent string, auto bool, pending *AccountSwapData) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Account == account && i.accountAgent == accountAgent && i.accountAutoSelected == auto && accountSwapDataEqual(i.pendingAccountSwap, pending) {
		return false
	}
	i.Account = account
	i.accountAgent = accountAgent
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
func (i *Instance) ParkManualAccountSwapAtLimit(resetAt time.Time, evidence string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning || i.pendingAccountSwap == nil || !i.pendingAccountSwap.Manual {
		return fmt.Errorf("manual account limit requires the pending replacement fence")
	}
	return i.parkAccountSwapAtLimitLocked(i.currentAgentNameLocked(), resetAt, evidence)
}

// ParkCommittedAccountSwapAtLimit is the pre-spawn twin of the Park*AtLimit
// pair: the daemon's committed-identity re-admission found a wall on the
// incoming account BEFORE the replacement spawned, when pane and runtime
// evidence still run the outgoing identity and currentAgentNameLocked would
// file the incoming account's wall under the outgoing agent — a cross-agent
// handoff would then exclude an unrelated outgoing-namespace account and
// incomingManualReset would not recognize the incoming reset window (#6915
// review). The caller names the committed swap's namespace instead; every
// other contract — the fence, the incoming account already on i.Account, the
// manual path's not-delivered marker, the durable observation — matches the
// pair.
func (i *Instance) ParkCommittedAccountSwapAtLimit(agent string, resetAt time.Time, evidence string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning || i.pendingAccountSwap == nil {
		return fmt.Errorf("account limit park requires the pending replacement fence")
	}
	return i.parkAccountSwapAtLimitLocked(agent, resetAt, evidence)
}

// parkAccountSwapAtLimitLocked is the shared body of the Park*AtLimit family:
// the caller holds i.mu, has verified the pending replacement fence, and names
// the agent namespace the wall files under — currentAgentNameLocked for a wall
// found on the already-running replacement, the committed swap's namespace for
// one found before it spawned.
func (i *Instance) parkAccountSwapAtLimitLocked(agent string, resetAt time.Time, evidence string) error {
	lv, op, prevReset := i.lifecycleStateLocked()
	i.assignLivenessLocked(LiveLimitReached)
	i.limitResetAt = resetAt
	// The wait path resolved this reset and parsed its phrase; recording the
	// phrase keeps the daemon's first poll from re-parsing the unchanged banner
	// against a later clock and moving the stored reset (#6765).
	i.limitResetEvidence = evidence
	i.attributeLimitIdentityAsLocked(agent)
	// Readiness found the incoming identity's wall before mission submission,
	// which is positive non-delivery evidence for this transaction. Replace any
	// earlier ambiguity so the scheduler may resume it after the recorded
	// reset. The marker is consulted only on the manual path
	// (pendingManualAccountSwapDeliveryUnconfirmedLocked early-returns when the
	// swap is not manual), so automatic swaps never set it.
	if i.pendingAccountSwap.Manual && i.pendingAccountSwap.MissionDeliveryStatus != PromptNotDelivered {
		i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered
		i.touchLocked()
	}
	i.recordAccountLimitObservationLocked(agent, i.Account, resetAt, false)
	i.noteStateChangeLocked(lv, op, prevReset)
	return nil
}

// SetAccountSwapReplacementPanesStartedForTest seeds the committed account
// swap's replacement-pane flag. Test-only: the post-commit blind-teardown wedge
// (ErrAccountSwapAgentTeardownBlind) occurs during the STOP phase, before the
// respawn that sets this flag, so a committed swap that wedged there carries
// ReplacementPanesStarted=false. A fixture that strands delivery AFTER a
// successful respawn cannot reproduce that state through the real flow, so the
// test seeds it directly to exercise the live-pane repair branch.
func (i *Instance) SetAccountSwapReplacementPanesStartedForTest(started bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingAccountSwap != nil && i.pendingAccountSwap.ReplacementPanesStarted != started {
		i.pendingAccountSwap.ReplacementPanesStarted = started
		i.touchLocked()
	}
}

// ParkAutomaticAccountSwapAtLimit attributes a readiness wall to the incoming
// replacement identity without releasing the account transaction's fence. It is
// the automatic-account-swap twin of ParkManualAccountSwapAtLimit: the incoming
// identity is already committed (commitNewAccountSwapIdentity ran before
// settleReplacementRuntime), so both i.Account and resetAt belong to the incoming
// identity — the same invariant under which attributeLimitIdentityLocked and
// recordAccountLimitObservationLocked are correct.
//
// The live LimitIdentity and the durable accountLimitObservations ledger must
// carry the incoming wall so limitedAccountsForSwap excludes the incoming
// account from future candidate selection for the wall window. Without
// attribution the row hetero-binds (outgoing identity + incoming window) and the
// ledger never gains the incoming entry, so a sibling session may re-admit the
// still-exhausted incoming account and waste a swap cycle.
//
// Deliberately does NOT flip pendingAccountSwap.MissionDeliveryStatus: that
// non-delivery marker is consulted only on the manual path
// (pendingManualAccountSwapDeliveryUnconfirmedLocked early-returns when the
// swap is not manual), and the automatic resume path retries after the recorded
// reset unconditionally, with no operator-delivery step.
func (i *Instance) ParkAutomaticAccountSwapAtLimit(resetAt time.Time, evidence string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning || i.pendingAccountSwap == nil || i.pendingAccountSwap.Manual {
		return fmt.Errorf("automatic account limit requires the pending replacement fence")
	}
	return i.parkAccountSwapAtLimitLocked(i.currentAgentNameLocked(), resetAt, evidence)
}
