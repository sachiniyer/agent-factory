package session

import "time"

// IdleReason is the daemon's mechanically established explanation for why a
// session is not doing visible work. It deliberately excludes semantic guesses
// about pane content: a question, a completed task, and a wedged agent can render
// alike, so none of those is a value in this vocabulary.
type IdleReason string

const (
	IdleReasonNone                      IdleReason = ""
	IdleReasonUsageLimit                IdleReason = "usage-limit"
	IdleReasonProcessExited             IdleReason = "process-exited"
	IdleReasonRecreatePending           IdleReason = "recreate-pending"
	IdleReasonPromptNotDelivered        IdleReason = "prompt-not-delivered"
	IdleReasonDeliveryUnconfirmed       IdleReason = "delivery-unconfirmed"
	IdleReasonNoPaneChangeSinceDelivery IdleReason = "no-pane-change-since-delivery"
	IdleReasonSettledAfterPaneChange    IdleReason = "settled-after-pane-change"
)

// Label is the short human wording shared by row renderers. Unknown future
// values render nothing rather than inviting an older client to interpret them.
func (r IdleReason) Label() string {
	switch r {
	case IdleReasonUsageLimit:
		return "usage limit"
	case IdleReasonProcessExited:
		return "process exited"
	case IdleReasonRecreatePending:
		return "recreate notice pending"
	case IdleReasonPromptNotDelivered:
		return "prompt not delivered"
	case IdleReasonDeliveryUnconfirmed:
		return "delivery unconfirmed"
	case IdleReasonNoPaneChangeSinceDelivery:
		return "no pane change since delivery"
	case IdleReasonSettledAfterPaneChange:
		return "settled after pane change"
	default:
		return ""
	}
}

// IdleReasonFor derives the public reason from closed, mechanically observed
// facts. It does not trust InstanceData.IdleReason: that field is a projection,
// and deriving it here keeps persisted evidence the source of truth.
func IdleReasonFor(data InstanceData) IdleReason {
	if data.InFlightOp != OpNone {
		return IdleReasonNone
	}

	liveness := livenessFromData(data)
	switch liveness {
	case LiveLimitReached:
		return IdleReasonUsageLimit
	case LiveLost, LiveDead:
		return IdleReasonProcessExited
	case LiveReady:
		// The prompt/recreate evidence below only explains a settled Ready row.
	default:
		return IdleReasonNone
	}

	switch data.RootRecreateContext {
	case RootRecreateContextFresh, RootRecreateContextUnknown:
		return IdleReasonRecreatePending
	}

	if data.LastPromptAttemptAt.IsZero() {
		return IdleReasonNone
	}
	if data.LastPromptDeliveryStatus == PromptNotDelivered {
		return IdleReasonPromptNotDelivered
	}
	if data.LastPaneChurnAt.After(data.LastPromptAttemptAt) {
		return IdleReasonSettledAfterPaneChange
	}
	switch data.LastPromptDeliveryStatus {
	case PromptCouldNotConfirm:
		return IdleReasonDeliveryUnconfirmed
	case PromptDelivered:
		return IdleReasonNoPaneChangeSinceDelivery
	default:
		return IdleReasonNone
	}
}

// ProjectIdleReason recomputes the projection field from its evidence. It is
// used by daemonless disk-list fallback as well as live Instance snapshots.
func (d InstanceData) ProjectIdleReason() InstanceData {
	d.IdleReason = IdleReasonFor(d)
	return d
}

// RecordPromptAttempt stores the observation made by an actual prompt send.
// attemptedAt must be captured before delivery begins, so a pane observation
// racing the send can still be ordered after it. Invalid local values normalize
// to honest uncertainty; a zero timestamp establishes no order and is ignored.
func (i *Instance) RecordPromptAttempt(status PromptDeliveryStatus, attemptedAt time.Time) bool {
	if attemptedAt.IsZero() {
		return false
	}
	if !status.Valid() {
		status = PromptCouldNotConfirm
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lastPromptAttemptAt.Equal(attemptedAt) && i.lastPromptDeliveryStatus == status {
		return false
	}
	i.lastPromptAttemptAt = attemptedAt
	i.lastPromptDeliveryStatus = status
	return true
}

// RecordPaneChurnAtEpoch records that an Observation reported Updated for the
// same runtime generation the caller observed. A lifecycle fence raised during
// capture invalidates the observation exactly as it invalidates liveness.
func (i *Instance) RecordPaneChurnAtEpoch(churnAt time.Time, observedEpoch uint64) bool {
	if churnAt.IsZero() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != observedEpoch || !churnAt.After(i.lastPaneChurnAt) {
		return false
	}
	i.lastPaneChurnAt = churnAt
	return true
}

// ReconcileIdleEvidence mirrors the daemon's evidence onto a client row model.
// It applies both directions because runtime replacement can clear or replace
// the evidence, and the daemon snapshot is authoritative for all three fields.
func (i *Instance) ReconcileIdleEvidence(attemptedAt time.Time, status PromptDeliveryStatus, churnAt time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lastPromptAttemptAt.Equal(attemptedAt) &&
		i.lastPromptDeliveryStatus == status &&
		i.lastPaneChurnAt.Equal(churnAt) {
		return false
	}
	i.lastPromptAttemptAt = attemptedAt
	i.lastPromptDeliveryStatus = status
	i.lastPaneChurnAt = churnAt
	return true
}

// IdleReasonSnapshot returns the derived reason and last observed pane churn in
// one lock hold for row renderers.
func (i *Instance) IdleReasonSnapshot() (IdleReason, time.Time) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	data := InstanceData{
		Status:                   i.statusLocked(),
		Liveness:                 i.liveness,
		InFlightOp:               i.inFlightOp,
		RootRecreateContext:      i.rootRecreateContext,
		LastPromptAttemptAt:      i.lastPromptAttemptAt,
		LastPromptDeliveryStatus: i.lastPromptDeliveryStatus,
		LastPaneChurnAt:          i.lastPaneChurnAt,
	}
	return IdleReasonFor(data), i.lastPaneChurnAt
}
