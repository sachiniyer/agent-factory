package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// dialogHoldEntry is what the poll remembers about one session's held launch
// dialog (#7635). Guarded by Manager.dialogHoldMu.
type dialogHoldEntry struct {
	// due is a hold the poll observed and has not yet flagged; nil when none.
	due *session.DialogHold
	// since names the hold this session was last flagged for. One held,
	// unchanged pane keeps one Since, so a hold is flagged once: a request the
	// user dismissed is not raised again for the same pane.
	since time.Time
	// setAt is the attention request this path owns, the only one it may
	// clear or replace: the one it raised, or its own notice found on the row
	// after a restart. Zero when it owns none. It is carried while a newer
	// hold is due, so that hold's flush replaces the request under the locks
	// instead of racing an asynchronous clear of it.
	setAt time.Time
	// recoveredAt is set on an entry made from a notice found on the row with
	// no hold observed: the daemon restarted, and the request outlived the
	// memory of who raised it. Zero otherwise.
	recoveredAt time.Time
}

// dialogHoldRecoveryGrace is how long a notice recovered after a restart may
// stand with no hold observed before it is cleared. A restarted daemon's own
// recorder starts its settle clock over, so a pane that is still held reports
// no hold for that long; clearing at once would blink the flag off and on at
// every restart. Three settle times covers the recorder and a slow first poll.
const dialogHoldRecoveryGrace = 30 * time.Second

// dialogHoldStep is the poll's whole decision about one observation, as a pure
// function of what it remembered and what it saw. hold is the observation's
// settled hold, or nil. It returns the entry to keep (nil to forget the
// session) and, when clear is true, the attention request to clear: the one
// this path owns, for a hold that has resolved.
//
// A hold that CHANGED clears nothing here. The request stays owned in the
// entry beside the new due hold, and raiseDueDialogHold replaces it under the
// attention locks. Queuing a clear for it instead would race that flush: the
// flush could find the old request, take it for the new hold's, and then
// lose it to the clear.
func dialogHoldStep(entry *dialogHoldEntry, hold *session.DialogHold) (next *dialogHoldEntry, clearSetAt time.Time, clear bool) {
	if hold == nil {
		// Answered, cleared, or the pane changed: the notice is withdrawn.
		if entry != nil && !entry.setAt.IsZero() {
			return nil, entry.setAt, true
		}
		return nil, time.Time{}, false
	}
	if entry != nil && !entry.since.IsZero() && entry.since.Equal(hold.Since) {
		// The hold this session is already flagged for: raise nothing more.
		kept := *entry
		kept.due = nil
		return &kept, time.Time{}, false
	}
	// A hold not flagged yet. Any request this path still owns rides along.
	due := *hold
	next = &dialogHoldEntry{due: &due}
	if entry != nil {
		next.setAt = entry.setAt
	}
	return next, time.Time{}, false
}

// noteDialogHold feeds one successful observation to the hold tracker. It only
// records: the attention write happens in raiseDueDialogHold, after the
// observation fence releases, the way the staged-draft flag is written. The
// clear it may queue runs on its own goroutine and is keyed by SetAt, so it
// removes nothing but the request this path owns.
func (m *Manager) noteDialogHold(key string, instance *session.Instance, hold *session.DialogHold) {
	m.dialogHoldMu.Lock()
	entry := m.dialogHolds[key]
	if entry == nil && hold == nil {
		m.dialogHoldMu.Unlock()
		// The common case: no hold, nothing remembered. One thing can still be
		// owed: this path's own notice left on the row by a daemon that
		// restarted before the hold resolved.
		m.recoverDialogHoldNotice(key, instance)
		return
	}
	if hold == nil && !entry.recoveredAt.IsZero() && nowFunc().Sub(entry.recoveredAt) < dialogHoldRecoveryGrace {
		m.dialogHoldMu.Unlock()
		return // a recovered notice waits out the restarted recorder's settle
	}
	next, clearSetAt, clear := dialogHoldStep(entry, hold)
	if next == nil {
		delete(m.dialogHolds, key)
	} else {
		if m.dialogHolds == nil {
			m.dialogHolds = make(map[string]*dialogHoldEntry)
		}
		m.dialogHolds[key] = next
	}
	m.dialogHoldMu.Unlock()
	if clear {
		m.queueAttentionClear(instance, clearSetAt)
	}
}

