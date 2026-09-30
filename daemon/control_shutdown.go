package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"net/rpc"
	"os"
	"strings"
	"syscall"
	"time"
)

// ShutdownResult reports how RequestShutdown stopped (or failed to stop) the
// running daemon. Used by upgrade.go and autoupdate.go to pick the right
// user-facing message after a binary swap.
type ShutdownResult int

const (
	// ShutdownNoDaemon means no daemon was running (no socket, ECONNREFUSED,
	// or PID-file scan found nothing). The upgrade prints bare success.
	ShutdownNoDaemon ShutdownResult = iota
	// ShutdownViaRPC means the daemon acknowledged the Shutdown RPC and is
	// exiting cleanly. The post-#501 happy path.
	ShutdownViaRPC
	// ShutdownViaSIGTERM means the daemon was a pre-#501 binary that did not
	// register the Shutdown RPC, so we located its PID and signaled it
	// directly. The upgrade prints a slightly different success message so
	// users know we used the fallback. See #504.
	ShutdownViaSIGTERM
	// ShutdownFailed means a daemon was proven to be listening on the
	// control socket (the Shutdown RPC came back as method-not-found, not
	// ECONNREFUSED) but the SIGTERM fallback could not locate a PID to
	// signal — e.g. no PID file AND pgrep is unavailable on this host. The
	// daemon is still running the old binary; the caller must surface the
	// recovery hint in the accompanying error. See #553.
	ShutdownFailed
	// ShutdownError means the control socket was present and a Shutdown RPC
	// was attempted, but it failed with an error that does NOT prove the
	// daemon absent and is NOT method-not-found: EACCES (socket exists but
	// the caller lacks permission to connect), ECONNRESET/EPIPE (the
	// connection was established then reset), or a dial timeout (the socket
	// is bound but the listener is unresponsive). All of these imply a daemon
	// WAS listening, so reporting ShutdownNoDaemon — documented as "no daemon
	// was running" — would mislabel the outcome. The daemon's final state is
	// unknown and it may still be running; the accompanying error carries the
	// detail. See #978.
	ShutdownError
)

// sigtermFallbackGrace is the max time we wait for a SIGTERM'd daemon to exit
// before escalating to SIGKILL.
const sigtermFallbackGrace = 5 * time.Second

// sigtermFallbackPoll is how often we check whether the SIGTERM'd daemon has
// exited.
const sigtermFallbackPoll = 100 * time.Millisecond

