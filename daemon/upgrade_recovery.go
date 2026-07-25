package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/sachiniyer/agent-factory/log"
)

// upgradeValidateGrace bounds how long ValidatePrevious waits for the restored
// previous daemon to answer Ping at the expected version with its recorded
// listeners. Package vars so tests can shorten the path.
var (
	upgradeValidateGrace = 30 * time.Second
	upgradeValidatePoll  = 100 * time.Millisecond
)

// Indirection points so the recovery/rollback operations can be exercised
// without a real service manager, a spawned daemon, or a live control socket.
// upgradeRecoveryHealthFn drives validatePreviousDaemon's readiness sequence;
// the start hooks let startPreviousDaemon's owner dispatch be tested hermetically.
var (
	upgradeRecoveryHealthFn = Health
	startPreviousViaUnitFn  = RestartAutostartUnit
	startPreviousAdHocFn    = launchDaemonProcessAt
	runRecoveryActorFn      = upgradetxn.RunRecoveryActor
	// The stop is seamed separately from the health probe so a test's stop actually
	// changes what the health probe then reports (the daemon going quiet). Bound to
	// the real primitives, StopConfirmed hinges on WaitForShutdownCompletion
	// OBSERVING the socket close — the fabricated-confirmation guard these ops exist
	// to hold — so the seam must preserve that observe-then-confirm ordering.
	stopDaemonFn      = StopDaemon
	waitForShutdownFn = WaitForShutdownCompletion
)

// HandleUpgradeRecoveryExec consumes the internal __upgrade-recovery invocation
// before normal Cobra startup, mirroring sessionenv.HandleInternalExec. On an
// ordinary invocation it returns immediately. On the recovery invocation it runs
// the previous-binary recovery actor and exits — a candidate can wake this exact
// command but can never acquire the recovery lease (the flock capability is
// granted only to the immutable previous binary).
func HandleUpgradeRecoveryExec() {
	invocation, matched, err := upgradetxn.ParseRecoveryInvocation(os.Args[1:])
	if !matched {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "af: invalid internal upgrade recovery invocation")
		os.Exit(64) // EX_USAGE: the argv is malformed, not a recovery failure.
	}
	// Bind this process to the home the transaction is recovering BEFORE any
	// home-derived path resolves — logging, the control socket, the pid file, and
	// the autostart unit all go through config.GetConfigDir(), which reads
	// AGENT_FACTORY_HOME. The recovery job execs us with --home only and supplies
	// no environment (renderSystemd/LaunchdRecoveryUnit emit no Environment=), so
	// without this every op would resolve the DEFAULT home and stop, validate, or
	// restart a different daemon than the one being recovered (#782/#1916, #2212).
	if setErr := os.Setenv("AGENT_FACTORY_HOME", invocation.HomeDir); setErr != nil {
		fmt.Fprintln(os.Stderr, "af: cannot bind the upgrade recovery home")
		os.Exit(1)
	}
	log.Initialize(false)
	defer log.Close()
	if runErr := RunUpgradeRecoveryActor(context.Background(), invocation); runErr != nil {
		log.ErrorLog.Printf("daemon upgrade recovery actor failed: %v", runErr)
		os.Exit(1)
	}
	os.Exit(0)
}

