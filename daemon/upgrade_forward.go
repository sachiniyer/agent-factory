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

// adoptAfterUpgradeCommit hands the just-committed candidate — which ran ad-hoc
// through probation (Option A) — to the installed unit, reusing the #2168 adopt
// lifecycle. It MUST run only after Supervisor.Run returns and the journal is
// gone, so the unit-started daemon (runDaemon(cfg,"")) does not defer to the
// still-active transaction in the entrypoint gate.
//
// Idempotent and identity-agnostic: it hands off only when an installed unit owns
// this home AND the responder is not already unit-supervised, so after a ROLLBACK
// (the previous daemon is already unit-supervised via startPreviousDaemon) it
// no-ops. Best-effort: the commit is irreversible and the candidate is serving, so
// a failed hand-off is warned by the caller, not fatal — `af daemon adopt` or the
// next login re-supervises it.
func adoptAfterUpgradeCommit() error {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	owner, err := ResolveSupervisionOwner(configDir)
	if err != nil || owner != OwnerUnit {
		return nil // no installed unit serves this home — nothing to hand off to
	}
	h := upgradeRecoveryHealthFn()
	if h.PingErr != nil {
		return nil // no responder to hand off (a committed candidate always serves)
	}
	supervised := false
	ServingDaemonSupervised(h, AutostartSupervision()).Match(
		func() { supervised = true },
		func() {}, func() {}, func(error) {},
	)
	if supervised {
		return nil // the installed unit already owns the responder (e.g. after rollback)
	}
	if _, err := StopDaemon(); err != nil {
		return fmt.Errorf("stop the ad-hoc committed candidate before adopting: %w", err)
	}
	if err := WaitForShutdownCompletion(); err != nil {
		return fmt.Errorf("the committed candidate did not release the control socket: %w", err)
	}
	if err := startPreviousViaUnitFn(); err != nil {
		return fmt.Errorf("start the installed unit to supervise the committed candidate: %w", err)
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
