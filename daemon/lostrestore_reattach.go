package daemon

import (
	"errors"

	"github.com/sachiniyer/agent-factory/session"
)

// reattachLostRemote tries the W1.5 reattach for a Lost ssh/sandbox row whose
// liveness probe was inconclusive, and settles the row if the agent-server
// answers. It reports whether the row was healed. A failure leaves the row
// exactly as it was: still Lost, still recoverable, and never replaced on the
// strength of the failure (#5571 W1.6). The caller holds the session's op lock.
func (m *Manager) reattachLostRemote(repoID, key, stateKey string, inst *session.Instance) bool {
	err := inst.ReattachSandbox()
	switch {
	case err == nil:
		m.info().Printf("lost remote session %q (repo %s): its agent-server answered a reattach, so the row was healed without re-provisioning", inst.Title, repoID)
		m.healLostRemoteRestored(repoID, key, stateKey, inst)
		return true
	case errors.Is(err, session.ErrReattachIneligible):
		return false
	default:
		m.warn().Printf("lost remote session %q: reattach to its runtime did not succeed: %v", inst.Title, err)
		return false
	}
}

// healLostRemoteRestored settles a Lost remote row whose runtime was proven live
// without re-provisioning it: the probe answered alive, or the W1.5 reattach
// re-bound its agent-server (#5571 W1.6).
func (m *Manager) healLostRemoteRestored(repoID, key, stateKey string, inst *session.Instance) {
	_ = inst.Transition(session.ObserveLiveness(session.LiveRunning))
	inst.ClearLostRestoreFailure()
	inst.ClearLostRestoreWait()
	// The answered probe is transport evidence: the outage flag retires
	// with the mark it was explaining (#5571 W1.3).
	inst.ClearUnreachable()
	// Clear before the persist, same ordering rule as the recovery path below:
	// the answered probe already ended the loss episode, and leaving the stale
	// count in place across a disk write lets a blip in that window re-mark the
	// very session we just proved alive.
	m.clearRemoteLoss(stableSessionKey(repoID, inst))
	m.mu.Lock()
	delete(m.lostRestoreStates, stateKey)
	// Deliberately NOT clearing lostRestoreConfirmations here: the flap
	// history is the bound's only memory of prior restore-die cycles, and a
	// lucky blip-heal is not an operator intervention — clearing on an
	// automatic heal would let a flapping session reset its own bound
	// (#5238). The manual restore.go path is the off-ramp that clears it.
	m.mu.Unlock()
	if err := m.persistSettlement(repoID, key, inst); err != nil {
		m.warn().Printf("restore of lost session %q was confirmed alive but its healed state is not yet durable: %v", inst.Title, err)
	}
}
