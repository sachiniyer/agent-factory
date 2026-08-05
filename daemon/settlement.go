package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// settleOwedEntry identifies a session whose settlement write did not land, so
// the poll can retry it. Keyed elsewhere by stable instance identity; the pointer
// is what proves the row has not been replaced since.
type settleOwedEntry struct {
	repoID   string
	key      string
	instance *session.Instance
}

// A SETTLEMENT is the write that records the outcome of an irreversible step —
// the class of persist in this package that may not be best-effort.
//
// persistInstance's contract is that a dropped write is a checkpoint "the next
// poll/tick will re-attempt". That holds for status, and only for status: the
// poll's change detection (persistPollChange) covers liveness and the limit reset
// time and nothing else. Any other fact a settlement carries is invisible to it,
// so a lost write survives every later writer and is repaired only by the
// whole-state shutdown checkpoint — which the unclean exit that makes the
// divergence matter is exactly what skips.
//
// What that costs depends on the fact:
//
//   - a handoff's PendingHandoffMission is a standing instruction, so losing its
//     clear makes the next daemon deliver a mission the agent already ran (#2781);
//   - a recovery's branchCreatedByUs says af created this branch and may delete
//     it, so losing the flip leaves an af-* branch nothing will ever clean up
//     (#2883, and the outcome #1841 named).
//
// So settlement writes are durable AND retried: the failure reaches the caller
// instead of a log line, and the row joins a retry set the poll drains until the
// write lands. It announces like every other committed change (#2782) — memory
// has already moved, whether or not disk agreed yet.
func (m *Manager) persistSettlement(repoID, key string, instance *session.Instance) error {
	// persistAndPublishInstanceErr goes through startLockForRepo, which takes m.mu
	// — the #2106 lock contract, so the bookkeeping below happens after it returns.
	err := m.persistAndPublishInstanceErr(repoID, instance)
	owedKey := stableSessionKey(repoID, instance)
	m.mu.Lock()
	if err != nil {
		m.settleOwed[owedKey] = settleOwedEntry{repoID: repoID, key: key, instance: instance}
	} else {
		delete(m.settleOwed, owedKey)
	}
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf(
			"the settled state for %q could not be written to disk "+
				"(the daemon retries it on its poll; an unclean exit before it lands would lose this outcome): %w",
			instance.Title, err)
	}
	return nil
}

// FlushOwedSettlements retries settlement writes that did not land, so a
// transient failure self-heals within a poll tick instead of leaving the next
// daemon to load an outcome that never happened. Driven from the daemon poll, and
// a no-op in the overwhelmingly common case where no write ever failed.
func (m *Manager) FlushOwedSettlements() {
	m.mu.Lock()
	owed := make([]settleOwedEntry, 0, len(m.settleOwed))
	for _, entry := range m.settleOwed {
		owed = append(owed, entry)
	}
	m.mu.Unlock()

	for _, entry := range owed {
		m.mu.Lock()
		registered := m.instances[entry.key] == entry.instance
		if !registered {
			delete(m.settleOwed, stableSessionKey(entry.repoID, entry.instance))
		}
		m.mu.Unlock()
		// The row is gone (killed) or a successor took its key. Its record is being
		// deleted or belongs to another instance now, so writing this snapshot back
		// would fight whatever removed it — and there is nothing left to settle
		// either way, because a tombstone outranks every other marker on load
		// (session.FromInstanceData).
		if !registered {
			continue
		}
		if err := m.flushOneOwedSettlement(entry); err != nil {
			log.WarningLog.Printf("settlement retry for %q: %v", entry.instance.Title, err)
		}
	}
}

// flushOneOwedSettlement retries one owed write, but only while the session
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
func (m *Manager) flushOneOwedSettlement(entry settleOwedEntry) error {
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
	return m.persistSettlement(entry.repoID, entry.key, entry.instance)
}

// clearOwedSettlement retires a row's owed retry because a LATER write already
// made its state durable.
//
// A multi-step operation can persist the whole row again after a settlement
// failed — ToInstanceData carries everything, so the later write subsumes the
// earlier one. Leaving the entry enrolled would retry a write nothing needs, and
// leaving its error to be reported would tell a caller that a fully durable
// operation failed. Both are wrong in the same direction: describing a gap that
// has since closed.
func (m *Manager) clearOwedSettlement(repoID string, instance *session.Instance) {
	m.mu.Lock()
	delete(m.settleOwed, stableSessionKey(repoID, instance))
	m.mu.Unlock()
}
