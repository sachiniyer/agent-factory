package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/sachiniyer/agent-factory/log"
)

// ErrUpgradeActivationNotEnabled is returned by every FORWARD-activation
// SupervisorOperation in this slice (#2212 R1). R1 wires the entrypoint takeover
// gate and the persistent recovery-job launcher, and implements the
// recovery/rollback-side operations — but it deliberately does NOT implement the
// operations that quiesce or replace a live daemon (StopPrevious, StartCandidate,
// ValidateCandidate, ApproveCandidate, AwaitActivation). Those land with the
// daemon quiesce/probation slice (R2). Binding them fail-closed makes "quiesce or
// replace a live daemon" structurally impossible here: nothing in this build
// creates a transaction, and even if one existed the supervisor could never drive
// a forward activation.
var ErrUpgradeActivationNotEnabled = errors.New("daemon upgrade activation is not enabled in this build")

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
	supervisor := upgradetxn.Supervisor{Operations: productionSupervisorOperations()}
	return upgradetxn.RunRecoveryActor(ctx, invocation, supervisor)
}

// productionSupervisorOperations wires the recovery/rollback-side operations to
// the daemon's real lifecycle primitives and binds the forward-activation
// operations to ErrUpgradeActivationNotEnabled (R1; see that error's doc).
func productionSupervisorOperations() upgradetxn.SupervisorOperations {
	notEnabled := func(context.Context, upgradetxn.Journal) error {
		return ErrUpgradeActivationNotEnabled
	}
	return upgradetxn.SupervisorOperations{
		// Forward activation — not in this slice.
		AwaitActivation: notEnabled,
		StopPrevious: func(context.Context, upgradetxn.Journal) (upgradetxn.StopOutcome, error) {
			return upgradetxn.StopUnknown, ErrUpgradeActivationNotEnabled
		},
		StartCandidate:    notEnabled,
		ValidateCandidate: notEnabled,
		ApproveCandidate:  notEnabled,
		// Recovery / rollback — real, reusing the #2168 daemon lifecycle.
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
	if dir != journal.HomeDir {
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
	// Fail closed if this process is not resolved to the home being recovered:
	// StopDaemon/WaitForShutdownCompletion act on config.GetConfigDir()'s home, so
	// a mismatch would report StopConfirmed while the transaction's candidate is
	// still alive — the destructive lie this op must never tell. StopUnknown, not
	// StopConfirmed, so the supervisor never restores the previous binary under a
	// live candidate.
	if err := recoveryHomeGuard(journal); err != nil {
		return upgradetxn.StopUnknown, err
	}
	if _, err := StopDaemon(); err != nil {
		return upgradetxn.StopUnknown, fmt.Errorf("stop upgrade candidate daemon: %w", err)
	}
	if err := WaitForShutdownCompletion(); err != nil {
		// The socket is still answering at the deadline. Report it as still
		// running rather than confirming a stop we could not observe — restoring
		// the previous binary under a live candidate would be destructive.
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
	if err := recoveryHomeGuard(journal); err != nil {
		return err
	}
	deadline := time.Now().Add(upgradeValidateGrace)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h := upgradeRecoveryHealthFn()
		lastErr = previousDaemonHealthy(h, journal)
		if lastErr == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("restored previous daemon did not become healthy at version %s within %s: %w",
				journal.FromVersion, upgradeValidateGrace, lastErr)
		}
		time.Sleep(upgradeValidatePoll)
	}
}

// previousDaemonHealthy is the single readiness predicate for validatePreviousDaemon,
// factored so it is testable without real time. It requires a live responder, the
// exact FromVersion, and every listener the journal recorded as healthy.
func previousDaemonHealthy(h HealthStatus, journal upgradetxn.Journal) error {
	if h.PingErr != nil {
		return fmt.Errorf("previous daemon is not answering: %w", h.PingErr)
	}
	// A non-empty transaction id is the tell of an upgrade CANDIDATE in probation
	// (runDaemon carries it; an ordinary daemon reports empty). The restored
	// previous daemon is an ordinary daemon, so a responder still carrying a
	// transaction id is the candidate that never went away — not proof of a
	// restart. Ping retains this field for the full candidate boot exactly so a
	// supervisor can reject it (#1947). Without this check a rebuild/reinstall
	// where from==to (dev-install, a re-published tag) would let a surviving
	// candidate satisfy every version/listener test and pass off as rolled back.
	if h.TransactionID != "" {
		return fmt.Errorf("responder is an upgrade candidate (transaction %s), not the restored previous daemon", h.TransactionID)
	}
	if h.DaemonVersion == "" {
		return errors.New("previous daemon answered but did not report its version")
	}
	if h.DaemonVersion != journal.FromVersion {
		return fmt.Errorf("previous daemon reports version %s, want %s", h.DaemonVersion, journal.FromVersion)
	}
	want := journal.Daemon.Listeners
	if want.HTTPUnixBound && !h.Listeners.HTTPUnixBound {
		return errors.New("previous daemon has not rebound the HTTP control listener")
	}
	if want.TCPConfigured && want.TCPBound && !h.Listeners.TCPBound {
		return errors.New("previous daemon has not rebound the configured TCP listener")
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
