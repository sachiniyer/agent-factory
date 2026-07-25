package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/sachiniyer/agent-factory/log"
)

// The forward-activation SupervisorOperations (#2212 R2a). They run in the
// previous-binary recovery actor, same as the rollback ops, and carry the same
// destructive authority: StopPrevious can swap a binary under a live daemon, and
// ValidateCandidate can bless the wrong process as the upgrade. Every op is
// therefore home-bound (recoveryHomeGuard) and fails closed. The candidate is
// launched with its transaction id so it enters upgrade probation AND skips the
// entrypoint gate (a normal unit/ad-hoc start would defer to this very actor —
// the forward analogue of the R1 rollback deadlock).

// awaitActivationGrace bounds how long AwaitActivation waits for the old daemon
// to publish its activation handshake before the supervisor's own
// ActivationAuthorized check runs. Package vars so tests can shorten the path.
var (
	awaitActivationGrace = 30 * time.Second
	awaitActivationPoll  = 100 * time.Millisecond
)

// adoptConfirmGrace bounds the re-poll that confirms the committed candidate is
// actually serving before the hand-off touches it. Like the rollback path's
// ValidatePrevious, this refuses to accept weak evidence: a single dial timeout on
// a loaded box (a saturated accept backlog, #2039) is NOT proof the candidate is
// gone, so we re-poll rather than conclude from one probe. Package vars so tests
// can shorten the path.
var (
	adoptConfirmGrace = 10 * time.Second
	adoptConfirmPoll  = 100 * time.Millisecond
)

// Indirection points so the forward ops can be driven without a real service
// manager, a spawned candidate, or a live control socket.
var (
	launchCandidateDaemonFn     = launchCandidateDaemon
	activationApprovedFn        = activationApproved
	releaseCandidateProbationFn = releaseCandidateProbation
	adoptAfterUpgradeCommitFn   = adoptAfterUpgradeCommit
	waitForDaemonReadyFn        = waitForDaemonReady
)

// committedCandidateState is the DEFINITE answer confirmCommittedCandidate
// re-polls for. It exists so the irreversible hand-off never acts on weak
// evidence: a dial timeout is not proof of absence, and a fork is not proof of a
// serving daemon.
type committedCandidateState int

const (
	candidateConfirmed   committedCandidateState = iota // our committed candidate is answering
	candidateForeign                                    // a live daemon answered, but it is not ours
	candidateAbsent                                     // a DEFINITE absence — ENOENT/ECONNREFUSED
	candidateUnconfirmed                                // no definite answer within the grace
)

// confirmCommittedCandidate re-polls the health probe for a DEFINITE answer about
// the committed candidate, mirroring the rollback path's ValidatePrevious rather
// than concluding from a single probe. A dial timeout — a saturated accept backlog
// on a loaded box (#2039) — is Undetermined (ClassifyShutdownTarget), never proof
// of absence, so it keeps polling; only ENOENT/ECONNREFUSED is a definite absence.
func confirmCommittedCandidate(expectedTransactionID string) committedCandidateState {
	deadline := time.Now().Add(adoptConfirmGrace)
	for {
		h := upgradeRecoveryHealthFn()
		if h.PingErr == nil {
			if h.TransactionID == expectedTransactionID {
				return candidateConfirmed
			}
			return candidateForeign
		}
		absent := false
		ClassifyShutdownTarget(h.PingErr).Match(
			func() {},                // yes: unreachable, PingErr != nil
			func() { absent = true }, // no: ENOENT/ECONNREFUSED — a definite absence
			func() {},                // not-found: this classifier never returns it
			func(error) {},           // undetermined: a timeout etc. — keep polling, do NOT conclude
		)
		if absent {
			return candidateAbsent
		}
		if !time.Now().Before(deadline) {
			return candidateUnconfirmed
		}
		time.Sleep(adoptConfirmPoll)
	}
}

