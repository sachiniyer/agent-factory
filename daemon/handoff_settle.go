package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// Handoff settlement is the write that DISCHARGES a delivery obligation, and it
// is the one persist in this package that may not be best-effort (#2781).
//
// PendingHandoffMission on disk is a standing instruction, not a status field: a
// daemon that starts and finds one reconstructs the replacement fence and sends
// that exact brief (session.FromInstanceData). That is what makes an interrupted
// handoff recoverable — and it is what makes a LOST settlement write dangerous,
// because the marker then outlives a mission the agent already ran. The next
// daemon delivers it a second time and the session executes the same task twice,
// with every side effect that implies.
//
// persistInstance is therefore the wrong primitive here. Its contract is that a
// dropped write is a checkpoint "the next poll/tick will re-attempt", but the
// poll's change detection (persistPollChange) covers liveness and the limit reset
// time only. Nothing in it looks at PendingHandoffMission, so this divergence is
// invisible to every later writer and survives until the shutdown checkpoint —
// which the unclean exit that makes the divergence matter never reaches.
//
// So settlement writes are durable AND retried: the failure reaches the caller
// instead of a log line, and the row joins a retry set the handoff poll drains
// until the write lands. It announces like every other settlement (#2782) — the
// fence has come down in memory whether or not disk agreed yet.
func (m *Manager) persistHandoffSettlement(repoID, key string, instance *session.Instance) error {
	// persistAndPublishInstanceErr goes through startLockForRepo, which takes m.mu
	// — the #2106 lock contract, so the bookkeeping below happens after it returns.
	err := m.persistAndPublishInstanceErr(repoID, instance)
	owedKey := remoteLossKey(repoID, instance)
	m.mu.Lock()
	if err != nil {
		m.handoffSettleOwed[owedKey] = pendingHandoffEntry{repoID: repoID, key: key, instance: instance}
	} else {
		delete(m.handoffSettleOwed, owedKey)
	}
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf(
			"the settled handoff state for %q could not be written to disk "+
				"(the daemon retries it on its poll; an unclean exit before it lands would redeliver the mission): %w",
			instance.Title, err)
	}
	return nil
}

// flushHandoffSettlements retries settlement writes that did not land, so a
// transient failure self-heals within a poll tick instead of leaving a stale
// delivery obligation on disk for the next daemon to replay. It is driven from
// ResumePendingHandoffs — the poll pass that already owns handoff durability —
// and is a no-op in the overwhelmingly common case where no write ever failed.
func (m *Manager) flushHandoffSettlements() {
	m.mu.Lock()
	owed := make([]pendingHandoffEntry, 0, len(m.handoffSettleOwed))
	for _, entry := range m.handoffSettleOwed {
		owed = append(owed, entry)
	}
	m.mu.Unlock()

	for _, entry := range owed {
		m.mu.Lock()
		registered := m.instances[entry.key] == entry.instance
		if !registered {
			delete(m.handoffSettleOwed, remoteLossKey(entry.repoID, entry.instance))
		}
		m.mu.Unlock()
		// The row is gone (killed) or a successor took its key. Its record is being
		// deleted or belongs to another instance now, so writing this snapshot back
		// would fight whatever removed it — and there is no obligation left to
		// discharge either way, because a tombstone outranks a pending mission on
		// load (session.FromInstanceData).
		if !registered {
			continue
		}
		if err := m.flushOneHandoffSettlement(entry); err != nil {
			log.WarningLog.Printf("handoff %q: %v", entry.instance.Title, err)
		}
	}
}

// flushOneHandoffSettlement retries one owed write, but only while the session
// is between operations.
//
// A retry is a WHOLE-ROW write of live memory, so running it inside another
// session transaction would checkpoint that transaction's half-built state. A
// second handoff is the concrete case: it raises OpReplacing and rewrites Program
// BEFORE it records its own mission marker, and disk strips the transient op — so
// a retry landing in that window stores the incoming agent as settled with no
// obligation at all, and a crash before the real checkpoint loses that takeover
// brief entirely. Trading a duplicated mission for a lost one is not a fix.
//
// The per-session op lock is what actually serializes this, because that is the
// lock every lifecycle operation holds for its whole transaction; the in-flight-op
// re-check under it is the same fence persistPollChange puts in front of the only
// other poll-driven whole-row write. TryLock, never Lock: the poll goroutine must
// not stall behind a slow teardown, and a skipped retry costs nothing — the
// obligation stays owed for the next tick, and a newer settlement on the same row
// discharges it outright.
func (m *Manager) flushOneHandoffSettlement(entry pendingHandoffEntry) error {
	opLock := m.opLockFor(entry.key)
	if !opLock.TryLock() {
		return nil
	}
	defer opLock.Unlock()

	// Re-verify under the lock. Registration can change while the lock is acquired,
	// and an op raised outside it must not be flattened by this write.
	m.mu.Lock()
	registered := m.instances[entry.key] == entry.instance
	m.mu.Unlock()
	if !registered || entry.instance.GetInFlightOp() != session.OpNone {
		return nil
	}
	return m.persistHandoffSettlement(entry.repoID, entry.key, entry.instance)
}