// RequestShutdown asks any running daemon to exit cleanly. The normal path
// uses the Shutdown RPC (#498/#501). When the running daemon is a pre-#501
// binary that does not register Shutdown, we fall back to locating the
// daemon's PID and sending SIGTERM directly (#504) so an `af upgrade` does
// not leave a stale daemon running the old binary.
//
// Returns (ShutdownNoDaemon, {}, nil) when no daemon is running (no socket or
// ECONNREFUSED), (ShutdownViaRPC, target, nil) when the Shutdown RPC acknowledged,
// (ShutdownViaSIGTERM, target, nil) when the fallback signaled a real `af --daemon`
// process, (ShutdownFailed, {}, err) when the daemon is provably running but
// the fallback could not locate or signal it (ambiguous pgrep matches, no
// PID file with pgrep unavailable, permission denied on signal) — the
// returned error carries the recovery hint the caller must surface — and
// (ShutdownError, {}, err) when the socket was present but the Shutdown RPC
// failed with a transport error that is neither daemon-absent nor
// method-not-found (EACCES, ECONNRESET/EPIPE, dial timeout): a daemon was
// listening but its final state is unknown (#978).
//
// The ShutdownTarget names the process being stopped (zero when unknown). On
// the RPC path its PID is the acknowledging daemon's own, falling back to the
// PID a pre-shutdown Ping reported for daemons built before ShutdownResponse
// carried one; callers pass it to WaitForShutdownCompletion so the respawn
// waits for that exact process to exit rather than for its socket to go quiet
// (#5007).
func RequestShutdown() (ShutdownResult, ShutdownTarget, error) {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return ShutdownNoDaemon, ShutdownTarget{}, err
	}
	if _, statErr := os.Stat(socketPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return ShutdownNoDaemon, ShutdownTarget{}, nil
		}
		return ShutdownNoDaemon, ShutdownTarget{}, statErr
	}
	// Capture the target's PID before asking it to stop: once it acknowledges it
	// may stop answering, and a daemon predating ShutdownResponse.PID reports
	// none. Bounded so a wedged responder cannot stall the upgrade, and its error
	// is ignored — a failed ping only means the PID is unknown (0), and the
	// Shutdown RPC below is the authority on whether a daemon is there at all.
	var pingResp PingResponse
	_ = callDaemonNoEnsureBefore("Ping", PingRequest{}, &pingResp, time.Now().Add(daemonDialTimeout), true)
	// Pin the pinged process's incarnation now, while it is certainly still the
	// daemon: for a daemon whose ack carries no PID this is the only sample
	// taken before it could exit and have its PID recycled.
	var pingToken string
	if pingResp.PID > 0 {
		pingToken = processStartTokenFn(pingResp.PID)
	}
	var resp ShutdownResponse
	if rpcErr := callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp); rpcErr != nil {
		if isDaemonAbsentErr(rpcErr) {
			return ShutdownNoDaemon, ShutdownTarget{}, nil
		}
		if isRPCMethodNotFoundErr(rpcErr) {
			// Daemon is alive on the socket but does not speak Shutdown
			// (pre-#501 binary). Fall through to the PID-based fallback.
			return sigtermFallback()
		}
		// The socket was present (os.Stat above succeeded) and the error is
		// neither daemon-absent (ECONNREFUSED/ENOENT) nor method-not-found:
		// EACCES, ECONNRESET/EPIPE, or a dial timeout. Something was listening,
		// so ShutdownNoDaemon would mislabel this — report the ambiguous
		// contacted-but-errored outcome instead (#978).
		return ShutdownError, ShutdownTarget{}, rpcErr
	}
	if !resp.OK {
		return ShutdownNoDaemon, ShutdownTarget{}, fmt.Errorf("daemon Shutdown RPC returned OK=false")
	}
	// The Shutdown ack's PID wins; the Ping PID covers daemons built before the
	// ack carried one.
	pid := resp.PID
	if pid == 0 {
		pid = pingResp.PID
	}
	// An ack PID comes from a daemon that was alive to send it, so sample it
	// now; a PID taken from the Ping reuses the pre-shutdown sample.
	tok := pingToken
	if pid != pingResp.PID {
		tok = processStartTokenFn(pid)
	}
	return ShutdownViaRPC, ShutdownTarget{PID: pid, StartToken: tok}, nil
}

// ClassifyShutdownTarget turns the read-only ping made before a restart into
// the exact presence answer RequestShutdown will rely on. Only the two kernel
// answers that RequestShutdown already treats as daemon-absent — ENOENT and
// ECONNREFUSED — become No. Permission errors, resets, timeouts, and every
// other failure remain Undetermined, so a failed observation cannot authorize
// a supposedly harmless no-op before restart-safety checks run.
func ClassifyShutdownTarget(pingErr error) ProbeAnswer {
	if pingErr == nil {
		return AnswerYes()
	}
	if isDaemonAbsentErr(pingErr) {
		return AnswerNo()
	}
	return Undetermined(fmt.Errorf("cannot determine whether a daemon is available to shut down: %w", pingErr))
}

// ShutdownTarget identifies the daemon process a shutdown was sent to. The zero
// value means the process is unknown, and WaitForShutdownCompletion then falls
// back to waiting for the control socket to go quiet.
type ShutdownTarget struct {
	// PID is the target's process id, 0 when unknown.
	PID int
	// StartToken pins the process incarnation at the moment the PID was
	// learned (see processStartToken), so a PID recycled before or during the
	// wait still reads as the daemon's exit. Empty when unobservable.
	StartToken string
}

// ErrShutdownIncomplete reports that a daemon acknowledged Shutdown but had not
// finished tearing down when WaitForShutdownCompletion's bound expired. The
// daemon is still alive, so respawning would race it (#5007); callers withhold
// the respawn and surface the condition instead.
var ErrShutdownIncomplete = errors.New("daemon shutdown acknowledged but not finished")

// shutdownCompleteGrace bounds how long WaitForShutdownCompletion waits for a
// known daemon PID to exit; shutdownCompletePoll is the cadence. The grace is
// generous because a daemon drains durable work after acknowledging Shutdown,
// and waiting on its actual exit is the only signal that cannot mistake a
// still-draining daemon for a gone one (#5007). shutdownSocketQuietGrace bounds
// the no-PID fallback, which keeps the socket-quiet wait and its 5s bound —
// sigtermFallbackGrace, the wait signalAndWait already imposes on the SIGTERM
// path. The poll is tighter than sigtermFallbackPoll because the normal RPC
// teardown can complete just past shutdownAckGrace (50ms). Package vars rather
// than constants so tests can shorten the timeout paths, mirroring
// stopDaemonGrace/stopDaemonPoll.
var (
	shutdownCompleteGrace    = 60 * time.Second
	shutdownSocketQuietGrace = sigtermFallbackGrace
	shutdownCompletePoll     = shutdownAckGrace
)

