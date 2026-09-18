package daemon

import (
	"errors"
	"slices"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// The poll's persist gate (#960 targeted writer, #1146 reset-time arm,
// #4361 account-evidence compare): the single choke point that decides which
// session-state changes the daemon's status poll makes durable and announces.
// Split out of limit.go to keep that file under its length ceiling (#1145).

// testHookPollBeforePublish runs immediately before persistPollChange announces
// its session.updated, inside the repo start lock it persisted under. Tests
// substitute it to prove the publish really is in that critical section — the
// property that keeps an older whole-session payload from landing after a newer
// tab roster. No-op in production.
var testHookPollBeforePublish = func() {}

// testHookPollBeforePersistLock runs in persistPollChange after it has decided to
// write or publish and BEFORE it takes the repo start lock — the exact window a
// concurrent transition or tab mutation can land in and be overwritten by this
// poll's older payload. Tests substitute it to land that mutation
// deterministically, with no goroutines or sleeps. No-op in production.
var testHookPollBeforePersistLock = func() {}

// testHookPollBeforeSettlementRecord runs after the poll's durable write and
// before its settlement bookkeeping. Both must remain inside the repo start
// lock: tests use this seam to pin that ordering without a scheduler race.
var testHookPollBeforeSettlementRecord = func() {}

// persistPollChange writes an instance's state to disk when the poll changed
// something durable this tick (the #960 targeted writer): its LIVENESS
// transitioned, OR — evaluated INDEPENDENTLY — its usage-limit reset time changed
// (#1146). The liveness is the compared axis (#1195), so a Ready→LimitReached idle
// transition (invisible to the old composed-Status compare, since LimitReached
// composes to Ready) is caught as a genuine change.
//
// The reset-time check MUST be independent of the liveness compare: a row can
// enter LiveLimitReached on one tick with no parsed reset time (the banner
// matched but the time was not yet captured/parseable) and only parse it on a
// LATER tick. That later tick leaves the liveness unchanged, so gating
// persistence on the liveness alone would silently drop the reset time — the
// [limit] resets <t> badge would never show it, and PR3's auto-resume scheduler
// would have no time to schedule against once the daemon restarts and reloads
// from disk. beforeReset is the reset time captured before this tick's poll.
//
// projectionChanged additionally announces a live-only diagnostic change without
// writing it to disk. This is the event-plane half of projection-only state: a
// quiet session can change its diagnostic while liveness and reset time remain
// identical, and already-open clients must not wait for an unrelated transition.
// settlementCheckpoint marks a durable one-shot change outside liveness/reset,
// currently the first post-prompt churn edge or retirement of a terminal Lost
// restore failure after positive liveness evidence.
//
// A concurrent client op (create/kill/archive) means that op's executor owns the
// durable state, so the poll never persists over it. Split from
// refreshInstanceStatus so control.go stays under its length ceiling (#1145).
//
// WHAT IS WRITTEN OR PUBLISHED IS WHAT IS TRUE UNDER THE ORDERING LOCK (#2135).
// The change test above is a decision about a payload read at one instant, and
// the write/publish happens at another — after a repo start lock that a session
// create can hold for seconds.
// An authoritative transition landing in between (a usage-limit resume clearing
// the block and persisting LiveRunning, above all) would otherwise be overwritten
// by the intermediate this poll decided from, and the reset-time arm is what
// carried it there: it fires INDEPENDENTLY of the liveness, so a poll whose
// liveness compare read "unchanged" (LimitReached → LimitReached) still flushed —
// planting a limit-blocked row on disk for a session that was working.
//
// So the payload is ALWAYS re-read under the lock once the lock-free gate decides
// there is something to announce: the gate decides WHETHER to write/publish,
// never WHAT. A lifecycle epoch cannot safely optimize this re-read because the
// payload also contains projection state outside that epoch — notably the tab
// roster. Deliberately do not take the lock before the gate: taking it on every
// tick of every session would park the whole poll behind an unrelated create.
func (m *Manager) persistPollChange(
	repoID string,
	instance *session.Instance,
	before session.Liveness,
	beforeReset time.Time,
	beforeObserved time.Time,
	beforeAccountEvidence []session.AccountLimitObservationData,
	projectionChanged bool,
) {
	m.persistPollChangeWithIdleEvidence(repoID, instance, before, beforeReset, beforeObserved, beforeAccountEvidence, projectionChanged, false)
}

func (m *Manager) persistPollChangeWithIdleEvidence(
	repoID string,
	instance *session.Instance,
	before session.Liveness,
	beforeReset time.Time,
	beforeObserved time.Time,
	beforeAccountEvidence []session.AccountLimitObservationData,
	projectionChanged bool,
	settlementCheckpoint bool,
) {
	if instance.GetInFlightOp() != session.OpNone {
		// The write is skipped, but a consumed one-shot checkpoint must not be
		// silently dropped: record the obligation so a later poll re-attempts it.
		if settlementCheckpoint {
			key := daemonInstanceKey(repoID, instance.Title)
			m.recordSettlementWrite(repoID, key, instance, errors.New("ceded to in-flight op"))
		}
		return
	}
	data := instance.ToInstanceData()
	livenessChanged := data.Liveness != before
	resetChanged := !data.LimitResetAt.Equal(beforeReset)
	// The FIRST sighting of a wall is durable evidence of its own: a record
	// loaded with LiveLimitReached but no observation time (every row written
	// before the field existed) changes neither liveness nor reset when the
	// poll re-observes the same banner, so the stamp it just earned would stay
	// memory-only. The gate is the zero edge, so per-tick re-observation of a
	// still-parked session costs no write.
	observedNewlyStamped := beforeObserved.IsZero() && !data.LimitObservedAt.IsZero()
	// Account limit evidence is durable too (#4361 review): a repeat sighting
	// re-dates the (agent, account) observation's "last seen" stamp on the same
	// per-tick edge, so comparing the slice — not another zero edge — is what
	// makes a quantized refresh, a new observation, or a safer reset merge reach
	// disk instead of rolling back on the next daemon restart.
	accountEvidenceChanged := !slices.Equal(beforeAccountEvidence, data.AccountLimitObservations)
	durableChanged := livenessChanged || resetChanged || observedNewlyStamped || accountEvidenceChanged || settlementCheckpoint
	publishChanged := durableChanged || projectionChanged
	if !publishChanged {
		return
	}
	repoStartLock := m.startLockForRepo(repoID)
	testHookPollBeforePersistLock()
	repoStartLock.Lock()
	// Re-read the WHOLE projection after joining the same ordering domain as tab
	// mutations and session creation. InFlightOpAndEpoch intentionally covers
	// lifecycle state only, so using it as a whole-payload change detector lets
	// an untracked roster mutation publish first and then get erased by this stale event.
	data = instance.ToInstanceData()
	// The lock-free gate above can pass an OpNone a client op raises before the
	// re-read lands. A handoff is the concrete case: between the gate and this
	// lock it runs BeginHandoff (OpReplacing) and RecordHandoffSwap (rewriting
	// Program to the incoming agent before its mission marker exists), and it
	// holds the per-(repo,title) and op locks — neither of which is repoStartLock
	// — so acquiring repoStartLock does not exclude it. persistInstanceData →
	// ForStorage strips the transient op axis, so persisting that snapshot stores
	// the incoming agent as SETTLED with no delivery obligation: a state the
	// session never legitimately reached. Re-check under the lock and cede to
	// the op's executor, which owns the durable state — the same fence the
	// settlement retry holds under the op lock (settlement.go). The gate decides
	// WHETHER to write; an op holding the session decides WHO writes.
	if data.InFlightOp != session.OpNone {
		// The write is skipped, but a consumed one-shot checkpoint must not be
		// silently dropped: record the obligation so a later poll re-attempts it.
		if settlementCheckpoint {
			key := daemonInstanceKey(repoID, instance.Title)
			m.recordSettlementWrite(repoID, key, instance, errors.New("ceded to in-flight op"))
		}
		repoStartLock.Unlock()
		return
	}
	var err error
	if durableChanged {
		err = persistInstanceData(repoID, data)
	}
	key := daemonInstanceKey(repoID, instance.Title)
	testHookPollBeforeSettlementRecord()
	switch {
	case err == nil && durableChanged:
		// Any successful whole-row checkpoint subsumes an older evidence write.
		m.recordSettlementWrite(repoID, key, instance, nil)
	case err != nil && settlementCheckpoint:
		// These checkpoint edges are one-shot in memory. Preserve the obligation
		// after a failed write so a later poll can make the outcome durable.
		m.recordSettlementWrite(repoID, key, instance, err)
	}
	// Push the change onto the events plane (#1592 PR5): this is the single choke
	// point every liveness/limit transition already flows through, so one publish
	// here covers session.updated without threading it through each caller.
	//
	// Published while STILL HOLDING the repo start lock, in the same critical section
	// as the persist that produced `data` — matching CreateTab/CloseTab. session.updated
	// carries a WHOLE InstanceData and every client re-projects the session wholesale
	// from it, so publish order is not cosmetic: it decides which snapshot wins. Publishing
	// after the unlock let this poll capture a roster, release the lock, and be preempted
	// by a tab create/delete that persisted AND announced the grown roster first — then
	// this older payload landed last and clients re-projected the tab right back out of
	// existence, until some later update happened to repair it (post-merge Codex finding
	// on #1815). Serializing publish with persist makes the last event the newest state.
	// publishEvent is non-blocking (disconnect-slow), so a wedged subscriber can't stall the poll.
	testHookPollBeforePublish()
	m.publishEvent(agentproto.EventSessionUpdated, data)
	repoStartLock.Unlock()
	if err != nil {
		m.warn().Printf("daemon failed to persist status for %q: %v", instance.Title, err)
	}
}
