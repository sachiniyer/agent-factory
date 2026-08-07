package daemon

import "github.com/sachiniyer/agent-factory/session"

// forgetSessionRuntimeStateLocked drops the per-session runtime state whose
// lifetime is the SESSION RECORD's, at the moment that record stops existing.
//
// The caller MUST hold m.mu, and MUST call this in the same critical section
// that removes the instance — the point of the exercise is that the entry and
// the record it describes disappear together, so no window exists in which one
// outlives the other.
//
// # Why here and not a sweep
//
// A sweep is a second structure that decides what exists, and it can disagree
// with the first. This daemon has already been bitten by exactly that: m.instances
// is not the universe (#2549), so a sweep keyed off it drops entries for sessions
// that are merely mid-provision, or keeps entries for sessions it cannot see.
// Deleting where the record is deleted has no such gap by construction — there is
// only one decision about whether the session exists, and both structures follow
// it (#3031).
//
// # Why not a cap or an LRU
//
// Evicting a LIVE session's observation to stay under a bound would make
// liveness reporting wrong for that session: aliveObservations is what the
// Lost-restore loop reads to decide whether a session was ever seen alive after
// spawn, so a missing entry reads as "never observed" and suppresses a legitimate
// restore. That trades a slow leak for a fabricated answer, which is worse.
func (m *Manager) forgetSessionRuntimeStateLocked(repoID string, instance *session.Instance) {
	if instance == nil {
		return
	}
	key := stableSessionKey(repoID, instance)
	// The reported leak: the only stableSessionKey-keyed map in the manager with
	// neither a delete nor a sweep, so an entry survived every session it ever
	// described (#3031).
	delete(m.aliveObservations, key)
	// The sibling with the same shape, found by the audit the issue asked for.
	// handoffRetryDue IS cleared — but only by ResumePendingHandoffs, which walks
	// m.instances. A session removed from that map is never walked again, so its
	// entry orphans exactly like aliveObservations. Clearing on settlement is the
	// common path; this is the one it cannot reach.
	delete(m.handoffRetryDue, key)
}
