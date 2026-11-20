package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// The poll's part in surfacing a launch dialog the trust gate holds (#7635).
//
// What is durable lives on the session and is persisted with its row:
//   - the attention request carries Origin = session.AttentionOriginDialogHold.
//     That marker is the whole of "this path raised it". It is the only thing
//     recovery, adoption and clearing look at; the request's text is never
//     read to decide ownership.
//   - DialogHoldFlagged is the key of the held pane the session was last
//     flagged for. A hold is flagged once per key, so a dismissed notice is
//     not raised again for the same pane — in this daemon's life or the next.
//
// What is in memory here is what one poll owes the next, and the bound that
// makes a peer churning hold keys cost one write per interval instead of one
// per poll (#7648). Both are per runtime: a replacement starts a fresh
// budget, the way a dismissal belongs to the pane it was made on.

// dialogHoldEntry is that memory for one session. Guarded by
// Manager.dialogHoldMu.
type dialogHoldEntry struct {
	// due is a hold observed and not yet flagged; nil when none.
	due *session.DialogHold
	// resolveDue says the hold is over and the session still carries its
	// marks: this path's request, the flagged key, or both.
	resolveDue bool
	// seenHold says this daemon has observed a hold on this session. Until
	// then a no-hold observation is not proof the hold is over: see
	// dialogHoldRestartGrace.
	seenHold bool
	// quietSince is the first no-hold observation of a session that carries
	// marks this daemon never saw a hold for. Zero otherwise.
	quietSince time.Time
	// raisedAt is when this daemon last completed a raise for this session:
	// the bound on raises is one per dialogHoldRaiseInterval, whatever keys
	// a peer sends.
	raisedAt time.Time
	// dismissedAt is when a flagged session was first seen without this
	// path's request on the row — the notice was dismissed, or replaced by
	// someone else's. A hold under another key does not raise until
	// dialogHoldDismissalHold has passed from then.
	dismissedAt time.Time
	// observed is the runtime whose observation left due or resolveDue owed.
	// What one pane's observation owes is never paid on another: a flush that
	// finds the runtime replaced meanwhile drops it.
	observed session.AgentObservationGeneration
}

// dialogHoldRestartGrace is how long a session's marks stand, with no hold
// observed, in a daemon that has not seen the hold itself. That is a daemon
// just restarted: the request and the flagged key came back with the row, and
// a local gate's recorder starts its settle clock over, so a pane that is
// still held reports no hold for about that long. Resolving at once would
// blink the flag at every restart and forget a dismissal. Three settle times
// covers the recorder and a slow first poll.
const dialogHoldRestartGrace = 30 * time.Second

// dialogHoldRaiseInterval bounds raises for one session to at most one per
// settle time, whatever keys a peer sends (#7648): a sandbox whose agent
// server reports a new, validly shaped hold key on every poll costs one row
// persist and one publish per interval, not per poll.
const dialogHoldRaiseInterval = 10 * time.Second

// dialogHoldDismissalHold is how long a dismissal suppresses raises for keys
// the session is not flagged for: three settle times, the margin
// dialogHoldRestartGrace takes. A dismissal already sticks for the same key
// for the life of the held pane; this keeps a peer that mints a new key per
// poll — or flaps its hold off and back on to take the flag down — from
// talking around it.
const dialogHoldDismissalHold = 3 * dialogHoldRaiseInterval

// suppresses reports whether the entry's bounds still hold at now: a raise
// inside the raise interval, or a dismissal inside its hold. The same check
// keeps an entry remembered until the bound lapses — dropping it the moment
// the marks come down would let a flapping peer spend it again at once.
func (e dialogHoldEntry) suppresses(now time.Time) bool {
	return (!e.raisedAt.IsZero() && now.Sub(e.raisedAt) < dialogHoldRaiseInterval) ||
		(!e.dismissedAt.IsZero() && now.Sub(e.dismissedAt) < dialogHoldDismissalHold)
}

// noteDismissal applies what the observation's marks say about a dismissal. A
// flagged key with none of this path's request on the row means the notice
// was dismissed or replaced by someone else's: the hold it earns runs from
// when that was first seen, so a restarted daemon or a peer that re-asks
// every poll cannot roll the window. A live request is no dismissal — and a
// raise that lands after one ends its bound, so a later dismissal's interval
// is not shortened by it.
func (e dialogHoldEntry) noteDismissal(flagged string, owned bool, now time.Time) dialogHoldEntry {
	if flagged == "" {
		return e // a bound outlives the marks it was earned on
	}
	if !owned {
		if e.dismissedAt.IsZero() {
			e.dismissedAt = now
		}
		return e
	}
	e.dismissedAt = time.Time{}
	return e
}

