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
	durableChanged := livenessChanged || resetChanged || settlementCheckpoint || conversationNeedsWrite(instance)
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
		rowLock.Unlock()
		repoStartLock.Unlock()
		return
	}
	var err error
	if durableChanged {
		err = persistInstanceData(repoID, data)
		rowLock.Unlock()
		noteConversationWritten(instance, data, err)
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
