package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// HealthStatus is a read-only snapshot of daemon liveness for `af doctor`.
// Collecting it never spawns a daemon and never signals anything.
type HealthStatus struct {
	// SocketPath is the control socket location; empty when the config dir
	// cannot be resolved (SocketErr says why).
	SocketPath string
	SocketErr  error
	// SocketExists reports whether the socket file is present on disk.
	SocketExists bool
	// PingErr is nil when a daemon answered the control-socket ping.
	PingErr error
	// DaemonVersion is the af version the responding daemon reported. It is
	// empty when nothing answered (PingErr != nil), and — importantly — also
	// when a daemon answered but predates version reporting. Read it together
	// with PingErr: answered-but-empty means the daemon is older than any
	// client that can ask, which is exactly the skew that makes a newer
	// client's requests fail with "unknown field <name>" (#1044).
	DaemonVersion string
	// AutostartUnit reports whether the supervised autostart unit (systemd
	// user service / launchd agent) is installed — i.e. whether a running
	// daemon is expected to be unit-managed rather than an ad-hoc child.
	AutostartUnit bool
	// PIDFilePID is the PID recorded in daemon.pid, 0 when absent/unreadable.
	PIDFilePID int
	// PIDVerified reports whether PIDFilePID is a live process whose
	// cmdline identifies an agent-factory daemon (the #1004 guard against
	// recycled PIDs).
	PIDVerified bool
	// BinaryDeleted reports whether the verified daemon process is
	// executing a binary that has since been deleted or replaced on disk
	// (/proc/<pid>/exe ends in " (deleted)") — i.e. an install happened and
	// the daemon has not been restarted onto the new binary.
	BinaryDeleted bool
}

// Health collects a HealthStatus. Best-effort on every axis: unavailable
// facts (no /proc, unreadable pid file) simply leave their fields zeroed.
func Health() HealthStatus {
	var h HealthStatus
	h.SocketPath, h.SocketErr = DaemonSocketPath()
	if h.SocketPath != "" {
		if _, err := os.Stat(h.SocketPath); err == nil {
			h.SocketExists = true
		}
	}
	ping, pingErr := pingDaemonResponse()
	h.PingErr = pingErr
	if pingErr == nil {
		h.DaemonVersion = ping.Version
	}
	h.AutostartUnit = AutostartInstalled()

	pidPath, err := daemonPIDFilePath()
	if err == nil {
		if data, err := os.ReadFile(pidPath); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				h.PIDFilePID = pid
			}
		}
	}
	if h.PIDFilePID > 0 && pidLooksAlive(h.PIDFilePID) && isAgentFactoryDaemon(h.PIDFilePID) {
		h.PIDVerified = true
		// Linux-only freshness probe; on other platforms Readlink fails and
		// the field stays false.
		if target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", h.PIDFilePID)); err == nil {
			h.BinaryDeleted = strings.HasSuffix(target, " (deleted)")
		}
	}
	return h
}

// DaemonSocketNames returns the file names of the Unix sockets a daemon binds
// inside an agent-factory home. Exported so `af doctor` can look for sockets
// left behind in a home it was pointed at, rather than only the active one,
// and so its idea of "a daemon socket" cannot drift from the daemon's own.
//
// Names, not paths: a caller joins them onto the home it is inspecting. Callers
// must also verify the entry really is a socket before acting on it — a name
// alone proves nothing about the file (see isAbandonedVSCodeSocket).
func DaemonSocketNames() []string {
	return []string{daemonSocketFileName, daemonHTTPSocketFileName}
}

// LooksLikeDaemonArgv reports whether argv names an agent-factory daemon
// process (an `af`/`agent-factory` binary carrying a discrete --daemon flag).
// It takes real argv elements (boundaries preserved) so a daemon installed
// under a path containing spaces is classified correctly (#1214). Exported for
// `af doctor`'s host-wide scan so its matching stays in lockstep with the
// daemon's own PID-validation rules (#1004).
func LooksLikeDaemonArgv(args []string) bool {
	return argsHaveDaemonFlag(args) && argsAreDaemonBinary(args)
}

// ProcessArgv returns pid's argv using the daemon package's cross-platform
// lookup: /proc where available, then a best-effort `ps` fallback for macOS
// and other Unix platforms without /proc.
func ProcessArgv(pid int) []string {
	return daemonArgs(pid)
}

// PIDLooksAlive reports whether pid still appears to name a live process,
// using the same zombie-aware liveness probe as daemon shutdown paths.
func PIDLooksAlive(pid int) bool {
	return pidLooksAlive(pid)
}
