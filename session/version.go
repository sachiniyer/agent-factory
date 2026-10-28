package session

import "sync/atomic"

// daemonVersion is the af build version of this process, recorded once via
// SetDaemonVersion at startup. It is the version a remote host's staged `af`
// binary must equal (#5571 W2.2): the agent-server a host launches is the
// daemon's own build, and a version mismatch means the wire contract between
// them may differ.
//
// Stored atomically: SetDaemonVersion runs once on the main goroutine before
// the daemon accepts work, but tests and later readers must not race it.
var daemonVersion atomic.Value // string

// SetDaemonVersion records the af build version for this process. It mirrors
// daemon.SetVersion — session cannot import daemon (a cycle), so the version
// is planted twice, once per package that needs it.
func SetDaemonVersion(v string) { daemonVersion.Store(v) }

// DaemonVersion returns the version recorded by SetDaemonVersion, or "" when
// it was never set — a state the remote-binary selector treats as "no known
// version to match" rather than a guess.
func DaemonVersion() string {
	if v := daemonVersion.Load(); v != nil {
		return v.(string)
	}
	return ""
}