// adoptAfterUpgradeCommit replaces the just-committed candidate with a fresh daemon
// that fully arms. The candidate ran ad-hoc through probation (Option A), so at
// commit it is BLOCKED in runDaemon's probation branch: it admits work and reports
// DaemonPhaseHandoffPending, but nothing ever signalled that select, so it never
// armed its scheduler, watchers, or session poll. Stopping it and starting a fresh
// daemon (which passes through the whole startup path) is what arms the loops. This
// is NOT unit-only: an ad-hoc home has no unit to defer to, so without this the
// parked candidate would be the permanent post-upgrade daemon and cron/watch/
// session-restore would silently never run.
//
// It MUST run only after Supervisor.Run returns and the journal is gone (the caller
// proves this), so a unit-started or ad-hoc daemon (runDaemon(cfg,"")) does not
// defer to a still-active transaction in the entrypoint gate.
//
// This is the LAST step of the IRREVERSIBLE path, so — unlike a best-effort probe —
// it holds itself to the rollback path's evidentiary discipline. It confirms the
// committed candidate is genuinely serving before touching it (a dial timeout is
// not proof of absence, so it re-polls); it stops a daemon only when that daemon IS
// our committed candidate, identified by expectedTransactionID which the candidate
// keeps for its whole boot (#1947); and it OBSERVES the fresh daemon become ready
// rather than trusting a fork. A state it could not confirm is a loud error the
// caller surfaces — never a silent success.
func adoptAfterUpgradeCommit(expectedTransactionID, canonicalExecPath string) error {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	switch confirmCommittedCandidate(expectedTransactionID) {
	case candidateForeign:
		// A live daemon that is not ours serves the home — never touch it. This is
		// also the idempotent re-entry case: a prior hand-off's replacement answers
		// with no transaction id.
		return nil
	case candidateUnconfirmed:
		// Best-effort at an irreversible boundary must be VISIBLE, not a silent exit 0.
		// We cannot stop (identity unconfirmed → the round-1 destructive-stop risk) or
		// start (the candidate may be alive → a singleton-lock collision), so surface it.
		return fmt.Errorf("could not confirm the committed candidate is serving this home within %s; not handing off blindly", adoptConfirmGrace)
	case candidateConfirmed:
		// Our parked candidate is up — stop it before replacing it.
		if _, err := stopDaemonFn(); err != nil {
			return fmt.Errorf("stop the committed candidate before handing it off: %w", err)
		}
		if err := waitForShutdownFn(); err != nil {
			return fmt.Errorf("the committed candidate did not release the control socket: %w", err)
		}
	case candidateAbsent:
		// The committed candidate is genuinely gone (it committed, then exited).
		// Nothing to stop, but a fresh daemon still must be started so the home is
		// not left daemonless after an irreversible commit.
		log.WarningLog.Printf("committed upgrade candidate is no longer serving this home; starting a fresh daemon")
	}
	// A resolve failure is unknown, not ad-hoc — but the daemon still must arm, and
	// an ad-hoc spawn always leaves one whether or not a unit exists, so a non-unit
	// answer falls through to it. Only a definite OwnerUnit takes the unit path.
	owner, _ := ResolveSupervisionOwner(configDir)
	return startArmedDaemonUnderOwner(owner, canonicalExecPath)
}

// startArmedDaemonUnderOwner starts a fresh daemon under the owner and OBSERVES
// that it becomes ready. A fork/exec return proves neither a bound socket nor a
// live daemon — startDaemonChild returns on cmd.Start(), and systemctl restart on a
// Type=simple unit returns once the main process is forked — so the fresh daemon
// could exit on any startup path (restore error, bind failure, a config the new
// binary can no longer load) and leave the home daemonless after an irreversible
// commit. The unit path falls back to an ad-hoc spawn if the unit fails to start OR
// to become ready, so a committed upgrade never ends with zero daemons (P2); a
// daemon that starts but never answers is a loud error the caller surfaces.
func startArmedDaemonUnderOwner(owner SupervisionOwner, canonicalExecPath string) error {
	if owner == OwnerUnit {
		if unitErr := startPreviousViaUnitFn(); unitErr != nil {
			log.WarningLog.Printf("upgrade committed but starting the installed unit failed; falling back to an ad-hoc daemon (run `af daemon adopt` to re-supervise): %v", unitErr)
		} else if readyErr := waitForDaemonReadyFn(time.Now().Add(daemonReadyTimeout)); readyErr != nil {
			log.WarningLog.Printf("the installed unit was started but its daemon did not become ready; falling back to an ad-hoc daemon (run `af daemon adopt`): %v", readyErr)
		} else {
			return nil // unit daemon is up and answering
		}
	}
	if err := startPreviousAdHocFn(canonicalExecPath); err != nil {
		return fmt.Errorf("start a daemon for the committed candidate: %w", err)
	}
	if err := waitForDaemonReadyFn(time.Now().Add(daemonReadyTimeout)); err != nil {
		return fmt.Errorf("started a daemon for the committed candidate but it did not become ready: %w", err)
	}
	return nil
}

