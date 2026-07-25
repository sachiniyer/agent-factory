package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/sachiniyer/agent-factory/log"
)

// upgradeActivationSupervisorReadyGrace bounds how long the trigger waits for the
// previous-binary recovery actor to reach supervisor_ready before giving up. The
// actor forks, acquires the lease, and publishes its readiness proof; a minute is
// generous without hanging forever on a wedged actor. Package var so tests shorten it.
var upgradeActivationSupervisorReadyGrace = 60 * time.Second

// Seams so the trigger runs end-to-end without spawning a real recovery actor,
// touching the host service manager, or re-execing the real binary. The
// lease-handshake pair (AwaitSupervisorReady / AuthorizeActivation) is seamed
// because a genuine supervisor_ready proof requires the actor to run as the exact
// preserved previous binary (TryAcquireRecovery's path identity) — a real spawned
// process, which is R4's territory. AwaitSupervisorReady's own real-journal behavior
// is proven in the upgradetxn package where that identity seam is reachable.
var (
	upgradeTriggerExecutableFn     = os.Executable
	upgradeTriggerVersionFn        = Version
	upgradeRecoveryJobControllerFn = func() upgradetxn.RecoveryJobController { return upgradetxn.RecoveryJobController{} }
	upgradeAwaitSupervisorReadyFn  = upgradetxn.AwaitSupervisorReady
	upgradeAuthorizeActivationFn   = func(txn *upgradetxn.Transaction, transactionID, nonce string) error {
		return txn.AuthorizeActivation(transactionID, nonce)
	}
	upgradeAbortPreparedFn = upgradetxn.AbortPreparedTransaction
)

// triggerUpgradeActivation is the OLD-daemon side of an upgrade activation, run
// inside the serving daemon (#2212 R2b). R3's update loop will call it once it has
// decided to install a candidate; R2b wires it to nothing. It stages the candidate,
// hands supervision to the immutable previous binary, and quiesces + exits so the
// candidate can bind the socket.
//
// It is built so the irreversible path never promotes weak evidence to proof (the
// habit behind every R2a P1). Every conclusion about the actor is a POSITIVE
// observation, never an error being nil/non-nil: AwaitSupervisorReady proves the
// actor took the lease AND reached supervisor_ready before AuthorizeActivation
// consents, and an actor that never gets there is a LOUD error that leaves the
// daemon serving — never an assumed success. Only after authorizing does the daemon
// quiesce (stop admitting, report DaemonPhaseQuiescing so a stuck hand-off is
// visible) and exit; the actor's StopPrevious then confirms it is gone.
func triggerUpgradeActivation(ctx context.Context, lifecycle *daemonLifecycle, requestExit func(), candidate []byte, toVersion string) error {
	plan, err := captureUpgradePlan(lifecycle, candidate, toVersion)
	if err != nil {
		return fmt.Errorf("capture upgrade plan: %w", err)
	}
	txn, err := upgradetxn.Prepare(plan)
	if err != nil {
		return fmt.Errorf("prepare upgrade transaction: %w", err)
	}
	journal := txn.Journal()
	// From here active.json is published (fsynced). Any failure BEFORE an actor takes
	// authority must abort it, or one transient service-manager error wedges the
	// upgrade path forever — every future Prepare fails "already active", with no
	// actor able to clear it. abortPrepared refuses once an actor owns the
	// transaction, so it is safe to call and a no-op past that point.
	abortPrepared := func(cause error) error {
		if abortErr := upgradeAbortPreparedFn(journal.HomeDir, journal.ID); abortErr != nil {
			return errors.Join(cause, fmt.Errorf("abort the prepared upgrade transaction: %w", abortErr))
		}
		return cause
	}
	if err := upgradeRecoveryJobControllerFn().InstallAndStart(ctx, txn); err != nil {
		return abortPrepared(fmt.Errorf("install and start the recovery actor: %w", err))
	}
	deadline := time.Now().Add(upgradeActivationSupervisorReadyGrace)
	if err := upgradeAwaitSupervisorReadyFn(ctx, journal.HomeDir, deadline); err != nil {
		// The actor never PROVED supervisor_ready. Do NOT authorize or exit: the
		// daemon keeps serving. Abort the prepared transaction (a no-op if an actor
		// took it and its own deadline will drive teardown) so the upgrade path is not
		// wedged, and surface the failure loudly — never an assumed success.
		return abortPrepared(fmt.Errorf("upgrade recovery actor did not reach supervisor_ready; daemon keeps serving: %w", err))
	}
	if err := upgradeAuthorizeActivationFn(txn, journal.ID, journal.RecoveryNonce); err != nil {
		// The actor is at supervisor_ready and owns the transaction; its own
		// supervisor_ready deadline times out and aborts. Do not abort from here
		// (abortPrepared would refuse anyway) — just surface the failure.
		return fmt.Errorf("authorize upgrade activation: %w", err)
	}
	// Authorized: the actor is greenlit to replace us. Stop admitting new work so no
	// mutation races the hand-off, report DaemonPhaseQuiescing, then free the socket.
	lifecycle.markQuiescing()
	log.InfoLog.Printf("upgrade activation authorized for transaction %s; quiescing and exiting for the candidate", journal.ID)
	requestExit()
	return nil
}