// dialogHoldStep is the poll's whole decision about one observation, as a pure
// function. entry is what it remembered (the zero value for nothing), hold the
// observation's normalized hold or nil, flagged and owned the session's
// durable marks: the key it is flagged for, and whether this path's request is
// on the row. It returns the entry to keep, and keep=false to forget the
// session.
func dialogHoldStep(entry dialogHoldEntry, hold *session.DialogHold, flagged string, owned bool, now time.Time) (next dialogHoldEntry, keep bool) {
	if hold != nil {
		next = dialogHoldEntry{
			seenHold: true, raisedAt: entry.raisedAt, dismissedAt: entry.dismissedAt,
		}.noteDismissal(flagged, owned, now)
		if flagged == hold.Key {
			// Flagged for this very pane already: raised, or raised and dismissed.
			return next, true
		}
		if next.suppresses(now) {
			return next, true // bounded: too soon after a raise or a dismissal
		}
		due := *hold
		next.due = &due
		return next, true
	}
	if flagged == "" && !owned {
		if entry.suppresses(now) {
			// The hold and its marks are gone, but a bound this session
			// earned still runs: keep it until it lapses, or a peer that
			// flaps the hold off and back on spends it again each poll.
			return dialogHoldEntry{seenHold: entry.seenHold, raisedAt: entry.raisedAt, dismissedAt: entry.dismissedAt}, true
		}
		return dialogHoldEntry{}, false // no hold, no marks: nothing to do
	}
	// No hold, and marks to take down — once that is known to be true.
	next = dialogHoldEntry{
		seenHold:    entry.seenHold,
		quietSince:  entry.quietSince,
		raisedAt:    entry.raisedAt,
		dismissedAt: entry.dismissedAt,
	}.noteDismissal(flagged, owned, now)
	if !next.seenHold {
		if next.quietSince.IsZero() {
			next.quietSince = now
		}
		if now.Sub(next.quietSince) < dialogHoldRestartGrace {
			return next, true
		}
	}
	next.resolveDue = true
	return next, true
}

// noteDialogHold feeds one successful observation to the tracker. It only
// records; every write happens in flushDialogHold, after the observation
// fence releases, the way the staged-draft flag is written. The hold is
// normalized here whatever its source, so a zero since, a malformed key or an
// unknown kind never reaches a decision.
//
// generation names the runtime the observation came from. It is kept with
// whatever the observation leaves owed, so the flush can refuse to pay it on a
// runtime that replaced this one.
func (m *Manager) noteDialogHold(key string, instance *session.Instance, generation session.AgentObservationGeneration, observed *session.DialogHold) {
	hold := session.NormalizedDialogHold(observed)
	flagged, ownedSetAt := instance.DialogHoldFlagged()
	m.dialogHoldMu.Lock()
	defer m.dialogHoldMu.Unlock()
	entry, remembered := m.dialogHolds[key]
	if hold == nil && !remembered && flagged == "" && ownedSetAt.IsZero() {
		return // the common case, without touching the map
	}
	var prior dialogHoldEntry
	if entry != nil {
		prior = *entry
		if prior.observed != generation {
			// The runtime these bounds were earned on was replaced: a
			// dismissal belongs to the pane it was made on, and a raise on a
			// retired pane does not spend the replacement's interval.
			prior.raisedAt, prior.dismissedAt = time.Time{}, time.Time{}
		}
	}
	next, keep := dialogHoldStep(prior, hold, flagged, !ownedSetAt.IsZero(), nowFunc())
	if !keep {
		delete(m.dialogHolds, key)
		return
	}
	if m.dialogHolds == nil {
		m.dialogHolds = make(map[string]*dialogHoldEntry)
	}
	next.observed = generation
	m.dialogHolds[key] = &next
}

