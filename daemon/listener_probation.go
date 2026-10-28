package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/sachiniyer/agent-factory/log"
)

// Upgrade-probation listener carry (#5183): the one case where the #5137
// refusal is deferred. An upgrade candidate loading a refused file would
// otherwise refuse a TCP listener the previous daemon had bound, fail the
// previous binary's journaled TCPBound validation, and roll back to that old
// daemon still serving the exposed posture — on every retry. The candidate
// keeps the journaled socket bound instead, under a token floor: the journal
// records no auth posture, so "preserving an exposed socket" and "creating
// one" are indistinguishable, and the safe answer to both is bound AND gated.
//
// The floor lives in Manager.live beside the config (livePosturePublication)
// and is armed/disarmed here; the gate overlays it per request
// (livePosture.current → withProbationTokenFloor).

// loadUpgradeJournalFn reads the active upgrade transaction journal for a home.
// Seam for tests, in the same shape as the upgrade_* indirections in
// upgrade_recovery.go — a real journal write needs the flock the test does not
// hold.
var loadUpgradeJournalFn = func(homeDir string) (upgradetxn.Journal, error) {
	txn, err := upgradetxn.Load(homeDir)
	if err != nil {
		return upgradetxn.Journal{}, err
	}
	return txn.Journal(), nil
}

// upgradeCandidateCarriesRefusedListener reports whether THIS daemon is an
// upgrade candidate whose transaction journal recorded the previous daemon's
// TCP listener bound — the one case where reconcile must NOT apply the #5137
// refusal. The reason is on the wire, not in the code: the supervisor
// validating a candidate is the PREVIOUS binary's recovery actor
// (internal/upgradetxn/recovery_actor.go execs the preserved old binary), so a
// daemon upgrading FROM a pre-#5137 build is validated by a
// daemonMatchesIdentity that has no TCPRefusalReason and requires TCPBound
// whenever the journal recorded it. A candidate that refused would fail that
// check and roll the upgrade back to the exposed old daemon — the worst
// available outcome.
//
// The deferral is deliberately narrow:
//
//   - It requires this daemon to BE the candidate (lifecycle transactionID
//     matching the journal's ID) — a plain daemon or a stale/foreign journal
//     never qualifies.
//   - It requires the journal to have expected the socket bound — a probation
//     daemon never gets to BIND a fresh exposure the old daemon was not already
//     serving.
//   - It is bound but NOT unauthenticated: the journal records TCPBound only —
//     no auth posture — so a candidate loading a refused file cannot tell
//     "the old daemon was already exposed" from "the old daemon was tokened
//     and the file was hand-edited". The probation token floor makes the answer
//     safe either way: the deferred socket demands the bearer token for the
//     whole window.
//   - It ends with the journal: after Cleanup the transaction no longer loads,
//     so a late reconcile refuses normally. The parked candidate is stopped and
//     replaced by a fresh ordinary daemon at adoption regardless — its startup
//     reconcile enforces the refusal for real.
//
// No reconcile runs at release, by design: a supervisor re-entering
// PhaseCommitted after a crash re-runs StartCandidate+ValidateCandidate before
// ApproveCandidate, so retiring the socket at release would fail a validation
// that still expects bound.
func (wl *webListeners) upgradeCandidateCarriesRefusedListener() bool {
	m := wl.manager
	if m == nil || m.lifecycle == nil {
		return false
	}
	transactionID := m.lifecycle.snapshot().transactionID
	if transactionID == "" {
		return false
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return false
	}
	journal, err := loadUpgradeJournalFn(home)
	if err != nil {
		return false
	}
	return journal.ID == transactionID && journal.Daemon.Listeners.TCPBound
}

// servingUnauthenticatedLocked reports whether anything this daemon could keep
// or start SERVING under newCfg is the unauthenticated-network exposure —
// either the configured address (a reconcile may (re)bind it) or a socket
// already bound, whose real address a failed rebind can leave diverged from the
// file. Deliberately blind to network.allow_unauthenticated_network: the
// upgrade journal records bound state only, so "the previous daemon was already
// serving it open on purpose" is unknowable — and the probation floor must
// never extend an unauthenticated socket on a guess. Caller holds wl.mu.
func (wl *webListeners) servingUnauthenticatedLocked(newCfg *config.Config) bool {
	if config.ListenerServesUnauthenticatedNetwork(newCfg.ListenAddr, newCfg.RequireToken) {
		return true
	}
	addr := wl.webBoundAddr
	if addr == "" {
		addr = wl.webConfigAddr
	}
	return config.ListenerServesUnauthenticatedNetwork(addr, newCfg.RequireToken)
}