// captureUpgradePlan snapshots the LIVE daemon into a transaction plan. Candidate
// bytes and ToVersion are inputs (R3 supplies them from the release; the test a
// stamped fake); everything else is read from this running daemon so the actor
// validates and, on failure, restores exactly what was serving.
func captureUpgradePlan(lifecycle *daemonLifecycle, candidate []byte, toVersion string) (upgradetxn.Plan, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return upgradetxn.Plan{}, err
	}
	executable, err := upgradeTriggerExecutableFn()
	if err != nil {
		return upgradetxn.Plan{}, fmt.Errorf("resolve running executable: %w", err)
	}
	id, err := newUpgradeTransactionID()
	if err != nil {
		return upgradetxn.Plan{}, err
	}
	owner, err := upgradeDaemonOwner(home)
	if err != nil {
		return upgradetxn.Plan{}, err
	}
	recoveryJob, err := upgradeRecoveryJobFor(id, owner.Kind)
	if err != nil {
		return upgradetxn.Plan{}, err
	}
	snap := lifecycle.snapshot()
	return upgradetxn.Plan{
		ID:             id,
		HomeDir:        home,
		ExecutablePath: executable,
		FromVersion:    upgradeTriggerVersionFn(),
		ToVersion:      toVersion,
		Candidate:      candidate,
		Daemon: upgradetxn.DaemonSnapshot{
			WasRunning: true,
			BootID:     snap.bootID,
			Owner:      owner,
			Listeners:  toListenerExpectation(snap.listeners),
		},
		RecoveryJob: recoveryJob,
		// MetadataPaths is intentionally empty because R2b STARTS NO CANDIDATE — the
		// trigger is wired to nothing yet, so no candidate daemon boots to write any
		// metadata. It is NOT empty because probation makes divergence safe: probation
		// blocks mutating RPCs, but migrate-on-load is not an RPC and runs anyway.
		// config.LoadAndMigrateSchemaFile atomically REWRITES a state file as it reads
		// it (config/schema_migration.go), and it runs at daemon load — before restore —
		// for every per-repo instances.json (config.MigrateAllRepoInstancesForDaemonLoad)
		// and for tasks.json (task/schema_migration.go). So a candidate that boots,
		// migrates vN→vN+1, then FAILS validation would leave a binary-only rollback
		// restoring the FromVersion daemon onto state files written in a schema it can no
		// longer read. That gap is inert only while nothing is staged; it goes live the
		// moment R3 stages a real candidate, so R3 MUST populate this manifest with those
		// migrated paths (recorded as the R3 prerequisite on #2212). Capturing them here
		// now, with no candidate to protect, would only be guessing at the set.
	}, nil
}

func newUpgradeTransactionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate upgrade transaction id: %w", err)
	}
	return "upgrade-" + hex.EncodeToString(buf), nil
}

// upgradeDaemonOwner captures the CURRENT supervision owner so the actor restores
// the daemon under the same owner it was serving under. A resolve failure is an
// error, never silently coerced to ad-hoc.
func upgradeDaemonOwner(home string) (upgradetxn.DaemonOwner, error) {
	owner, err := ResolveSupervisionOwner(home)
	if err != nil {
		return upgradetxn.DaemonOwner{}, err
	}
	if owner != OwnerUnit {
		return upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionAdHoc}, nil
	}
	switch autostartGOOS {
	case "linux":
		return upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionSystemd, ServiceName: autostartUnitName}, nil
	case "darwin":
		return upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionLaunchd, ServiceName: autostartLaunchdLabel}, nil
	default:
		return upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionAdHoc}, nil
	}
}

// upgradeRecoveryJobFor derives the transaction-scoped recovery launcher for this
// owner via the canonical NewRecoveryJob builder (so the name/path can never drift
// from what the actor side expects). The recovery unit lives beside the autostart
// unit; an ad-hoc daemon uses the detached actor (recovered by the all-entrypoint
// takeover gate).
func upgradeRecoveryJobFor(id string, kind upgradetxn.SupervisionKind) (upgradetxn.RecoveryJob, error) {
	var jobKind upgradetxn.RecoveryJobKind
	switch kind {
	case upgradetxn.SupervisionSystemd:
		jobKind = upgradetxn.RecoveryJobSystemd
	case upgradetxn.SupervisionLaunchd:
		jobKind = upgradetxn.RecoveryJobLaunchd
	default:
		return upgradetxn.RecoveryJob{Kind: upgradetxn.RecoveryJobDetached}, nil
	}
	dir, err := upgradeUnitDir()
	if err != nil {
		return upgradetxn.RecoveryJob{}, err
	}
	return upgradetxn.NewRecoveryJob(jobKind, id, dir)
}

func upgradeUnitDir() (string, error) {
	path, err := autostartUnitFilePath()
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("no autostart unit directory on %s", autostartGOOS)
	}
	return filepath.Dir(path), nil
}

func toListenerExpectation(l DaemonListenerStatus) upgradetxn.ListenerExpectation {
	return upgradetxn.ListenerExpectation{
		HTTPUnixBound: l.HTTPUnixBound,
		TCPConfigured: l.TCPConfigured,
		TCPListenAddr: l.TCPListenAddr,
		TCPBound:      l.TCPBound,
	}
}