// RunUpgradeRecoveryActor binds the production supervisor operations to the
// upgradetxn recovery actor. The operations live here rather than in
// internal/upgradetxn because they reach into daemon lifecycle primitives, which
// upgradetxn cannot import without a cycle; SupervisorOperations is injectable
// precisely to cross that boundary.
func RunUpgradeRecoveryActor(ctx context.Context, invocation upgradetxn.RecoveryInvocation) error {
	// Capture, BEFORE the supervisor runs, whether OUR transaction (id-matched, not
	// a foreign or stale one that happens to be active) is the one recovering, and
	// the canonical path its committed candidate occupies — a successful commit
	// removes the journal during Cleanup, taking both with it. EVERY owner needs the
	// post-commit hand-off, not just unit-owned homes: the candidate ran ad-hoc
	// through probation and is parked in runDaemon's probation branch, so on an
	// ad-hoc home the parked process would otherwise be the permanent post-upgrade
	// daemon with its scheduler/watchers/session-restore never armed (#2212 R2a).
	ourTransaction := false
	var canonicalExecPath string
	if txn, loadErr := upgradetxn.Load(invocation.HomeDir); loadErr == nil {
		journal := txn.Journal()
		if journal.ID == invocation.TransactionID {
			ourTransaction = true
			canonicalExecPath = journal.ExecutablePath
		}
	}

	supervisor := upgradetxn.Supervisor{Operations: productionSupervisorOperations()}
	if err := runRecoveryActorFn(ctx, invocation, supervisor); err != nil {
		return err
	}

	// A nil error is NOT proof of commit: runRecoveryActorWith also returns nil for
	// a clean stand-down (a foreign/stale transaction we had no authority over),
	// ErrRecoveryActive, and a terminal rollback (which restores the previous daemon
	// under its own owner). Hand off ONLY on a positive commit signal — this was OUR
	// transaction AND its journal is now gone (Cleanup ran). rollback_failed
	// deliberately RETAINS the journal, and a newer transaction leaves a different one
	// — both leave a journal, so both are excluded here. The hand-off is additionally
	// id-guarded so it can only ever stop our own committed candidate.
	if !ourTransaction {
		return nil
	}
	if _, loadErr := upgradetxn.Load(invocation.HomeDir); !errors.Is(loadErr, upgradetxn.ErrNoActiveTransaction) {
		return nil // a journal is still present — not a completed commit of our transaction
	}
	if err := adoptAfterUpgradeCommitFn(invocation.TransactionID, canonicalExecPath); err != nil {
		log.WarningLog.Printf("upgrade committed but arming the post-upgrade daemon did not complete; check `af daemon status` and `af doctor`: %v", err)
	}
	return nil
}

// productionSupervisorOperations wires every SupervisorOperation to the daemon's
// real lifecycle primitives. R1 shipped the recovery/rollback ops and stubbed the
// forward ops as ErrUpgradeActivationNotEnabled; R2 makes the forward ops live so
// a candidate is actually launched, validated, committed, or rolled back. Every
// destructive op (both directions) is home-bound and fails closed.
func productionSupervisorOperations() upgradetxn.SupervisorOperations {
	return upgradetxn.SupervisorOperations{
		// Forward activation (#2212 R2).
		AwaitActivation:   awaitOldDaemonActivation,
		StopPrevious:      stopPreviousDaemon,
		StartCandidate:    startCandidateDaemon,
		ValidateCandidate: validateCandidateDaemon,
		ApproveCandidate:  approveCandidateDaemon,
		// Recovery / rollback (#2212 R1), reusing the #2168 daemon lifecycle.
		StopCandidate:      stopCandidateDaemon,
		StartPrevious:      startPreviousDaemon,
		ValidatePrevious:   validatePreviousDaemon,
		DisableRecoveryJob: disableRecoveryJob,
	}
}

// recoveryHomeGuard fails closed when this process is not resolved to the AF
// home the transaction is recovering. HandleUpgradeRecoveryExec binds the home
// from the invocation, so a mismatch means that binding did not take — and every
// home-dependent op (StopDaemon, the health probe, the autostart unit) would then
// act on a DIFFERENT daemon than the one under recovery. Refusing is the only safe
// answer: proceeding on the wrong home is how a "stop" fabricates StopConfirmed
// over a live candidate.
func recoveryHomeGuard(journal upgradetxn.Journal) error {
	dir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("resolve AF home for upgrade recovery: %w", err)
	}
	// Compare home IDENTITY, not string spelling. config.GetConfigDir returns
	// AGENT_FACTORY_HOME verbatim (no symlink resolution), while upgradetxn.Prepare
	// stored journal.HomeDir in its symlink-resolved form (canonicalExistingDir). A
	// user whose home traverses a symlink — a symlinked home dir, or macOS
	// /var -> /private/var — would otherwise spell the same home two ways and have
	// EVERY op refused (StopUnknown), failing that box's upgrades closed forever with
	// a baffling message. pathutil.ResolveForCompare canonicalizes both sides (incl.
	// the missing-leaf case, #2110), so equal homes compare equal regardless of
	// spelling. Today the actor's home is argv-pinned to the resolved journal.HomeDir
	// so the raw compare happened to hold, but this removes the latent trap.
	if pathutil.ResolveForCompare(dir) != pathutil.ResolveForCompare(journal.HomeDir) {
		return fmt.Errorf(
			"upgrade recovery is bound to AF home %q but the transaction recovers %q; refusing to act on the wrong daemon",
			dir, journal.HomeDir)
	}
	return nil
}