// awaitOldDaemonActivation (AwaitActivation) waits for the old daemon's
// activation handshake to appear. It must NOT stop or quiesce that daemon —
// StopPrevious, a later phase, owns the stop. The supervisor re-checks the
// handshake against its own lease immediately after this returns, so this is a
// bounded wait for the approval to be published, not the authoritative check.
func awaitOldDaemonActivation(ctx context.Context, journal upgradetxn.Journal) error {
	deadline := time.Now().Add(awaitActivationGrace)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		approved, err := activationApprovedFn(journal)
		if err != nil {
			lastErr = err
		} else if approved {
			return nil
		} else {
			lastErr = fmt.Errorf("the old daemon has not published its activation approval")
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("upgrade activation handshake did not arrive within %s: %w", awaitActivationGrace, lastErr)
		}
		time.Sleep(awaitActivationPoll)
	}
}

// stopPreviousDaemon (StopPrevious) stops the OLD daemon under the captured owner
// and reports StopConfirmed only on proof the socket is gone. It shares the
// destructive-authority contract and home guard with the rollback stop: a
// fabricated confirmation here installs the candidate binary under a live old
// daemon.
func stopPreviousDaemon(ctx context.Context, journal upgradetxn.Journal) (upgradetxn.StopOutcome, error) {
	_ = ctx
	return stopDaemonForRecovery(journal, "previous")
}

// startCandidateDaemon (StartCandidate) launches the just-installed candidate
// binary in upgrade probation, carrying the transaction id. The id makes the new
// daemon (a) enter probation (rejecting mutating RPCs until ApproveCandidate) and
// (b) skip the entrypoint gate — a plain start would be deferred by this actor's
// own live transaction. Idempotent: the committed phase re-runs it, and a
// candidate already answering with this transaction id is left alone.
func startCandidateDaemon(ctx context.Context, journal upgradetxn.Journal) error {
	_ = ctx
	if err := recoveryHomeGuard(journal); err != nil {
		return err
	}
	if h := upgradeRecoveryHealthFn(); h.PingErr == nil && h.TransactionID == journal.ID {
		return nil // already the probation candidate for this transaction
	}
	if err := launchCandidateDaemonFn(journal.ExecutablePath, journal.ID); err != nil {
		return fmt.Errorf("launch upgrade candidate daemon %s: %w", journal.ExecutablePath, err)
	}
	return nil
}

// validateCandidateDaemon (ValidateCandidate) is the mirror of
// validatePreviousDaemon: it REQUIRES the responder carry this transaction's id
// (a probation candidate — the opposite polarity from the previous daemon) at the
// exact ToVersion with every recorded listener. Requiring the id is what keeps a
// from==to rebuild from validating a surviving previous daemon as the candidate.
func validateCandidateDaemon(ctx context.Context, journal upgradetxn.Journal) error {
	return awaitDaemonIdentity(ctx, journal, daemonIdentity{
		role:                 "candidate",
		version:              journal.ToVersion,
		requireTransactionID: journal.ID,
		listeners:            journal.Daemon.Listeners,
	})
}

// approveCandidateDaemon (ApproveCandidate) releases the validated candidate from
// probation so it admits ordinary daemon work. It is only reachable after Commit,
// which is the irreversible verdict. Supervision is handed to the installed unit
// separately, after the transaction's journal is removed, so the unit-started
// daemon does not defer to a still-active journal (see adoptAfterUpgradeCommit).
func approveCandidateDaemon(ctx context.Context, journal upgradetxn.Journal) error {
	_ = ctx
	if err := recoveryHomeGuard(journal); err != nil {
		return err
	}
	if err := releaseCandidateProbationFn(journal.ID); err != nil {
		return fmt.Errorf("release upgrade candidate from probation: %w", err)
	}
	return nil
}

// launchCandidateDaemon spawns the just-installed candidate binary in probation,
// carrying the transaction id so it enters DaemonPhaseUpgradeProbation and skips
// the entrypoint gate. Ad-hoc during probation — the recovery actor supervises
// it; the installed unit re-adopts it after commit (adoptAfterUpgradeCommit).
func launchCandidateDaemon(execPath, transactionID string) error {
	pid, err := startDaemonChild(execPath, "--upgrade-transaction", transactionID)
	if err != nil {
		return err
	}
	log.InfoLog.Printf("started upgrade candidate daemon child PID %d (transaction %s)", pid, transactionID)
	return nil
}

func activationApproved(journal upgradetxn.Journal) (bool, error) {
	return upgradetxn.ActivationApproved(journal.HomeDir)
}

// releaseCandidateProbation dials the candidate's control socket (the actor's
// home is bound to the transaction's, so this is the candidate) and asks it to
// leave probation. No-ensure: the candidate is already running; we must not spawn.
func releaseCandidateProbation(transactionID string) error {
	var resp ReleaseUpgradeProbationResponse
	if err := callDaemonNoEnsure("ReleaseUpgradeProbation",
		ReleaseUpgradeProbationRequest{TransactionID: transactionID}, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("daemon did not confirm upgrade probation release")
	}
	return nil
}
