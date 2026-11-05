package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// noticePendingSwap is the #6767 fixture: a committed account swap of either
// kind whose replacement panes started, carrying the notice verdict and the
// send-attempt instant the re-verification watch binds to.
func noticePendingSwap(manual bool, status PromptDeliveryStatus, attemptedAt time.Time) *Instance {
	return &Instance{
		Title:    "swapped",
		Account:  "personal",
		started:  true,
		liveness: LiveLimitReached,
		pendingAccountSwap: &AccountSwapData{
			Manual: manual, From: "work", To: "personal", ReplacementPanesStarted: true,
			MissionDeliveryStatus: status, MissionDeliveryAttemptAt: attemptedAt,
		},
		accountSwapLaunch: &accountSwapLaunchPlan{account: "personal", base: "claude", program: "claude"},
	}
}

// An automatic swap's notice obligates the same durable verdict the manual
// mission's does: begin fences replay before the composer is touched, record
// binds the send's exact verdict AND its attempt instant, and a failed
// persistence restores the displaced pair rather than leaving a send behind
// that its verdict cannot describe.
func TestAccountSwapNoticeDeliveryBoundaryAndVerdict(t *testing.T) {
	inst := noticePendingSwap(false, "", time.Time{})
	inst.lastPromptAttemptAt = time.Now()

	prev, prevAttempt, err := inst.BeginPendingAccountSwapNoticeDelivery("work", "personal")
	require.NoError(t, err)
	require.Equal(t, PromptDeliveryStatus(""), prev, "a first attempt displaces no verdict")
	require.True(t, prevAttempt.IsZero())
	require.Equal(t, PromptCouldNotConfirm, inst.pendingAccountSwap.MissionDeliveryStatus,
		"the begin boundary durably closes the replay gate before the send")
	require.True(t, inst.pendingAccountSwap.MissionDeliveryAttemptAt.IsZero(),
		"and the in-flight attempt has no boundary yet")

	require.NoError(t, inst.RecordPendingAccountSwapNoticeDelivery("work", "personal", PromptNotDelivered))
	require.Equal(t, PromptNotDelivered, inst.pendingAccountSwap.MissionDeliveryStatus)
	require.Equal(t, inst.lastPromptAttemptAt, inst.pendingAccountSwap.MissionDeliveryAttemptAt,
		"the recorded verdict binds the send's own attempt instant")

	// A second attempt displaces the recorded verdict; a persistence failure
	// must restore the pair it replaced, not just the status.
	older := inst.pendingAccountSwap.MissionDeliveryAttemptAt
	prev, prevAttempt, err = inst.BeginPendingAccountSwapNoticeDelivery("work", "personal")
	require.NoError(t, err)
	require.Equal(t, PromptNotDelivered, prev)
	require.Equal(t, older, prevAttempt)
	require.NoError(t, inst.RestorePendingAccountSwapNoticeDelivery("work", "personal", prev, prevAttempt))
	require.Equal(t, PromptNotDelivered, inst.pendingAccountSwap.MissionDeliveryStatus)
	require.Equal(t, older, inst.pendingAccountSwap.MissionDeliveryAttemptAt)
}

// The scheduler's replay gate reads the durable verdict for either swap kind:
// positive non-delivery and a never-attempted send admit it, every other
// outcome may already have submitted and stays off the scheduler (#6767).
func TestPendingAccountSwapDeliveryUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status PromptDeliveryStatus
		want   bool
	}{
		{"sent-unverified stays off the scheduler", PromptSentUnverified, true},
		{"could-not-confirm stays off the scheduler", PromptCouldNotConfirm, true},
		{"delivered crash window stays off the scheduler", PromptDelivered, true},
		{"not-delivered admits automatic replay", PromptNotDelivered, false},
		{"never attempted admits automatic replay", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := noticePendingSwap(false, tc.status, time.Now())
			require.Equal(t, tc.want, inst.PendingAccountSwapDeliveryUnconfirmed())
		})
	}
}

// SettlePendingAccountSwapNoticeDelivery retires the obligation on pane
// evidence bound to exactly the send that produced the recorded verdict —
// either swap kind — and releases the parked limit the way the delivered
// send's own success path does (#6767).
func TestSettlePendingAccountSwapNoticeDelivery(t *testing.T) {
	attemptAt := time.Now().Add(-time.Minute)
	for _, manual := range []bool{false, true} {
		for _, status := range []PromptDeliveryStatus{PromptSentUnverified, PromptCouldNotConfirm, PromptDelivered} {
			inst := noticePendingSwap(manual, status, attemptAt)
			inst.limitResetAt = time.Now().Add(time.Hour)
			inst.limitAccount = "work"
			require.NoError(t, inst.SettlePendingAccountSwapNoticeDelivery("work", "personal", attemptAt))
			require.Nil(t, inst.pendingAccountSwap, "the evidence retires the pending swap")
			require.Nil(t, inst.accountSwapLaunch, "and its admitted launch plan")
			require.Equal(t, LiveRunning, inst.GetLiveness(),
				"a notice the agent took is a working session, and the stale wall lifts with it")
			require.True(t, inst.limitResetAt.IsZero())
			require.Empty(t, inst.limitAccount)
		}
	}
}

func TestSettlePendingAccountSwapNoticeDeliveryRefusals(t *testing.T) {
	attemptAt := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name   string
		mutate func(*Instance)
	}{
		{"evidence bound to a different send", func(i *Instance) { i.pendingAccountSwap.MissionDeliveryAttemptAt = attemptAt.Add(time.Second) }},
		{"not-delivered belongs to automatic recovery", func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered }},
		{"no verdict recorded", func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = "" }},
		{"no pending swap", func(i *Instance) { i.pendingAccountSwap = nil }},
		{"replacement panes never started", func(i *Instance) { i.pendingAccountSwap.ReplacementPanesStarted = false }},
		{"kill tombstone", func(i *Instance) { i.userKilled = true }},
		{"lost", func(i *Instance) { i.liveness = LiveLost }},
		{"dead", func(i *Instance) { i.liveness = LiveDead }},
		{"archived", func(i *Instance) { i.liveness = LiveArchived }},
		{"resume fence held by a retry", func(i *Instance) { i.inFlightOp = OpRespawning }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := noticePendingSwap(false, PromptSentUnverified, attemptAt)
			tc.mutate(inst)
			before := inst.pendingAccountSwap
			require.Error(t, inst.SettlePendingAccountSwapNoticeDelivery("work", "personal", attemptAt))
			require.Same(t, before, inst.pendingAccountSwap,
				"a refused settle must not retire the obligation")
		})
	}
}

// The accessor reports the durable verdict/boundary pair the watch re-reads,
// and nothing when no verdict exists.
func TestPendingAccountSwapNoticeDeliveryAccessor(t *testing.T) {
	inst := noticePendingSwap(false, PromptSentUnverified, time.Time{})
	attemptAt := time.Now()
	inst.pendingAccountSwap.MissionDeliveryAttemptAt = attemptAt
	from, to, status, at, ok := inst.PendingAccountSwapNoticeDelivery()
	require.True(t, ok)
	require.Equal(t, "work", from)
	require.Equal(t, "personal", to)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, attemptAt, at)

	inst.pendingAccountSwap.MissionDeliveryStatus = ""
	_, _, _, _, ok = inst.PendingAccountSwapNoticeDelivery()
	require.False(t, ok, "no recorded verdict means no watch may arm")
}