// stopCandidateDaemon stops the candidate daemon so the previous binary and
// metadata can be restored. The candidate serves this AF home's control socket
// and pid file, so StopDaemon signals exactly it; WaitForShutdownCompletion then
// upgrades a signalled stop into positive proof the socket is gone before the
// caller restores anything (#854). Idempotent: with nothing running, StopDaemon
// reports no daemon and the socket is already quiet — StopConfirmed.
func stopCandidateDaemon(ctx context.Context, journal upgradetxn.Journal) (upgradetxn.StopOutcome, error) {
	_ = ctx
	return stopDaemonForRecovery(journal, "candidate")
}

// stopDaemonForRecovery stops THIS home's serving daemon (the candidate on
// rollback, or the previous daemon on forward activation) and only reports
// StopConfirmed on positive proof the socket is gone. Both directions carry the
// same destructive authority: a fabricated StopConfirmed lets the supervisor
// swap a binary or restore metadata under a live daemon, so it fails closed on a
// home mismatch (StopUnknown) and never upgrades an unobserved stop to confirmed.
func stopDaemonForRecovery(journal upgradetxn.Journal, role string) (upgradetxn.StopOutcome, error) {
	// Fail closed if this process is not resolved to the home being recovered:
	// StopDaemon/WaitForShutdownCompletion act on config.GetConfigDir()'s home, so
	// a mismatch would report StopConfirmed while the transaction's daemon is still
	// alive — the destructive lie this op must never tell.
	if err := recoveryHomeGuard(journal); err != nil {
		return upgradetxn.StopUnknown, err
	}
	if _, err := stopDaemonFn(); err != nil {
		return upgradetxn.StopUnknown, fmt.Errorf("stop upgrade %s daemon: %w", role, err)
	}
	if err := waitForShutdownFn(); err != nil {
		// The socket is still answering at the deadline. Report it as still
		// running rather than confirming a stop we could not observe.
		return upgradetxn.StopStillRunning, nil
	}
	return upgradetxn.StopConfirmed, nil
}

// startPreviousDaemon brings the restored previous daemon back under the
// captured owner. The previous executable has already been restored to the
// canonical path by the transaction, so a service-managed owner starts the unit
// (reset-failed clears only the runtime failure counter, never the StartLimit*
// directives #2168/#2212 preserve) and an ad-hoc owner spawns the canonical
// binary directly. Idempotent: RestartAutostartUnit cycles a unit that is
// already up, and the ad-hoc launcher is a no-op reclaim when a daemon answers.
func startPreviousDaemon(ctx context.Context, journal upgradetxn.Journal) error {
	_ = ctx
	if err := recoveryHomeGuard(journal); err != nil {
		return err
	}
	switch journal.Daemon.Owner.Kind {
	case upgradetxn.SupervisionSystemd, upgradetxn.SupervisionLaunchd:
		if err := startPreviousViaUnitFn(); err != nil {
			return fmt.Errorf("restart previous daemon under its %s unit: %w", journal.Daemon.Owner.Kind, err)
		}
		return nil
	default:
		if err := startPreviousAdHocFn(journal.ExecutablePath); err != nil {
			return fmt.Errorf("spawn previous ad-hoc daemon %s: %w", journal.ExecutablePath, err)
		}
		return nil
	}
}

// validatePreviousDaemon confirms the restored previous daemon is genuinely the
// FromVersion daemon and has rebound every listener that was healthy before the
// upgrade — not merely that something answers the socket (#1947). It polls the
// no-spawn health probe within a bounded grace; a wrong version, a missing
// listener, or no answer by the deadline is a rollback validation failure.
func validatePreviousDaemon(ctx context.Context, journal upgradetxn.Journal) error {
	return awaitDaemonIdentity(ctx, journal, previousDaemonIdentity(journal))
}