// WaitForShutdownCompletion blocks until the daemon that acknowledged Shutdown
// has finished tearing down. The Shutdown RPC acknowledges before the daemon
// tears down (shutdownAckGrace plus the drain), so a caller that respawns
// immediately after RequestShutdown races the dying daemon: EnsureDaemon's
// liveness ping — or a unit-restarted daemon's startup ping guard — can see
// the old daemon still answering, skip the spawn, and leave nothing running
// once it exits (#854, #5007). Callers on the shutdown-then-respawn path must
// wait for this to return nil before respawning.
//
// With target.PID > 0 (the target RequestShutdown returned) it waits for that
// process to exit, bounded by shutdownCompleteGrace. Process exit is a positive signal;
// socket quietness is not — a draining daemon can stop answering pings well
// before it releases what a successor needs. Renamed or relocated binaries are
// irrelevant here because nothing checks the process name, only liveness and
// start time — a PID recycled to another process counts as the daemon's exit. On
// the SIGTERM fallback path the process is already gone, so the first check
// returns. With target.PID == 0 (unknown) it falls back to waiting for the control
// socket to stop answering, bounded by shutdownSocketQuietGrace.
//
// It only observes: it never signals the process. A daemon still draining at
// the bound is left alone, and the returned error wraps ErrShutdownIncomplete
// so the caller can withhold the respawn and tell the user.
func WaitForShutdownCompletion(target ShutdownTarget) error {
	if pid := target.PID; pid > 0 {
		// The start-time token, taken when the PID was learned, tells a recycled
		// PID from the daemon: if the daemon exits and its PID is reused, liveness
		// alone would wait out the grace and the hint would name an unrelated
		// process. "" (unobservable) falls back to liveness only, and a later read
		// that fails is not a change: a failed read must not fabricate an exit.
		token := target.StartToken
		gone := func() bool {
			if !pidLooksAlive(pid) {
				return true
			}
			cur := processStartTokenFn(pid)
			return token != "" && cur != "" && cur != token
		}
		deadline := time.Now().Add(shutdownCompleteGrace)
		for time.Now().Before(deadline) {
			if gone() {
				return nil
			}
			time.Sleep(shutdownCompletePoll)
		}
		// The process may have exited between the last in-loop check and the
		// deadline; do not report a daemon that is already gone as still running.
		if gone() {
			return nil
		}
		return fmt.Errorf("%w: daemon pid %d still running %s after shutdown was acknowledged (it may still be draining durable work)", ErrShutdownIncomplete, pid, shutdownCompleteGrace)
	}
	deadline := time.Now().Add(shutdownSocketQuietGrace)
	for time.Now().Before(deadline) {
		if pingDaemon() != nil {
			return nil
		}
		time.Sleep(shutdownCompletePoll)
	}
	return fmt.Errorf("%w: daemon control socket still answering %s after shutdown was acknowledged", ErrShutdownIncomplete, shutdownSocketQuietGrace)
}

// isDaemonAbsentErr reports whether err from a dial/RPC call indicates that
// no daemon is currently listening on the control socket (vs. some other
// transport failure). Both ECONNREFUSED (stale socket, no listener) and
// ENOENT (socket removed between Stat and Dial) qualify. Application-level
// RPC errors (method-not-found, server panic) do NOT — those are handled
// separately by isRPCMethodNotFoundErr so we can route them to the SIGTERM
// fallback rather than treating them as "no daemon" and silently leaving the
// stale process running (#504).
func isDaemonAbsentErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return false
}

// isRPCMethodNotFoundErr reports whether err is the net/rpc server's reply
// for an unknown method or service. The connection succeeded (daemon is
// running, control socket is alive) but the registered service did not have
// the requested method — i.e. a pre-#501 daemon that never registered
// "Control.Shutdown". The stdlib returns this as rpc.ServerError with the
// literal prefix "rpc: can't find method " or "rpc: can't find service ".
func isRPCMethodNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var serverErr rpc.ServerError
	if !errors.As(err, &serverErr) {
		return false
	}
	s := string(serverErr)
	return strings.Contains(s, "can't find method") || strings.Contains(s, "can't find service")
}
