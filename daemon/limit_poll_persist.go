package daemon

import (
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

func (m *Manager) persistPollChangeWithIdleEvidence(
	repoID string,
	instance *session.Instance,
	before session.Liveness,
	beforeReset time.Time,
	beforeEvidence string,
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
	// The change gate reads the fields it diffs through their accessors rather
	// than a whole ToInstanceData: the snapshot is the largest single allocator
	// in the #6845 profile, and on an unchanged tick nothing here is used beyond
	// these comparisons. The accessors carry the same liveness gating the
	// projection applies (LimitResetAt/LimitResetEvidence report their stored
	// values only while LiveLimitReached), so the verdicts are identical; the
	// full projection is still re-read under the lock below whenever something
	// actually changed — that build is both the write and the publish payload.
	livenessChanged := instance.GetLiveness() != before
	resetAt, _ := instance.LimitResetAt()
	resetChanged := !resetAt.Equal(beforeReset)
	// Evidence is durable too (#6765 post-merge): a send-path park stores a
	// reset with NO phrase, so the first poll that adopts the banner's phrase
	// changes nothing else — and dropping it would leave a restart re-parsing
	// the same banner against a later clock.
	evidenceChanged := instance.LimitResetEvidence() != beforeEvidence
	// A wall observation can change — a first sighting, a fresh wall event
	// after recovery, a tightened reset claim — while every axis above stays
	// put. The instance flags it like an unpersisted conversation, or the
	// stored record would carry a fact the reconcile no longer decides
	// against (#6802 review). Re-seeing an UNCHANGED wall flags nothing and
	// writes nothing — see recordAccountLimitObservationLocked.
	obsChanged := instance.AccountLimitObservationsUnpersisted()
	// The durable wall record can mutate under an unchanged liveness: a new
	// emission re-parking the row rewrites its anchor and drops the send
	// tombstone — a fact no axis above sees, and one a restart must not lose
	// or the scheduler settles a wall it never answered (#6972, Codex).
	wallChanged := instance.LimitWallUnpersisted()
	durableChanged := livenessChanged || resetChanged || evidenceChanged || settlementCheckpoint || conversationNeedsWrite(instance) || obsChanged || wallChanged
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
	// The row-write lock (#6210) is taken before the fence re-check below and held
	// through the write: the admission writers (restore) do not take the start
	// lock, so without it a restore could raise its fence and persist between this
	// re-check and the write, and this older snapshot would then overwrite it.
	rowLock := m.rowWriteLockForRepo(repoID)
	rowLock.Lock()
	data := instance.ToInstanceData()
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
		rowLock.Unlock()
		repoStartLock.Unlock()
		return
	}
	var err error
	if durableChanged {
		err = persistInstanceData(repoID, data)
		rowLock.Unlock()
		noteConversationWritten(instance, data, err)
		if err == nil {
			instance.MarkAccountLimitObservationsPersisted(data.AccountLimitObservations)
			instance.MarkLimitWallPersisted(data.LastLimitWall)
		}
	} else {
		rowLock.Unlock()
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
