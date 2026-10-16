package sessionenv

import "os"

// Ownership markers that must never cross the af process boundary (#5329).
//
// The daemon stamps AF_HOME and AF_WATCH_GEN into every watch command it spawns
// (daemon/watcher_orphans.go, envWatchGeneration), and a restarted daemon reaps
// any same-home process group carrying a foreign generation. That proof is only
// sound if the markers stay on the watch command's own processes. A watch script
// that runs `af` while the daemon is down makes af ad-hoc-spawn a daemon, which
// re-execs `af --daemon` with the caller's environment — so without this strip
// that daemon, its tmux server and every session under it would inherit the
// stale generation, and the next restart's sweep would reap the user's whole
// session fleet.
//
// The names are literals because this package cannot import daemon (or
// session/tmux, whose envmarker.go defines AF_HOME as EnvMarkerHome) without a
// cycle. Exactly these two are stripped: AF_SESSION/AF_SESSION_GEN stay because
// `af sessions whoami` reads its own AF_SESSION to cross-check its pane,
// AF_DAEMON_URL/AF_DAEMON_TOKEN stay because panes need them to reach the
// daemon, and AF_TESTGUARD_RUN stays for test-leak attribution.
var ownershipMarkers = []string{"AF_WATCH_GEN", "AF_HOME"}

// StripOwnershipMarkers removes af's watch-ownership markers from this process's
// environment, so nothing af spawns afterwards inherits them (#5329).
//
// main calls it once, AFTER HandleInternalExec and before anything else can
// spawn a child. The order is load-bearing: the pane shim is a legitimate pane
// member that must keep its inherited AF_HOME to stamp the pane it execs, and it
// never returns from HandleInternalExec. It changes what CHILDREN inherit, not
// this process's own /proc/<pid>/environ, which is fixed at exec; the daemon's
// sweep refuses to signal any group holding an af process for that reason.
func StripOwnershipMarkers() {
	for _, name := range ownershipMarkers {
		_ = os.Unsetenv(name)
	}
}
