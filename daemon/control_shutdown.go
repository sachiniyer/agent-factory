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

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
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
// Returns (ShutdownNoDaemon, nil) when no daemon is running (no socket or
// ECONNREFUSED), (ShutdownViaRPC, nil) when the Shutdown RPC acknowledged,
// (ShutdownViaSIGTERM, nil) when the fallback signaled a real `af --daemon`
// process, (ShutdownFailed, err) when the daemon is provably running but
// the fallback could not locate or signal it (ambiguous pgrep matches, no
// PID file with pgrep unavailable, permission denied on signal) — the
// returned error carries the recovery hint the caller must surface — and
// (ShutdownError, err) when the socket was present but the Shutdown RPC
// failed with a transport error that is neither daemon-absent nor
// method-not-found (EACCES, ECONNRESET/EPIPE, dial timeout): a daemon was
// listening but its final state is unknown (#978).
//
// The int is the PID of the daemon that was stopped, or 0 when there was none
// or it could not be identified. It is the positive handle
// WaitForShutdownCompletion waits on (#5007): the PID the Shutdown reply
// reports, else the PID file read BEFORE the RPC (the daemon removes that file
// during teardown), else — on the SIGTERM path — the PID actually signaled.
func RequestShutdown() (ShutdownResult, int, error) {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return ShutdownNoDaemon, 0, err
	}
	if _, statErr := os.Stat(socketPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return ShutdownNoDaemon, 0, nil
		}
		return ShutdownNoDaemon, 0, statErr
	}
	// Captured before the RPC: while the old daemon is up the PID file names the
	// lock holder, and teardown removes it.
	pidFilePID, _ := readPIDFromFile()
	var resp ShutdownResponse
	if rpcErr := callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp); rpcErr != nil {
		if isDaemonAbsentErr(rpcErr) {
			return ShutdownNoDaemon, 0, nil
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
		return ShutdownError, 0, rpcErr
	}
	if !resp.OK {
		return ShutdownNoDaemon, 0, fmt.Errorf("daemon Shutdown RPC returned OK=false")
	}
	if resp.PID != 0 {
		return ShutdownViaRPC, resp.PID, nil
	}
	return ShutdownViaRPC, pidFilePID, nil
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

// shutdownCompleteGrace bounds WaitForShutdownCompletion; shutdownCompletePoll
// is the cadence. The mechanism is the exit signal — the stopped daemon's PID
// going away, or, without a PID, its control socket going quiet. The bound only
// stops a wedged teardown from hanging the caller forever, so it is generous: a
// busy daemon's teardown (watchers, store flush, dozens of tmux clients) has
// outlasted a 5s budget in production (#5007). Package vars rather than
// constants so tests can shorten the timeout path, mirroring
// stopDaemonGrace/stopDaemonPoll. The poll is tighter than sigtermFallbackPoll
// because the normal RPC teardown completes just past shutdownAckGrace (50ms),
// so a 50ms cadence usually resolves the wait on its first or second check.
var (
	shutdownCompleteGrace = 60 * time.Second
	shutdownCompletePoll  = shutdownAckGrace
)

// shutdownKillConfirmGrace bounds how long WaitForShutdownCompletion waits for a
// SIGKILLed daemon to disappear from the process table.
const shutdownKillConfirmGrace = 2 * time.Second

// WaitForShutdownCompletion blocks until the daemon a Shutdown was sent to has
// actually exited. The Shutdown RPC acknowledges before the daemon tears down
// (shutdownAckGrace plus the teardown tail), so a caller that respawns
// immediately after RequestShutdown races the dying daemon: EnsureDaemon's
// liveness ping — or a unit-restarted daemon's startup ping guard — can see the
// old socket still answering, skip the spawn, and leave nothing running once
// the old daemon exits (#854). Callers on the shutdown-then-respawn path must
// not respawn until this returns.
//
// pid is RequestShutdown's second return. When it is positive the wait is on
// that process exiting — a positive signal, not a socket that happens to have
// stopped answering. If the PID stops naming an af daemon while still alive (a
// recycled number, or a handle that never was the daemon), the process watch
// has nothing more to say and the wait falls back to socket-drain polling for
// the rest of the same bound. pid == 0 polls the socket alone.
//
// At shutdownCompleteGrace a daemon that acknowledged Shutdown but is still
// alive is wedged: it is SIGKILLed, after re-verifying it is an af daemon of
// this uid serving this AF home, and the kill is confirmed. So the old
// 5s-then-respawn-anyway hole (#5007) is closed — a daemon still answering at
// the bound means escalation or an error, never a blind respawn beside it.
// Returns an error when the daemon could not be confirmed gone; the caller
// should warn and proceed, and the respawn's own startup checks arbitrate.
func WaitForShutdownCompletion(pid int) error {
	if pid == os.Getpid() {
		pid = 0 // never watch, let alone signal, ourselves
	}
	deadline := time.Now().Add(shutdownCompleteGrace)
	if pid > 0 {
		for time.Now().Before(deadline) {
			if !pidLooksAlive(pid) {
				return nil
			}
			if !isAgentFactoryDaemon(pid) {
				pid = 0
				break
			}
			time.Sleep(shutdownCompletePoll)
		}
		if pid > 0 {
			return killWedgedShutdownDaemon(pid)
		}
	}
	for time.Now().Before(deadline) {
		if pingDaemon() != nil {
			return nil
		}
		time.Sleep(shutdownCompletePoll)
	}
	return fmt.Errorf("daemon control socket still answering %s after shutdown was acknowledged", shutdownCompleteGrace)
}

// killWedgedShutdownDaemon escalates a daemon that acknowledged Shutdown but did
// not exit within shutdownCompleteGrace, the way the SIGTERM fallbacks escalate.
// A PID is a reusable handle, so it is re-verified as an af daemon of this uid
// serving this AF home immediately before the signal; anything else is refused.
func killWedgedShutdownDaemon(pid int) error {
	if !pidLooksAlive(pid) {
		return nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("daemon pid %d did not exit %s after shutdown was acknowledged, and the AF home could not be resolved to verify it before escalating: %w", pid, shutdownCompleteGrace, err)
	}
	wantHome, err := canonicalDir(dir)
	if err != nil {
		return fmt.Errorf("daemon pid %d did not exit %s after shutdown was acknowledged, and the AF home could not be resolved to verify it before escalating: %w", pid, shutdownCompleteGrace, err)
	}
	if scope := verifyScopedDaemon(pid, os.Getuid(), wantHome); scope != daemonOurs {
		if !pidLooksAlive(pid) {
			return nil
		}
		return fmt.Errorf("daemon pid %d did not exit %s after shutdown was acknowledged and could not be verified as this home's daemon; not signaling it", pid, shutdownCompleteGrace)
	}
	log.WarningLog.Printf("daemon pid %d acknowledged shutdown but did not exit within %s; escalating to SIGKILL", pid, shutdownCompleteGrace)
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("FindProcess %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil && !errIsProcessGone(err) {
		return fmt.Errorf("SIGKILL wedged daemon pid %d: %w", pid, err)
	}
	confirm := time.Now().Add(shutdownKillConfirmGrace)
	for time.Now().Before(confirm) {
		if !pidLooksAlive(pid) {
			return nil
		}
		time.Sleep(sigtermFallbackPoll)
	}
	return fmt.Errorf("daemon pid %d survived SIGKILL %s after shutdown was acknowledged", pid, shutdownKillConfirmGrace)
}

// daemonAlreadyServing is RunDaemon's startup liveness guard. Only a responder
// that is NOT quiescing counts as already serving: one reporting
// DaemonPhaseQuiescing acknowledged a Shutdown and is leaving (#5007), so
// exiting on its answer would leave no daemon once it finishes. For that
// responder it waits (bounded) for the exit and reports false; the per-home
// lock the caller takes next arbitrates a drain that outlived the bound — a
// held lock is a non-zero exit, which the unit's Restart=on-failure retries.
func daemonAlreadyServing() bool {
	resp, err := pingDaemonResponse()
	if err != nil {
		return false
	}
	if resp.Phase != DaemonPhaseQuiescing {
		return true
	}
	log.InfoLog.Printf("daemon pid %d on the control socket is draining after shutdown; waiting for it to exit", resp.PID)
	waitForDrainingDaemonExit(resp.PID, time.Now().Add(shutdownCompleteGrace))
	return false
}

// waitForDrainingDaemonExit is a best-effort wait for a responder that reported
// DaemonPhaseQuiescing to finish leaving, bounded by deadline. With a PID it
// waits for that process to exit (or to stop naming an af daemon); without one
// it waits for the control socket to stop answering. It never errors: callers
// fall through afterwards, and the per-home lock and socket liveness arbitrate
// a drain that outlived the bound.
func waitForDrainingDaemonExit(pid int, deadline time.Time) {
	if pid == os.Getpid() {
		pid = 0
	}
	for time.Now().Before(deadline) {
		if pid > 0 {
			if !pidLooksAlive(pid) || !isAgentFactoryDaemon(pid) {
				return
			}
		} else if pingDaemon() != nil {
			return
		}
		time.Sleep(shutdownCompletePoll)
	}
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
