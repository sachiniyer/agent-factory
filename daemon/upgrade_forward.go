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

// Indirection points so the forward ops can be driven without a real service
// manager, a spawned candidate, or a live control socket.
var (
	launchCandidateDaemonFn     = launchCandidateDaemon
	activationApprovedFn        = activationApproved
	releaseCandidateProbationFn = releaseCandidateProbation
	adoptAfterUpgradeCommitFn   = adoptAfterUpgradeCommit
)

// adoptAfterUpgradeCommit replaces the just-committed candidate with a fresh
// daemon that fully arms in place. The candidate ran ad-hoc through probation
// (Option A), so at commit it is BLOCKED in runDaemon's probation branch: Ping and
// `af daemon status` report it ready (releaseUpgradeProbation opened admission),
// but nothing ever signalled that select, so it never armed its scheduler,
// watchers, or session poll — a zombie that reports success. Stopping it and
// starting a fresh daemon (which passes through the whole startup path) is what
// arms the loops. This is NOT unit-only: an ad-hoc home has no unit to defer to,
// so without this hand-off the parked candidate would be the permanent
// post-upgrade daemon, and cron/watch/session-restore would silently never run.
//
// It MUST run only after Supervisor.Run returns and the journal is gone (the
// caller proves this), so a unit-started or ad-hoc daemon (runDaemon(cfg,"")) does
// not defer to a still-active transaction in the entrypoint gate.
//
// The stop is guarded by IDENTITY, not supervision state: it stops a daemon only
// when that daemon IS our committed candidate — it reports expectedTransactionID,
// which the candidate keeps for its whole boot (#1947). A responder that is a
// different daemon (a foreign transaction's candidate, or an ordinary daemon with
// no id — the shape a rollback's restored previous daemon has) is never touched.
// The identity guard is the whole protection: our committed candidate is always
// the ad-hoc process we spawned, so once it is id-matched the stop is always
// correct, and no supervision probe is needed (nor wanted — an UNDETERMINED probe
// on a systemctl blip must not leave our own parked candidate un-replaced).
//
// The fresh daemon starts under whichever owner supervises this home. If the unit
// start fails, it falls back to an ad-hoc spawn so a COMMITTED upgrade NEVER ends
// with zero daemons — the candidate exited cleanly, so systemd would not restart
// it, and the loops would stay dead until the next `af` invocation. Best-effort:
// the commit is irreversible, so a failed hand-off is warned by the caller.
func adoptAfterUpgradeCommit(expectedTransactionID, canonicalExecPath string) error {
	h := upgradeRecoveryHealthFn()
	if h.PingErr != nil {
		return nil // the committed candidate should be serving; nothing answers → nothing to hand off
	}
	if h.TransactionID != expectedTransactionID {
		return nil // not our committed candidate (a rollback's restored previous, or a different transaction)
	}
	configDir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	// A resolve failure is unknown, not ad-hoc — but the candidate still must be
	// replaced, and an ad-hoc spawn always leaves a daemon whether or not a unit
	// exists, so fall through to it (the unit, if any, can re-adopt via `af daemon
	// adopt`). Only a definite OwnerUnit takes the unit path.
	owner, _ := ResolveSupervisionOwner(configDir)
	if _, err := stopDaemonFn(); err != nil {
		return fmt.Errorf("stop the committed candidate before handing it off: %w", err)
	}
	if err := waitForShutdownFn(); err != nil {
		return fmt.Errorf("the committed candidate did not release the control socket: %w", err)
	}
	if owner == OwnerUnit {
		if unitErr := startPreviousViaUnitFn(); unitErr == nil {
			return nil
		} else {
			log.WarningLog.Printf("upgrade committed but starting the installed unit failed; falling back to an ad-hoc daemon (run `af daemon adopt` to re-supervise): %v", unitErr)
		}
	}
	if err := startPreviousAdHocFn(canonicalExecPath); err != nil {
		return fmt.Errorf("start a daemon for the committed candidate: %w", err)
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
