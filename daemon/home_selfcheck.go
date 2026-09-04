package daemon

import (
	"os"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The abandoned-daemon self-check, both halves.
//
// A daemon whose AF home is deleted out from under it can be reached by nothing
// on the control plane, yet it keeps firing cron schedules: #1093 was a leaked
// debug daemon spawning a session nightly for 23 days, and the #3842 census
// found 9,892 /tmp/af-* directories on the maintainer's box. watchDaemonHome
// (#1093/#1094) is the detector — and it only works if the daemon does not put
// the directory back. #3843 stopped the socket binds from doing that;
// latchDaemonHomePresent stops everything this daemon WRITES from doing it
// (#3845).
//
// Split out of daemon.go, which sat exactly on the 1000-line limit, along the
// seam home_selfcheck_test.go was already named for.

// homeCheckInterval is how often watchDaemonHome verifies the daemon's own AF
// home directory still exists. A package var so tests can shorten it, and
// overridable via AF_HOME_CHECK_INTERVAL so the INTEGRATION harness can too: the
// daemon runs as its own process there, so a package var alone is out of reach
// (the same seam AF_WS_KEEPALIVE_INTERVAL provides for the WS broker).
var homeCheckInterval = envDurationOr("AF_HOME_CHECK_INTERVAL", 60*time.Second)

// homeMissingChecksToExit is how many consecutive missing observations
// watchDaemonHome requires before declaring the home deleted. Requiring two
// keeps a single transient stat blip from taking down a healthy daemon.
const homeMissingChecksToExit = 2

// watchDaemonHome periodically stats homeDir and closes homeGone once the
// directory has been missing for homeMissingChecksToExit consecutive checks,
// signaling RunDaemon to shut down (#1093). Only a definite ENOENT counts as
// missing: any other stat error (EACCES, EIO) leaves the directory's fate
// unknown, and a false-positive shutdown of a healthy daemon is worse than
// letting an abandoned one linger until the next check. The daemon's binary
// path is deliberately NOT checked — upgrades replace the binary while the
// daemon legitimately keeps running.
func watchDaemonHome(homeDir string, stopCh <-chan struct{}, homeGone chan<- struct{}) {
	ticker := time.NewTicker(homeCheckInterval)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
		}
		var exit bool
		if misses, exit = applyHomeCheck(homeDir, misses); exit {
			log.WarningLog.Printf("agent-factory home %s no longer exists; shutting down abandoned daemon", homeDir)
			close(homeGone)
			return
		}
	}
}

// applyHomeCheck folds one stat of homeDir into the running consecutive-miss
// counter and reports whether the exit threshold was reached. A present home
// (or an indeterminate stat error) resets the counter — only an unbroken run
// of definite ENOENTs counts as a deletion.
func applyHomeCheck(homeDir string, misses int) (int, bool) {
	if _, err := os.Stat(homeDir); err == nil || !os.IsNotExist(err) {
		return 0, false
	}
	misses++
	return misses, misses >= homeMissingChecksToExit
}

// latchDaemonHomePresent records this daemon's AF home as observed-present, so
// nothing it writes can re-create the directory once it is deleted (#3845). It
// returns the release, which is always safe to call.
//
// Every state write went through an os.MkdirAll that creates the home as an
// ancestor of its target — instances.json on each status poll, tasks.json on
// each task write, events/, logs/, locks/ — and any one of them clears
// applyHomeCheck's consecutive-miss counter, so the daemon above never observes
// its own deletion. It is the write-side counterpart to the socket binds'
// requireDaemonHomePresent, armed HERE — after acquireHomeLock has created the
// home — for the reason #3843 gives: by this point the home has been created
// several times over on a legitimate start, so a home missing later never means
// "first start".
//
// The home is resolved ONCE — it is the same value watchDaemonHome is given — so
// the guard costs a stat per write rather than a GetConfigDir, and the two
// halves can never disagree about which directory this daemon's life depends on.
//
// Fail-OPEN. An unresolvable or unobservable home arms nothing and the daemon
// behaves exactly as it did before, because a refusal on a false premise stops a
// HEALTHY daemon's writes, which is worse than the abandoned one this prevents.
func latchDaemonHomePresent() func() {
	home, err := config.GetConfigDir()
	if err != nil {
		log.WarningLog.Printf("cannot resolve the agent-factory home for the write-side abandoned-daemon guard: %v", err)
		return func() {}
	}
	release, err := config.MarkAFHomePresent(home)
	if err != nil {
		log.WarningLog.Printf("cannot confirm the agent-factory home %s is present; state writes will re-create it if it is deleted: %v", home, err)
	}
	return release
}
