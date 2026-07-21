package daemon

import (
	"errors"
	"fmt"
	"net"
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
	// BootID, TransactionID, Phase, and Listeners are the responding daemon's
	// additive Ping health surface. Empty phase/IDs mean the responder predates
	// these fields; read them only when PingErr is nil.
	BootID        string
	TransactionID string
	Phase         DaemonPhase
	Listeners     DaemonListenerStatus
	// ServingPID and BootConfig come from the process that answered Ping. They
	// are authoritative for supervision/config-skew diagnosis; PIDFilePID and a
	// fresh config read cannot say which process answered or what it booted with.
	// Zero/nil with PingErr == nil means the responder predates these fields.
	ServingPID int
	BootConfig *DaemonBootConfig
	// HTTPSocketPath is the daemon's HTTP/JSON socket location.
	HTTPSocketPath string
	// HTTPSocketExists reports whether HTTPSocketPath is present on disk.
	HTTPSocketExists bool
	// HTTPListening is whether anything accepts connections on the HTTP socket.
	//
	// A ProbeAnswer, not an error, because `err == nil` meant BOTH "the dial
	// succeeded" and "nobody dialed" — the same ambiguity that let a two-valued
	// probe fabricate answers, one field over. The zero value is Undetermined,
	// so a caller that never probed cannot report health it did not observe.
	//
	// It is a SEPARATE listener from the control socket, and RunDaemon treats a
	// failed startHTTPServer as non-fatal — so a daemon can answer the control
	// socket perfectly while the HTTP socket, which the TUI and every HTTP/web
	// client dial, is stale or absent. A healthy Ping says nothing about this
	// (#1044).
	HTTPListening ProbeAnswer
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
		h.BootID = ping.BootID
		h.TransactionID = ping.TransactionID
		h.Phase = ping.Phase
		h.Listeners = ping.Listeners
		h.ServingPID = ping.PID
		h.BootConfig = ping.BootConfig
	}
	h.HTTPSocketPath, h.HTTPSocketExists, h.HTTPListening = probeHTTPSocket()
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

// dialUnix dials a Unix socket with a bounded timeout. It is a package var so a
// test can substitute a deterministic dial outcome (a synthetic timeout or a
// refusal) instead of manufacturing one from the OS: kernel accept-backlog
// semantics differ across platforms, so a real saturated-backlog timeout is not
// portably reproducible (Darwin completes handshakes past the nominal backlog
// and never saturates — #2039). Production wires the real net.DialTimeout.
var dialUnix = net.DialTimeout

// probeHTTPSocket reports the HTTP socket's path, whether it exists, and
// whether anything is accepting connections on it.
//
// A dial, not a request: it distinguishes the failure that matters — a socket
// file with no listener behind it, where clients connect and wait — from a
// healthy listener, without needing a token, a route, or a response body.
// Bounded by daemonDialTimeout so a wedged listener cannot stall `af doctor`.
func probeHTTPSocket() (path string, exists bool, listening ProbeAnswer) {
	path, err := DaemonHTTPSocketPath()
	if err != nil || path == "" {
		return "", false, Undetermined(fmt.Errorf("cannot resolve the HTTP socket path: %w", err))
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			// Nothing to listen on: a definite answer, not a failure to look.
			return path, false, AnswerNo()
		}
		return path, false, Undetermined(fmt.Errorf("cannot stat %s: %w", path, err))
	}
	conn, err := dialUnix("unix", path, daemonDialTimeout)
	if err != nil {
		return path, true, classifyDialFailure(path, err)
	}
	_ = conn.Close()
	return path, true, AnswerYes()
}

// classifyDialFailure turns a FAILED dial of the socket into an answer about
// whether anything is listening. A dial error is not automatically a "no".
//
//   - A refusal (ECONNREFUSED) is a completed answer: the socket file is there
//     and the kernel had no listener to hand the connection to. Nobody is
//     behind it — a definite No.
//   - A deadline expiry is NOT an answer. A listener can be present with a
//     saturated accept backlog, so the connect waits and the 250ms bound fires
//     before it is serviced. Reading that timeout as No is the exact
//     timeout-is-not-a-negative fabrication #1920 set out to kill, one field
//     over (#2014): it would send a user to `af daemon restart` over a
//     live-but-busy listener. A timeout is Undetermined, never a made-up No.
func classifyDialFailure(path string, err error) ProbeAnswer {
	if os.IsTimeout(err) || errors.Is(err, os.ErrDeadlineExceeded) {
		return Undetermined(fmt.Errorf(
			"dialing %s did not complete within %s, so whether anything is listening is unknown "+
				"(a listener may be present with a saturated accept backlog): %w",
			path, daemonDialTimeout, err))
	}
	// The socket is there and refused us: nobody is behind it.
	return AnswerNo()
}

// ClassifyPingFailure turns a FAILED control-socket ping into a three-valued
// answer about daemon liveness, so `af doctor` never reads a dial TIMEOUT as a
// definite "the daemon is dead". It is the ping-path twin of classifyDialFailure
// on the HTTP path (#2014/#2039): the control socket has an accept backlog too,
// and under heavy TUI/CLI RPC load a dial can time out at daemonDialTimeout
// while a perfectly live daemon is merely busy.
//
//   - A nil error is Yes: a daemon answered.
//   - A timeout (os.IsTimeout / os.ErrDeadlineExceeded) is Undetermined: a
//     live-but-backlogged daemon times out the same way, so a made-up "dead"
//     would send the user to `af daemon restart` over a working daemon — the
//     exact timeout-is-not-a-negative fabrication #1920 set out to kill (#2040).
//   - Anything else — a refusal (ECONNREFUSED), a reset, an RPC-level error — is
//     a completed answer that nothing is responding: a definite No.
func ClassifyPingFailure(err error) ProbeAnswer {
	if err == nil {
		return AnswerYes()
	}
	if os.IsTimeout(err) || errors.Is(err, os.ErrDeadlineExceeded) {
		return Undetermined(fmt.Errorf(
			"the control socket did not answer within %s, so daemon liveness is unknown "+
				"(a live daemon may be busy with a saturated accept backlog): %w", daemonDialTimeout, err))
	}
	return AnswerNo()
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

// ControlSocketName is the file name of the control socket — the one Health
// pings. Exported so a caller enumerating DaemonSocketNames can tell which
// entry Health already speaks for and avoid reporting it twice.
func ControlSocketName() string { return daemonSocketFileName }

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

// PIDLooksAlive is GONE with its last caller. It was exported so `af doctor`
// could decide whether a temp home's daemon.pid named a live daemon — the
// heuristic PID validation #960 rejected, and the inference #1989 replaced with
// the per-home flock (ProbeHomeLock). Nothing outside this package needs to
// guess at daemon liveness any more: ask the lock. The internal pidLooksAlive
// remains for the shutdown paths that own a PID legitimately.
