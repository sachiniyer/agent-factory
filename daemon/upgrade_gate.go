package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/sachiniyer/agent-factory/log"
)

// upgradeGateTimeout is the HARD ceiling on the entrypoint takeover gate —
// covering both waking the recovery job (a bounded service-manager exec) and
// waiting for that actor to take the recovery lock. It is passed as BOTH the
// EntrypointGate.TakeoverTimeout and a context deadline, so no single gate path
// — a live actor, a stale actor that never takes over, a half-written journal,
// or a wedged systemctl — can exceed it.
//
// It is kept within the SMALLEST daemon-spawn caller budget
// (ensureUnitStartTimeout, 2s) so the gate can never overrun a caller's
// readiness window and churn a fallback spawn (#2212). Only the CLIENT entrypoint
// (EnsureDaemon) ever spends this budget, and only on a stale journal: the
// daemon-BIND path (RunDaemon) skips the wake/wait entirely, so it never waits.
//
// This bound is the whole point of the gate's placement (#2212 R1). The gate
// sits on the daemon-spawn path, which is in front of every af launch. A daemon
// that will not start is recoverable; an af binary that hangs on every command
// is not. So the gate is fail-OPEN: only a provably live, in-progress upgrade
// stops a launch, and it does so with an immediate typed error rather than a
// wait. Every other outcome proceeds. Package var so tests can shorten it.
var upgradeGateTimeout = 2 * time.Second

// upgradeGateDecision is what checkUpgradeGate tells a daemon-spawn caller.
type upgradeGateDecision int

const (
	// upgradeGateProceed means start/spawn the daemon normally. Returned for no
	// journal, a corrupt or partial journal, a stale journal whose recovery
	// actor never took over within the deadline, a rollback_failed circuit
	// breaker, or any probe failure — anything but a provably live upgrade.
	upgradeGateProceed upgradeGateDecision = iota
	// upgradeGateInProgress means a live recovery actor provably owns an active
	// FORWARD upgrade. The accompanying error is the typed, retryable "daemon is
	// upgrading" message; NO caller may start a rival daemon over the supervisor.
	upgradeGateInProgress
	// upgradeGateRestoringPrevious means a live recovery actor is rolling back and
	// restarting the PREVIOUS daemon right now (a rollback-restore phase). The
	// daemon-BIND path (RunDaemon) must PROCEED — it IS that previous daemon, and
	// deferring would deadlock the rollback (the actor is waiting for exactly this
	// daemon to answer). A client must still DEFER: it must not stop and replace
	// the daemon the actor is validating.
	upgradeGateRestoringPrevious
)

// runEntrypointGate is the actual gate probe, indirected so the fail-open
// classification can be tested against every EntrypointGate.Check outcome
// without constructing each on-disk journal state. skipWake selects the
// non-waking daemon-bind variant. Production wires the real bounded gate.
var runEntrypointGate = func(ctx context.Context, homeDir string, skipWake bool) error {
	gate := upgradetxn.EntrypointGate{TakeoverTimeout: upgradeGateTimeout, SkipWake: skipWake}
	return gate.Check(ctx, homeDir)
}

// checkUpgradeGate runs the transaction entrypoint gate fail-open and classifies
// the result for a daemon-spawn caller.
//
// skipWake=false is the CLIENT variant (EnsureDaemon): on a stale journal it may
// wake the recovery job and wait for takeover, bounded by upgradeGateTimeout.
// skipWake=true is the daemon-BIND variant (RunDaemon): non-waking and fast — it
// only reports a live actor to defer to and never waits, so it cannot overrun the
// caller's bind budget.
//
// It returns:
//   - upgradeGateProceed for no journal (the common case — a stat plus one
//     ErrNoActiveTransaction load), a corrupt/partial journal, a stale/dead-actor
//     journal, the rollback_failed breaker, or any probe error. Fail-open: a bad
//     journal never blocks a launch;
//   - upgradeGateInProgress for a live FORWARD upgrade — every caller must defer;
//   - upgradeGateRestoringPrevious for a live ROLLBACK restoring the previous
//     daemon — the bind path proceeds, a client defers.
//
// The context deadline is the hard bound over the ENTIRE call, including the
// service-manager exec that Wake runs, so a wedged systemctl cannot hang the
// launch.
func checkUpgradeGate(homeDir string, skipWake bool) (upgradeGateDecision, error) {
	ctx, cancel := context.WithTimeout(context.Background(), upgradeGateTimeout)
	defer cancel()

	err := runEntrypointGate(ctx, homeDir, skipWake)
	if err == nil {
		return upgradeGateProceed, nil
	}

	var inProgress *upgradetxn.UpgradeInProgressError
	if errors.As(err, &inProgress) {
		// A live recovery actor owns the transaction. During a rollback-restore
		// phase it is starting the previous daemon, which the bind path must be
		// allowed to become; otherwise defer so no rival is spawned over the
		// supervisor. Immediate either way — never a wait.
		if inProgress.Phase.IsRollbackRestore() {
			return upgradeGateRestoringPrevious, err
		}
		return upgradeGateInProgress, err
	}

	// Blocked recovery, a corrupt/partial journal, a timed-out takeover, the
	// context deadline, or any other probe failure. Do NOT block the launch:
	// log once and proceed with normal startup. Spawning a rival daemon is the
	// accepted risk here; wedging every af invocation on a bad journal is not.
	log.WarningLog.Printf("daemon upgrade entrypoint gate did not resolve (proceeding with normal startup): %v", err)
	return upgradeGateProceed, nil
}

// configHomeDir resolves the AF home the entrypoint gate inspects. A resolution
// failure is not fatal to the gate: with no resolvable home there is no journal
// to honor, so the caller proceeds with normal startup.
func configHomeDir() (string, bool) {
	dir, err := config.GetConfigDir()
	if err != nil || dir == "" {
		return "", false
	}
	return dir, true
}
