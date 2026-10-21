package daemon

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
)

// The confirmed-flap bound (#5238). consecutiveFailures only counts restore
// attempts that FAIL, so a runtime that is confirmed alive and then goes lost
// again retires its episode debt-free: a session whose agent survives the
// confirmation window and exits right after — the codex startup update-dialog
// death is the observed shape — loops lost→restored→lost forever and the six-
// attempt budget can never accumulate. Four CONFIRMED restores inside the
// window is the same terminal verdict the structural budget produces — a
// durable LostRestoreFailure, an ERROR line, and release of the watch-task
// slot — with a flap-specific reason and the operator's explicit off-ramp.
const lostRestoreFlapMaxRestores = 4

// lostRestoreFlapWindow bounds how far back confirmed restores count toward
// the flap bound. A var for the same reason the backoff knobs are: tests
// shrink it.
var lostRestoreFlapWindow = 30 * time.Minute

// noteLostRestoreConfirmationLocked records one CONFIRMED automatic
// lost-session restore in the session's flap history, pruning entries the
// window has already expired. Caller holds m.mu.
func (m *Manager) noteLostRestoreConfirmationLocked(stateKey string, now time.Time) {
	times := pruneLostRestoreConfirmations(m.lostRestoreConfirmations[stateKey], now)
	m.lostRestoreConfirmations[stateKey] = append(times, now)
}

// lostRestoreConfirmationCountLocked reports how many confirmed automatic
// restores the session has inside the flap window, pruning the expired edge as
// it counts. Caller holds m.mu.
func (m *Manager) lostRestoreConfirmationCountLocked(stateKey string, now time.Time) int {
	times := pruneLostRestoreConfirmations(m.lostRestoreConfirmations[stateKey], now)
	if len(times) == 0 {
		delete(m.lostRestoreConfirmations, stateKey)
		return 0
	}
	m.lostRestoreConfirmations[stateKey] = times
	return len(times)
}

// pruneLostRestoreConfirmations drops confirmations older than the flap
// window. Entries append in poll order, so the slice is chronological and only
// the leading edge can expire.
func pruneLostRestoreConfirmations(times []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-lostRestoreFlapWindow)
	drop := 0
	for drop < len(times) && !times[drop].After(cutoff) {
		drop++
	}
	return times[drop:]
}

// lostRestoreFlapGiveUpIfBound commits the confirmed-flap terminal verdict and
// reports true when the recorded confirmations already fill the window. Every
// site about to take irreversible work calls it — the automatic respawn, and
// the probeAnsweredDead preserve-and-reap arm, which can otherwise burn
// preserve-push retries forever before the bound is ever reached (#5238
// review).
func (m *Manager) lostRestoreFlapGiveUpIfBound(key, repoID, stateKey string, inst *session.Instance) bool {
	m.mu.Lock()
	n := m.lostRestoreConfirmationCountLocked(stateKey, nowFunc())
	m.mu.Unlock()
	if n < lostRestoreFlapMaxRestores {
		return false
	}
	m.lostRestoreFlapGiveUp(key, repoID, inst, n)
	return true
}

// lostRestoreFlapGiveUp commits the confirmed-flap terminal verdict (#5238):
// the same durable LostRestoreFailure the structural budget produces — the
// entry gate's LostRestoreGaveUp check then stops every later automatic pass —
// with the flap-specific reason and the operator's explicit way back. The
// in-memory flap history is kept: it IS the evidence, and nothing reads it
// once the durable flag suppresses further restores anyway.
func (m *Manager) lostRestoreFlapGiveUp(key, repoID string, inst *session.Instance, confirmedRestores int) {
	hint := shellsuggest.Command("af", "sessions", "restore", inst.Title)
	flapErr := fmt.Errorf("the session was confirmed restored %d times in %s and went lost again each time — automatic restore is stopped; find out why the agent exits soon after launch, then bring it back with %s",
		confirmedRestores, lostRestoreFlapWindow, hint)
	inst.ClearLostRestoreWait()
	inst.SetLostRestoreFailure(confirmedRestores, flapErr)
	m.err().Printf("restore of lost session %q (repo %s): giving up after %d confirmed restores in %s: %v",
		inst.Title, repoID, confirmedRestores, lostRestoreFlapWindow, flapErr)
	if persistErr := m.persistSettlement(repoID, key, inst); persistErr != nil {
		m.warn().Printf("restore of lost session %q gave up in memory but its terminal state is not yet durable: %v", inst.Title, persistErr)
	}
}
