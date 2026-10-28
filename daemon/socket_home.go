package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// requireDaemonHomePresent refuses to bind a daemon socket into an AF home that
// no longer exists, instead of creating the directory back (#3842).
//
// Both daemon sockets resolve as filepath.Join(config.GetConfigDir(), <name>)
// — see DaemonSocketPath and DaemonHTTPSocketPath — so the socket's directory
// IS the AF home, and the os.MkdirAll that used to sit at each bind site was
// creating the home, not some socket subdirectory. (If a socket ever moves into
// a subdirectory of the home, that subdirectory needs its own MkdirAll here,
// under a home this function has already found present.)
//
// By the time either socket binds, the home has been created several times
// over on a legitimate start: log.Initialize writes the daemon log into it,
// config.LoadConfig resolves it, and acquireHomeLock MkdirAll's it a few lines
// earlier in RunDaemon. A home that is missing HERE therefore never means "first
// start"; it means the home was deleted out from under a daemon that is already
// up — an abandoned temp/test home, or a user removing the install. That is
// exactly the signal watchDaemonHome self-terminates on (#1093/#1094), and
// re-creating the directory is what defeated it: the watcher clears its
// consecutive-miss counter on any successful stat, so a daemon that resurrects
// its own home never observes the deletion and keeps firing schedules forever.
// It left 108 dead /tmp/af-* homes on the maintainer's box holding nothing but
// the daemon-http.sock the resurrecting bind had just created.
//
// Only a POSITIVE observation of absence refuses. An inconclusive stat (EACCES,
// EIO) leaves the home's fate unknown, so the bind proceeds and net.Listen
// reports whatever the kernel actually says — refusing on "I could not tell"
// would take down a healthy daemon on the strength of a transient error.
func requireDaemonHomePresent(what, socketPath string) error {
	home := filepath.Dir(socketPath)
	if _, err := os.Stat(home); err != nil && os.IsNotExist(err) {
		return fmt.Errorf("cannot bind the %s: the agent-factory home %s was removed after this daemon started, "+
			"so this daemon is abandoned; refusing to re-create the home (it will shut itself down on its next home check)",
			what, home)
	}
	return nil
}
