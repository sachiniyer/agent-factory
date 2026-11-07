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
// either swap kind — and releases a parked OUTGOING-identity limit
// (limit_account == pending.From here) the way the delivered send's own
// success path does (#6767). The INCOMING-identity wall survives instead —
// see its #6799 twin below.
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

// The #6799 settle twin: pane evidence retires the notice obligation but must
// NOT lift a wall attributed to the swap's INCOMING identity — the proof is
// that the notice landed, not that the agent's quota recovered. limit_account
// == pending.To is the Park{Manual,Automatic}AccountSwapAtLimit attribution:
// a readiness wait that found the replacement's own quota. A row flattened
// to LiveRunning here leaves ResumeLimitedSessions' scan and, behind an
// attached client's paused poll, never re-parks — stranded outside the
// scheduled recovery the parked row was waiting for.
//
// The wall must ALSO postdate the send being settled (#6921 review): its
// durable observation's ObservedAt is newer than the attempt instant the
// evidence retires — a wall sighted before that send is stale, not fresh
// quota proof.
func TestSettlePendingAccountSwapNoticeDeliveryKeepsTheIncomingWall(t *testing.T) {
	attemptAt := time.Now().Add(-time.Minute)
	for _, manual := range []bool{false, true} {
		inst := noticePendingSwap(manual, PromptCouldNotConfirm, attemptAt)
		if manual {
			// The manual transaction records the incoming agent namespace;
			// attribution matches on both axes.
			inst.pendingAccountSwap.AccountAgent = "claude"
		}
		inst.limitResetAt = time.Now().Add(time.Hour)
		inst.limitAgent = "claude"
		inst.limitAccount = "personal" // == pending.To: the incoming identity's wall
		// The wall's sighting POSTDATES the recorded send — a later retry's
		// readiness wait hit the replacement's own quota after this attempt
		// landed (the Park{Manual,Automatic}AccountSwapAtLimit ledger row).
		inst.accountLimitObservations = []AccountLimitObservationData{
			{Agent: "claude", Account: "personal", ResetAt: inst.limitResetAt, ObservedAt: attemptAt.Add(time.Second)},
		}
		require.NoError(t, inst.SettlePendingAccountSwapNoticeDelivery("work", "personal", attemptAt))
		require.Nil(t, inst.pendingAccountSwap, "the evidence retires the pending swap")
		require.Nil(t, inst.accountSwapLaunch)
		require.Equal(t, LiveLimitReached, inst.GetLiveness(),
			"an incoming-account limit recorded after the send survives the evidence settle")
		if reset, ok := inst.LimitResetAt(); !ok || reset.IsZero() {
			t.Fatal("the incoming wall's reset must survive the settle")
		}
		agent, account, ok := inst.LimitIdentity()
		require.True(t, ok, "the wall's attribution survives with it")
		require.Equal(t, "claude", agent)
		require.Equal(t, "personal", account)
	}
}

// The settle site's stale-wall arm (#6921 review MED): a wall attributed to
// the incoming identity but sighted BEFORE the send being settled is stale —
// the send demonstrably got past it — so the settle clears it the way master
// did. Only a wall sighted after the pending attempt instant is fresh
// evidence the replacement is still quota-blocked.
func TestSettlePendingAccountSwapNoticeDeliveryClearsAPreSendIncomingWall(t *testing.T) {
	attemptAt := time.Now().Add(-time.Minute)
	for _, manual := range []bool{false, true} {
		inst := noticePendingSwap(manual, PromptCouldNotConfirm, attemptAt)
		if manual {
			inst.pendingAccountSwap.AccountAgent = "claude"
		}
		inst.limitResetAt = time.Now().Add(time.Hour)
		inst.limitAgent = "claude"
		inst.limitAccount = "personal" // == pending.To on the label axis
		// The wall's sighting PREDATES the send — the incoming quota wall
		// that parked the row before this attempt ever reached the composer.
		inst.accountLimitObservations = []AccountLimitObservationData{
			{Agent: "claude", Account: "personal", ResetAt: inst.limitResetAt, ObservedAt: attemptAt.Add(-time.Hour)},
		}
		require.NoError(t, inst.SettlePendingAccountSwapNoticeDelivery("work", "personal", attemptAt))
		require.Nil(t, inst.pendingAccountSwap)
		require.Equal(t, LiveRunning, inst.GetLiveness(),
			"a wall recorded before the settled send is stale — the send got past it — and must clear")
		if reset, ok := inst.LimitResetAt(); ok || !reset.IsZero() {
			t.Fatal("the pre-send wall must retire with the settle")
		}
		_, _, ok := inst.LimitIdentity()
		require.False(t, ok, "no attribution may survive the cleared pre-send wall")
	}
}