// previousDaemonIdentity is what the restored previous daemon must be: FromVersion,
// an EMPTY transaction id (an ordinary daemon, never a surviving probation
// candidate — #1947), and every listener the journal recorded healthy. Production
// (validatePreviousDaemon) and its table test both build the identity through THIS
// one constructor, so the require-vs-reject-id polarity lives in exactly one place:
// a change that wrongly required the id — catastrophic when FromVersion == ToVersion
// (dev-install / re-published tag), where a surviving candidate would pass as the
// restored previous daemon — fails the test instead of silently regressing a
// byte-identical duplicate the test never exercised.
func previousDaemonIdentity(journal upgradetxn.Journal) daemonIdentity {
	return daemonIdentity{
		role:      "previous",
		version:   journal.FromVersion,
		listeners: journal.Daemon.Listeners,
	}
}

// awaitDaemonIdentity is the shared bounded poll for both directions: it holds
// the responding daemon to a required identity (version, transaction-id polarity,
// listeners) within upgradeValidateGrace. It is home-bound — a home mismatch
// means the health probe would inspect a DIFFERENT daemon, so it refuses rather
// than validate the wrong process.
func awaitDaemonIdentity(ctx context.Context, journal upgradetxn.Journal, want daemonIdentity) error {
	if err := recoveryHomeGuard(journal); err != nil {
		return err
	}
	deadline := time.Now().Add(upgradeValidateGrace)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = daemonMatchesIdentity(upgradeRecoveryHealthFn(), want)
		if lastErr == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s daemon did not become healthy at version %s within %s: %w",
				want.role, want.version, upgradeValidateGrace, lastErr)
		}
		time.Sleep(upgradeValidatePoll)
	}
}

// daemonIdentity is what a validated responder must be. requireTransactionID
// distinguishes the two directions this predicate serves and is the load-bearing
// difference between them: the restored PREVIOUS daemon is an ordinary daemon
// (empty id), while a probation CANDIDATE carries its transaction id for its
// whole boot precisely so a supervisor can tell them apart (#1947). Getting this
// backwards either passes a surviving candidate off as a rollback, or blesses a
// stale previous daemon as a validated candidate — both catastrophic when
// FromVersion == ToVersion (the normal rebuild/reinstall shape).
type daemonIdentity struct {
	role                 string // "previous"/"candidate", for messages only
	version              string
	requireTransactionID string // "" means the responder MUST report an empty id; non-empty means it MUST match
	listeners            upgradetxn.ListenerExpectation
}

// daemonMatchesIdentity is the single readiness predicate shared by
// validatePreviousDaemon and validateCandidateDaemon, factored so it is testable
// without real time and so the require-vs-reject-id polarity lives in exactly one
// place. It requires a live responder at the exact version, the expected
// transaction-id polarity, and every listener the journal recorded as healthy.
func daemonMatchesIdentity(h HealthStatus, want daemonIdentity) error {
	if h.PingErr != nil {
		return fmt.Errorf("%s daemon is not answering: %w", want.role, h.PingErr)
	}
	if want.requireTransactionID == "" {
		if h.TransactionID != "" {
			return fmt.Errorf("responder is an upgrade candidate (transaction %s), not the restored %s daemon",
				h.TransactionID, want.role)
		}
	} else if h.TransactionID != want.requireTransactionID {
		return fmt.Errorf("responder carries transaction %q, want the %s candidate for %q",
			h.TransactionID, want.role, want.requireTransactionID)
	}
	if h.DaemonVersion == "" {
		return fmt.Errorf("%s daemon answered but did not report its version", want.role)
	}
	if h.DaemonVersion != want.version {
		return fmt.Errorf("%s daemon reports version %s, want %s", want.role, h.DaemonVersion, want.version)
	}
	if want.listeners.HTTPUnixBound && !h.Listeners.HTTPUnixBound {
		return fmt.Errorf("%s daemon has not bound the HTTP control listener", want.role)
	}
	if want.listeners.TCPConfigured && want.listeners.TCPBound && !h.Listeners.TCPBound {
		return fmt.Errorf("%s daemon has not bound the configured TCP listener", want.role)
	}
	return nil
}

// disableRecoveryJob disarms the persistent recovery job's restart policy after
// a terminal rollback or commit, without stopping the still-running actor (it
// owns the flock and removes the active journal afterward). The controller
// forbids --now/bootout by shape, so this cannot race the actor's own teardown.
func disableRecoveryJob(ctx context.Context, journal upgradetxn.Journal) error {
	return upgradetxn.RecoveryJobController{}.Disable(ctx, journal)
}
