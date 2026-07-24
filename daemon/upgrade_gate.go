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
// This bound is the whole point of the gate's placement (#2212 R1). The gate
// sits on the daemon-spawn path, which is in front of every af launch. A daemon
// that will not start is recoverable; an af binary that hangs on every command
// is not. So the gate is fail-OPEN: only a provably live, in-progress upgrade
// stops a launch, and it does so with an immediate typed error rather than a
// wait. Every other outcome proceeds. Package var so tests can shorten it.
var upgradeGateTimeout = 6 * time.Second

// upgradeGateDecision is what checkUpgradeGate tells a daemon-spawn caller.
type upgradeGateDecision int

const (
	// upgradeGateProceed means start/spawn the daemon normally. Returned for no
	// journal, a corrupt or partial journal, a stale journal whose recovery
	// actor never took over within the deadline, a rollback_failed circuit
	// breaker, or any probe failure — anything but a provably live upgrade.
	upgradeGateProceed upgradeGateDecision = iota
	// upgradeGateInProgress means a live recovery actor provably owns an active
	// upgrade transaction. The accompanying error is the typed, retryable
	// "daemon is upgrading" message; the caller must NOT spawn a rival daemon.
	upgradeGateInProgress
)

// checkUpgradeGate runs the transaction entrypoint gate fail-open. It returns
// upgradeGateInProgress ONLY when a live recovery actor provably owns an active
// transaction (an immediate, non-blocking result). Every other outcome returns
// upgradeGateProceed:
//
//   - no journal on disk (the overwhelmingly common case) — a stat plus one
//     ErrNoActiveTransaction load, so a healthy cold start is effectively free;
//   - a corrupt, partial, or otherwise unparseable journal — treated as no
//     transaction, never as a fatal error and never as an unbounded wait;
//   - a stale/orphaned journal whose actor died and never re-took the lock
//     within upgradeGateTimeout — the gate wakes the recovery job, waits at most
//     the deadline, then proceeds;
//   - the rollback_failed circuit breaker, or any service-manager/probe error.
//
// The context deadline is the hard bound over the ENTIRE call, including the
// service-manager exec that Wake runs, so a wedged systemctl cannot hang the
// launch. Nothing here blocks longer than upgradeGateTimeout.
// runEntrypointGate is the actual gate probe, indirected so the fail-open
// classification can be tested against every EntrypointGate.Check outcome
// without constructing each on-disk journal state. Production wires the real
// bounded gate.
var runEntrypointGate = func(ctx context.Context, homeDir string) error {
	gate := upgradetxn.EntrypointGate{TakeoverTimeout: upgradeGateTimeout}
	return gate.Check(ctx, homeDir)
}

func checkUpgradeGate(homeDir string) (upgradeGateDecision, error) {
	ctx, cancel := context.WithTimeout(context.Background(), upgradeGateTimeout)
	defer cancel()

	err := runEntrypointGate(ctx, homeDir)
	if err == nil {
		return upgradeGateProceed, nil
	}

	var inProgress *upgradetxn.UpgradeInProgressError
	if errors.As(err, &inProgress) {
		// A live recovery actor owns the transaction. Report it so the caller
		// surfaces "daemon is upgrading; retry shortly" instead of spawning a
		// rival over the supervisor. Immediate — never a wait.
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