// recoverDialogHoldNotice takes ownership of a held-dialog notice found on a
// session the tracker remembers nothing about. The request is persisted with
// the row and the tracker is not, so after a restart a notice whose dialog was
// answered meanwhile would otherwise tell the user, for good, to answer a
// dialog that is gone. Owned again, it is cleared once no hold has been
// observed for dialogHoldRecoveryGrace, or kept if the hold is reported again.
//
// The notice is recognized by its wording, which only this path writes. A
// request with any other reason is never touched.
func (m *Manager) recoverDialogHoldNotice(key string, instance *session.Instance) {
	existing := instance.Attention()
	if existing == nil || !session.IsDialogHoldReason(existing.Reason) {
		return
	}
	m.dialogHoldMu.Lock()
	defer m.dialogHoldMu.Unlock()
	if m.dialogHolds[key] != nil {
		return
	}
	if m.dialogHolds == nil {
		m.dialogHolds = make(map[string]*dialogHoldEntry)
	}
	m.dialogHolds[key] = &dialogHoldEntry{setAt: existing.SetAt, recoveredAt: nowFunc()}
}

// raiseDueDialogHold flags the session for a hold noteDialogHold recorded as
// due. It reuses the attention signal and its write path unchanged: the
// opLock→repoStartLock order, TryLock on both so the serial poll never parks
// behind a create or a handoff, then persist and publish. On contention the
// due mark stays and the next poll retries.
//
// What is already on the row decides the write, read under those locks:
//   - nothing: raise the notice.
//   - the request this path owns, from an earlier hold of this pane: keep it
//     when the reason is the same, replace it when the reason changed.
//   - this same notice, unowned (the daemon restarted while the pane stayed
//     held): adopt it.
//   - anything else — the agent's own question, a staged-draft flag: leave
//     it. The due mark stays, and the notice goes up once the row is free.
func (m *Manager) raiseDueDialogHold(repoID, key string, instance *session.Instance) {
	m.dialogHoldMu.Lock()
	entry := m.dialogHolds[key]
	if entry == nil || entry.due == nil {
		m.dialogHoldMu.Unlock()
		return
	}
	hold, owned := *entry.due, entry.setAt
	m.dialogHoldMu.Unlock()

	opLock := m.opLockFor(daemonInstanceKey(repoID, instance.Title))
	if !opLock.TryLock() {
		return
	}
	defer opLock.Unlock()
	repoStartLock := m.startLockForRepo(repoID)
	if !repoStartLock.TryLock() {
		return
	}
	defer repoStartLock.Unlock()

	reason := session.NormalizeAttentionReason(hold.Reason())
	existing := instance.Attention()
	ours := existing != nil && !owned.IsZero() && existing.SetAt.Equal(owned)
	var setAt time.Time
	switch {
	case existing != nil && existing.Reason == reason:
		setAt = existing.SetAt
	case existing != nil && !ours:
		return
	default:
		// The row is free, or holds this path's request for an earlier hold
		// whose reason no longer describes the pane. SetAttention replaces it.
		previous, err := instance.SetAttention(reason, nowFunc())
		if err != nil {
			return // not a live, attention-eligible session; a later poll retries
		}
		data := instance.ToInstanceData()
		if err := persistInstanceData(repoID, data); err != nil {
			instance.RestoreAttention(previous)
			m.warn().Printf("session %q: could not persist the held-dialog attention flag: %v", instance.Title, err)
			return
		}
		m.publishEvent(agentproto.EventSessionUpdated, data)
		if raised := instance.Attention(); raised != nil {
			setAt = raised.SetAt
		}
	}

	m.dialogHoldMu.Lock()
	defer m.dialogHoldMu.Unlock()
	// Only if the poll has not moved on meanwhile: the same hold still due.
	if now := m.dialogHolds[key]; now != nil && now.due != nil && now.due.Since.Equal(hold.Since) {
		m.dialogHolds[key] = &dialogHoldEntry{since: hold.Since, setAt: setAt}
	}
}

// sweepDialogHolds drops entries whose session no longer exists. The poll is
// their only feed, so one it can no longer reach would never be removed.
// m.mu and dialogHoldMu are taken separately, never nested.
func (m *Manager) sweepDialogHolds() {
	m.dialogHoldMu.Lock()
	size := len(m.dialogHolds)
	m.dialogHoldMu.Unlock()
	if size == 0 {
		return
	}
	m.mu.Lock()
	live := make(map[string]struct{}, len(m.instances))
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		live[stableSessionKey(repoID, inst)] = struct{}{}
	}
	m.mu.Unlock()
	m.dialogHoldMu.Lock()
	defer m.dialogHoldMu.Unlock()
	for key := range m.dialogHolds {
		if _, ok := live[key]; !ok {
			delete(m.dialogHolds, key)
		}
	}
}