// probationPreSwapLocked is retireWebBeforePostureSwap's probation half. It
// reads the journal ONCE for the whole apply and hands the decision to the
// reconcile that follows (applyCarryRefusedSocket): a journal removal landing
// between two independent reads would have this phase keep and floor the
// socket while reconcile decides the candidate carries nothing and clears the
// floor under it.
//
// Under a carry nothing retires — the previous-binary supervisor can still
// re-validate the candidate against the journaled bound listener until the
// journal is gone. But bound must never become unauthenticated on the way
// through: a socket answering on a network address floors to the bearer token
// for the window rather than serving the incoming tokenless posture. ARM only —
// the live config still holds the OLD posture until ApplyConfig publishes
// newCfg, so a request landing between a premature clear and that swap would
// be served unauthenticated. Reconcile disarms after the publish and the
// socket settle, where the swapped posture already decides on its own.
// Caller holds wl.mu.
func (wl *webListeners) probationPreSwapLocked(newCfg *config.Config) (carries bool) {
	carries = wl.upgradeCandidateCarriesRefusedListener()
	wl.applyCarryRefusedSocket = &carries
	if carries && wl.servingUnauthenticatedLocked(newCfg) {
		wl.manager.setTokenFloor(true)
	}
	return carries
}

// probationReconcileLocked is reconcile's probation half. It consumes this
// apply's carry decision — or, for the startup reconcile that no pre-swap ran
// before, reads the journal itself — arms the floor BEFORE any bind or retire
// below it, and returns the refusal reconcile should enforce ("" while the
// carry defers it) plus whether the floor must stay armed once the socket
// settles.
//
// The floor rides carries, not just the deferral: while the journal window is
// open this candidate must never let a kept-bound socket serve
// unauthenticated — including a retained socket whose real bound address
// diverged from the file, and including the deferral case where (re)binding
// the refused address is exactly what keeps the supervisor's TCPBound check
// green. A deferred bind must never answer unfloored, which is why the arm
// happens here and not after the bind. Caller holds wl.mu.
func (wl *webListeners) probationReconcileLocked(newCfg *config.Config, refusal string) (string, bool) {
	var carries bool
	if wl.applyCarryRefusedSocket != nil {
		carries = *wl.applyCarryRefusedSocket
		wl.applyCarryRefusedSocket = nil
	} else {
		carries = wl.upgradeCandidateCarriesRefusedListener()
	}
	floor := carries && wl.servingUnauthenticatedLocked(newCfg)
	if floor {
		wl.manager.setTokenFloor(true)
	}
	if refusal == "" || !carries {
		return refusal, floor
	}
	if wl.webConfigAddr != newCfg.ListenAddr || wl.webHandle == nil {
		log.WarningLog.Printf("upgrade candidate is keeping network.listen_addr %q bound for "+
			"the duration of this upgrade: the previous-binary supervisor validates the candidate against the "+
			"journaled bound listener and cannot see a refusal. The socket demands the bearer token while the "+
			"deferral holds (the journal records no auth posture to preserve), and the post-upgrade daemon "+
			"will refuse the bind — %s", newCfg.ListenAddr, refusal)
	}
	return "", floor
}

// probationBannerLocked corrects bindWebLocked's one-time banner inputs for
// the floor: while it is armed the bind serves WITH the token no matter what
// the (refused) config says, so the exposure notice and the "NO token" line
// would both be lies about what is bound. Enforcement is unaffected — the gate
// reads the floor per request. Caller holds wl.mu.
func (wl *webListeners) probationBannerLocked(policy tokenGatePolicy, notice string) (tokenGatePolicy, string) {
	if !wl.manager.tokenFloorArmed() {
		return policy, notice
	}
	policy.tokenDisabled = false
	return policy, ""
}
