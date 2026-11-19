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
	// setAt is the attention request this path raised for that hold, the only
	// one it may clear. Zero when it raised none.
	setAt time.Time
}

// dialogHoldStep is the poll's whole decision about one observation, as a pure
// function of what it remembered and what it saw. hold is the observation's
// settled hold, or nil. It returns the entry to keep (nil to forget the
// session) and, when clear is true, the attention request to clear: the one
// this path raised for a hold that has since resolved or changed.
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
	// A hold not flagged yet. If the entry still carries a request raised for
	// an earlier hold, that hold is over: clear its request, and flag this one.
	due := *hold
	next = &dialogHoldEntry{due: &due}
	if entry != nil && !entry.setAt.IsZero() {
		return next, entry.setAt, true
	}
	return next, time.Time{}, false
}

// noteDialogHold feeds one successful observation to the hold tracker. It only
// records: the attention write happens in raiseDueDialogHold, after the
// observation fence releases, the way the staged-draft flag is written. The
// clear it may queue runs on its own goroutine and is keyed by SetAt, so it
// removes nothing but the request this path raised.
func (m *Manager) noteDialogHold(key string, instance *session.Instance, hold *session.DialogHold) {
	m.dialogHoldMu.Lock()
	entry := m.dialogHolds[key]
	if entry == nil && hold == nil {
		m.dialogHoldMu.Unlock()
		return // the common case: no hold, nothing remembered
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

// raiseDueDialogHold flags the session for a hold noteDialogHold recorded as
// due. It reuses the attention signal and its write path unchanged: the
// opLock→repoStartLock order, TryLock on both so the serial poll never parks
// behind a create or a handoff, then persist and publish. On contention the
// due mark stays and the next poll retries.
//
// A request already on the row is left alone. If it is this same notice — the
// daemon restarted while the pane stayed held — the tracker adopts it, so it
// can clear it when the hold resolves. Any other request outranks the notice:
// the due mark stays, and the notice goes up once the row is free.
func (m *Manager) raiseDueDialogHold(repoID, key string, instance *session.Instance) {
	m.dialogHoldMu.Lock()
	entry := m.dialogHolds[key]
	if entry == nil || entry.due == nil {
		m.dialogHoldMu.Unlock()
		return
	}
	hold := *entry.due
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
	var setAt time.Time
	if existing := instance.Attention(); existing != nil {
		if existing.Reason != reason {
			return
		}
		setAt = existing.SetAt
	} else {
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
