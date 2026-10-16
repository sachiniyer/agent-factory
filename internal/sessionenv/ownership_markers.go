package sessionenv

import "os"

// The watch-ownership marker that must never cross the af process boundary
// (#5329).
//
// The daemon stamps AF_HOME and AF_WATCH_GEN into every watch command it spawns
// (daemon/watcher_orphans.go, envWatchGeneration), and a restarted daemon reaps
// any same-home process group carrying a foreign generation. That proof is only
// sound if the generation stays on the watch command's own processes. A watch
// script that runs `af` while the daemon is down makes af ad-hoc-spawn a daemon,
// which re-execs `af --daemon` with the caller's environment — so without this
// strip that daemon, its tmux server and every session under it would inherit
// the stale generation, and the next restart's sweep would reap the user's
// whole session fleet.
//
// Only AF_WATCH_GEN is stripped. AF_HOME deliberately survives: it is the
// existing pane/session ancestry marker (#1104, session/tmux/envmarker.go's
// EnvMarkerHome) that session/tmux/reap.go and `af doctor` rely on, and agents
// run af from inside panes all the time. The leak is closed anyway, because the
// sweep requires BOTH a foreign AF_WATCH_GEN AND a matching AF_HOME — a process
// carrying only AF_HOME is never reapable. AF_SESSION/AF_SESSION_GEN stay
// because `af sessions whoami` reads its own AF_SESSION to cross-check its
// pane, AF_DAEMON_URL/AF_DAEMON_TOKEN because panes need them to reach the
// daemon, and AF_TESTGUARD_RUN for test-leak attribution.
//
// The name is a literal because this package cannot import daemon without a
// cycle.
var ownershipMarkers = []string{"AF_WATCH_GEN"}

// StripOwnershipMarkers removes af's watch-generation marker from this
// process's environment, so nothing af spawns afterwards inherits it (#5329).
//
// main calls it once, right after HandleInternalExec and before anything else
// can spawn a child: the dedicated tmux server, upgrade recovery, and every
// ordinary CLI path, including an ad-hoc daemon launch. It changes what
// CHILDREN inherit, not this process's own /proc/<pid>/environ, which is fixed
// at exec; the daemon's sweep refuses to signal any group holding af
// infrastructure for that reason.
func StripOwnershipMarkers() {
	for _, name := range ownershipMarkers {
		_ = os.Unsetenv(name)
	}
}