// flushDialogHold does what noteDialogHold recorded as owed: flag the session
// for a hold, or take its marks down once the hold is over. It reuses the
// attention signal and its write path: the opLock→repoStartLock order, TryLock
// on both so the serial poll never parks behind a create or a handoff, then
// one persist and one publish. On contention what is owed stays owed and the
// next poll retries. Nothing here is asynchronous, so no clear can land after
// a raise.
func (m *Manager) flushDialogHold(repoID, key string, instance *session.Instance) {
	m.dialogHoldMu.Lock()
	entry := m.dialogHolds[key]
	if entry == nil || (entry.due == nil && !entry.resolveDue) {
		m.dialogHoldMu.Unlock()
		return
	}
	var hold *session.DialogHold
	if entry.due != nil {
		due := *entry.due
		hold = &due
	}
	observed := entry.observed
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

	// Inside the attention locks, which a handoff and every other runtime
	// replacement also take: is this still the runtime that was observed? If
	// not, what its pane owed is void. The flush also runs on polls that noted
	// nothing — a failed snapshot returns before the note — so without this a
	// hold left due by lock contention would be raised on the replacement,
	// for a dialog on a pane that no longer exists.
	stale := !instance.AgentObservationCurrent(observed)
	done := stale
	if !stale {
		if hold != nil {
			done = m.raiseDialogHoldLocked(repoID, instance, *hold)
		} else {
			done = m.resolveDialogHoldLocked(repoID, instance)
		}
	}
	if !done {
		return
	}
	m.dialogHoldMu.Lock()
	defer m.dialogHoldMu.Unlock()
	now := m.dialogHolds[key]
	if now == nil || now.observed != observed {
		return // a newer observation has replaced what this flush read
	}
	if stale {
		delete(m.dialogHolds, key) // the next observation of the new runtime starts over
		return
	}
	// Settle only what this flush did; the poll may have moved on meanwhile.
	if hold != nil {
		// A raise that lands ends the bound the dismissal before it earned:
		// the next dismissal's hold runs its own full interval from when it
		// is first seen, not the scraps of this one.
		now.raisedAt, now.dismissedAt = nowFunc(), time.Time{}
		if now.due != nil && now.due.Key == hold.Key {
			now.due = nil
		}
	} else if now.resolveDue {
		now.due, now.resolveDue = nil, false
		if !now.suppresses(nowFunc()) {
			delete(m.dialogHolds, key)
		}
	}
}

// raiseDialogHoldLocked flags the session for hold. Caller holds the op lock
// and the repo start lock. It reports whether the hold is dealt with; false
// leaves it due.
//
// What is on the row decides the write:
//   - nothing: raise the notice, marked as this path's.
//   - this path's own request, by its Origin marker, from an earlier hold or
//     an earlier daemon: keep it when its words still describe the pane,
//     replace it when they do not.
//   - any other request — the agent's own question, a staged-draft flag, a
//     request whose text happens to read like a notice: leave it. The hold
//     stays due and the notice goes up once the row is free.
func (m *Manager) raiseDialogHoldLocked(repoID string, instance *session.Instance, hold session.DialogHold) bool {
	if flagged, _ := instance.DialogHoldFlagged(); flagged == hold.Key {
		return true
	}
	reason := session.NormalizeAttentionReason(hold.Reason())
	existing := instance.Attention()
	ours := existing != nil && existing.Origin == session.AttentionOriginDialogHold
	if existing != nil && !ours {
		return false
	}
	var undo session.AttentionUndo
	wrote := false
	if !ours || existing.Reason != reason {
		var err error
		undo, err = instance.SetOwnedAttention(reason, session.AttentionOriginDialogHold, nowFunc())
		if err != nil {
			return false // not a live, attention-eligible session; a later poll retries
		}
		wrote = true
	}
	previousKey := instance.SetDialogHoldFlagged(hold.Key)
	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		if wrote {
			instance.RestoreAttention(undo)
		}
		instance.SetDialogHoldFlagged(previousKey)
		m.warn().Printf("session %q: could not persist the held-dialog attention flag: %v", instance.Title, err)
		return false
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
	return true
}

// resolveDialogHoldLocked takes the session's marks down once its hold is
// over: this path's request, if it is still the one on the row, and the
// flagged key. Caller holds the op lock and the repo start lock. A request
// without this path's Origin marker is never cleared, whatever it says.
func (m *Manager) resolveDialogHoldLocked(repoID string, instance *session.Instance) bool {
	flagged, ownedSetAt := instance.DialogHoldFlagged()
	if flagged == "" && ownedSetAt.IsZero() {
		return true
	}
	var undo session.AttentionUndo
	cleared := false
	if !ownedSetAt.IsZero() {
		undo, cleared = instance.ClearAttention(ownedSetAt)
	}
	instance.SetDialogHoldFlagged("")
	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		if cleared {
			instance.RestoreAttention(undo)
		}
		instance.SetDialogHoldFlagged(flagged)
		m.warn().Printf("session %q: could not clear the held-dialog attention flag: %v", instance.Title, err)
		return false
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
	return true
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
