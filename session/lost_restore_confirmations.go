package session

import "time"

// The lostRestoreConfirmations instance field is the durable copy of the
// daemon's confirmed-flap history: the rolling timestamps of automatic
// Lost-session restores whose replacement runtime then answered enough
// liveness polls to count as a real recovery (#5238). The flap bound counts
// how many of them fall inside lostRestoreFlapWindow, so the history is the
// bound's evidence.
//
// It lives on the session record for the same reason LostRestoreWait and
// LostRestoreFailure do: a daemon restart or upgrade drops the in-memory map,
// and a bound that forgets mid-window lets a flapping session buy extra cycles
// with every restart (#5436). The record holds timestamps rather than a count
// because window membership is wall-clock — a restart must be able to expire
// entries, not just remember them.
//
// omitempty + additive like the neighbouring fields: a record written by an
// older daemon decodes to nil and simply behaves as master's bound did —
// in-memory only for the remainder of the episode.

// SetLostRestoreConfirmations commits the daemon's rolling confirmed-restore
// history to the live session record. The caller passes the ALREADY-PRUNED
// in-window list it just appended to — the record mirrors the map, it does
// not prune itself, so window expiry is applied at write and at load. The
// daemon owns the disk write after this in-memory edge.
func (i *Instance) SetLostRestoreConfirmations(times []time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if timesEqual(i.lostRestoreConfirmations, times) {
		return false
	}
	i.lostRestoreConfirmations = append([]time.Time(nil), times...)
	i.touchLocked()
	return true
}

// ClearLostRestoreConfirmations retires the durable history with the episode:
// an explicit restore off-ramp, a tombstone, or a load that lands the row
// somewhere no automatic restore can reach.
func (i *Instance) ClearLostRestoreConfirmations() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.lostRestoreConfirmations) == 0 {
		return false
	}
	i.lostRestoreConfirmations = nil
	i.touchLocked()
	return true
}

// LostRestoreConfirmationsSnapshot returns a detached copy of the durable
// history — the daemon's restart seeding path is the only reader.
func (i *Instance) LostRestoreConfirmationsSnapshot() []time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return append([]time.Time(nil), i.lostRestoreConfirmations...)
}

func timesEqual(a, b []time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}
