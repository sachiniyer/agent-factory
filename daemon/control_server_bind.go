package daemon

import (
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
)

// daemonSpawnLockTarget returns the lock target whose adjacent flock file
// (daemon.spawn.lock, via config.WithFileLock) serializes the daemon spawn
// window across processes.
func daemonSpawnLockTarget() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.spawn"), nil
}

// testHookSpawnPingPassed runs between the under-lock ping re-check and the
// socket bind in bindControlServerExclusive. Tests substitute it to hold a
// spawner inside that window and prove a concurrent spawner cannot enter it
// at the same time. No-op in production.
var testHookSpawnPingPassed = func() {}

// bindControlServerExclusive re-checks for a live daemon, publishes this
// daemon's PID file, and binds the control socket — all while holding an
// exclusive cross-process file lock, making the ping→publish→bind sequence
// atomic across processes. RunDaemon's top-of-function ping guard rejects
// the common duplicate-daemon cases, but two daemons starting
// near-simultaneously can both pass that ping before either binds;
// the second startControlServer would then unlink and rebind the socket path,
// orphaning the first daemon — alive and looping, but unreachable (#718).
//
// The PID write belongs inside the lock, between the two ends of the
// sequence: after the under-lock ping, the final existing-daemon check — a
// pre-home-lock legacy daemon that slipped the top ping keeps its PID file,
// which an earlier write-then-alreadyRunning-exit would have overwritten
// and then deleted — and before the bind, so a Ping can never succeed for
// a daemon whose daemon.pid does not yet exist (#5188).
//
// The lock is held only for the ping→publish→bind window, not the daemon
// lifetime, and flock is released by the kernel if the holder dies, so a
// crashed spawner cannot wedge future spawns.
//
// Returns alreadyRunning=true when a live daemon answered the under-lock
// ping; the caller must exit cleanly (a non-zero exit would trip the
// autostart unit's Restart=on-failure into a retry loop against the live
// daemon).
func bindControlServerExclusive(manager *Manager, scheduler *taskScheduler, watchers *watcherSupervisor, shutdownCh chan struct{}) (closeFn func() error, alreadyRunning bool, err error) {
	lockTarget, lockTargetErr := daemonSpawnLockTarget()
	if lockTargetErr != nil {
		return nil, false, lockTargetErr
	}
	lockErr := config.WithFileLock(lockTarget, func() error {
		if pingErr := pingDaemon(); pingErr == nil {
			alreadyRunning = true
			return nil
		}
		// daemon.pid is published at exactly this point — after the
		// spawn-locked ping proves no daemon is answering (a pre-home-lock
		// legacy daemon that slipped the top-of-runDaemon ping keeps its
		// PID file: overwriting it here and then unlinking it on the
		// alreadyRunning exit would strip a live daemon's management
		// handle), and before the control socket below can accept its
		// first Ping, so a successful readiness probe structurally implies
		// the file exists (#5188). The write is FAIL-CLOSED: a daemon that
		// cannot publish its identity would serve while invisible to
		// StopDaemon, Health, `af daemon status`, and doctor, which is
		// worse than not serving. The caller sees the returned error on a
		// foreground start; a detached spawn's cause lands in the daemon
		// log (persisting it for status/doctor is #5196).
		if err := writeDaemonPIDFile(); err != nil {
			return err
		}
		testHookSpawnPingPassed()
		var serverErr error
		closeFn, serverErr = startControlServer(manager, scheduler, watchers, shutdownCh)
		if serverErr != nil {
			// daemon.pid was published above, but runDaemon's removal
			// defer only registers on the success path — without this
			// cleanup the file keeps naming a process that is about to
			// exit (#5188). Still under the spawn lock, so nothing else
			// could have published; the unlink removes exactly the file
			// this process wrote.
			removeDaemonPIDFile()
		}
		return serverErr
	})
	if lockErr != nil {
		return nil, false, lockErr
	}
	return closeFn, alreadyRunning, nil
}