// A manual cross-agent swap that reuses the outgoing label in a different
// provider's namespace (Claude `work` → Codex `work`) must clear the wall at
// the settle site too: limit_account == pending.To yet limit_agent does not
// match pending.AccountAgent, so it is the outgoing provider's wall, not the
// replacement's (#6921-review addendum — the two-axis check at settle).
func TestSettlePendingAccountSwapNoticeDeliveryClearsTheCrossAgentSameLabelWall(t *testing.T) {
	attemptAt := time.Now().Add(-time.Minute)
	inst := noticePendingSwap(true, PromptCouldNotConfirm, attemptAt)
	inst.pendingAccountSwap.From = "work"
	inst.pendingAccountSwap.To = "work" // same label, different provider namespace
	inst.pendingAccountSwap.AccountAgent = "codex"
	inst.Account = "work"
	inst.limitResetAt = time.Now().Add(time.Hour)
	inst.limitAgent = "claude" // outgoing provider — != pending.AccountAgent
	inst.limitAccount = "work" // == pending.To on the label axis alone
	inst.accountLimitObservations = []AccountLimitObservationData{
		{Agent: "claude", Account: "work", ResetAt: inst.limitResetAt, ObservedAt: attemptAt.Add(time.Second)},
	}
	require.NoError(t, inst.SettlePendingAccountSwapNoticeDelivery("work", "work", attemptAt))
	require.Equal(t, LiveRunning, inst.GetLiveness(),
		"the outgoing provider's wall clears even though the account label matches the target")
}

// A pending record whose attempt instant is ZERO has no send for the wall to
// be stale against — the shape a crash between Begin and Record leaves —
// so attribution alone decides and the incoming wall survives (#6921-review
// test gap). Clearing it would strand the session outside the scheduled
// recovery the parked row was waiting for, the exact harm #6799 fixed.
func TestSettlePendingAccountSwapNoticeDeliveryKeepsTheIncomingWallOnAZeroAttempt(t *testing.T) {
	for _, manual := range []bool{false, true} {
		inst := noticePendingSwap(manual, PromptCouldNotConfirm, time.Time{})
		if manual {
			inst.pendingAccountSwap.AccountAgent = "claude"
		}
		inst.limitResetAt = time.Now().Add(time.Hour)
		inst.limitAgent = "claude"
		inst.limitAccount = "personal" // == pending.To: the incoming identity's wall
		inst.accountLimitObservations = []AccountLimitObservationData{
			{Agent: "claude", Account: "personal", ResetAt: inst.limitResetAt, ObservedAt: time.Now()},
		}
		require.NoError(t, inst.SettlePendingAccountSwapNoticeDelivery("work", "personal", time.Time{}))
		require.Nil(t, inst.pendingAccountSwap)
		require.Equal(t, LiveLimitReached, inst.GetLiveness(),
			"no attempt instant means no send exists that could have proven the wall stale — attribution keeps it")
		agent, account, ok := inst.LimitIdentity()
		require.True(t, ok)
		require.Equal(t, "claude", agent)
		require.Equal(t, "personal", account)
	}
}

// A ledger row with a ZERO ObservedAt carries no provable sighting instant —
// it cannot be told apart from a wall recorded before the send, so it reads
// as stale and the settle clears it (#6921-review test gap).
func TestSettlePendingAccountSwapNoticeDeliveryClearsAZeroObservedAtWall(t *testing.T) {
	attemptAt := time.Now().Add(-time.Minute)
	for _, manual := range []bool{false, true} {
		inst := noticePendingSwap(manual, PromptCouldNotConfirm, attemptAt)
		if manual {
			inst.pendingAccountSwap.AccountAgent = "claude"
		}
		inst.limitResetAt = time.Now().Add(time.Hour)
		inst.limitAgent = "claude"
		inst.limitAccount = "personal" // == pending.To on the label axis
		inst.accountLimitObservations = []AccountLimitObservationData{
			{Agent: "claude", Account: "personal", ResetAt: inst.limitResetAt, ObservedAt: time.Time{}},
		}
		require.NoError(t, inst.SettlePendingAccountSwapNoticeDelivery("work", "personal", attemptAt))
		require.Equal(t, LiveRunning, inst.GetLiveness(),
			"an unprovable sighting instant reads as stale — the wall clears")
		_, _, ok := inst.LimitIdentity()
		require.False(t, ok, "no attribution may survive the cleared wall")
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
