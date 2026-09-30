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
// WaitForShutdownCompletion waits on (#5007), and a nonzero value is one the
// wait may trust without re-reading argv: the PID the daemon reported for
// itself in the Shutdown reply (trusted whatever its binary is named — a
// renamed install is legitimately not `af`); else, for a daemon predating that
// field, the PID it self-reported in a Ping sent just BEFORE the Shutdown
// (PingResponse.PID, same trust class; after the ack the socket may be gone);
// else the PID file read before the RPC (the daemon removes that file during
// teardown) but only when it named an af daemon at request time, since a stale
// file can name a recycled process; else — on the SIGTERM path — the PID
// actually signaled.
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
	// Also captured before the RPC: a daemon predating ShutdownResponse.PID still
	// names itself in Ping, which survives a renamed binary the PID-file check
	// rejects. A failed Ping leaves it 0 and changes nothing below.
	var pingPID int
	if ping, pingErr := pingDaemonResponse(); pingErr == nil {
		pingPID = ping.PID
	}
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
	// A reply predating ShutdownResponse.PID: its Ping self-report is trusted
	// like the field; the PID file is only a claim.
	if pingPID > 0 {
		return ShutdownViaRPC, pingPID, nil
	}
	if pidFilePID > 0 && isAgentFactoryDaemon(pidFilePID) {
		return ShutdownViaRPC, pidFilePID, nil
	}
	return ShutdownViaRPC, 0, nil
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
// stops a long or wedged teardown from hanging the caller forever (it reports,
// never signals — see WaitForShutdownCompletion), so it is generous: a
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

// ErrShutdownIncomplete is wrapped by WaitForShutdownCompletion's error at its
// bound: the daemon acknowledged Shutdown but has not finished tearing down. It
// is still alive — normally still joining durable work and about to exit on its
// own, though a wedged one may not — and nothing should be started beside it. Callers match it with errors.Is to report that state rather than "no
// daemon is running" (#5007).
var ErrShutdownIncomplete = errors.New("daemon shutdown acknowledged but not finished")

// ErrDaemonStillDraining is wrapped by EnsureDaemon's error when the daemon on
// the control socket reported DaemonPhaseQuiescing and was still there at the
// drain-wait deadline. It is finishing durable work (drainDaemon joins
// root-agent creates and admitted mutations, #3721), so it must not be stopped
// or killed to make room; the caller should retry shortly, once it has exited.
var ErrDaemonStillDraining = errors.New("daemon is still finishing its shutdown")

// WaitForShutdownCompletion blocks until the daemon a Shutdown was sent to has
// provably exited — the #5007 spec's draining→exited wait, shared by every
// consumer (see waitForDaemonExit for the proof). The Shutdown RPC acks before
// teardown, so shutdown-then-respawn callers must not respawn until this
// returns nil (#854). pid is RequestShutdown's second return, or 0.
//
// shutdownCompleteGrace is where it gives up with ErrShutdownIncomplete, never
// where it signals: drainDaemon JOINS root-agent creates and admitted mutations
// (#3721) with its socket already closed, so a still-alive daemon cannot be
// told apart from a wedged one and a kill could corrupt session state. Callers
// must not respawn on that error — the replacement would lose the home lock.
func WaitForShutdownCompletion(pid int) error {
	if waitForDaemonExit(pid, time.Now().Add(shutdownCompleteGrace)) {
		return nil
	}
	if pid > 0 && pid != os.Getpid() {
		return fmt.Errorf("%w: daemon pid %d still running %s after shutdown was acknowledged (it may still be draining durable work)", ErrShutdownIncomplete, pid, shutdownCompleteGrace)
	}
	return fmt.Errorf("%w: could not confirm within %s that the daemon released this home's lock (it may still be draining durable work)", ErrShutdownIncomplete, shutdownCompleteGrace)
}

// waitForDaemonExit is the one bounded draining→exited wait (#5007): true once
// the daemon is provably gone, false if not proven by deadline. It never
// signals. The proof is the established PID dying (never re-checked against
// argv: a renamed install is not `af`), else the home lock becoming takeable —
// the kernel releases it only at process exit, SIGKILL included. A quiet socket
// is never proof (drainDaemon closes it before its durable joins), nor is a
// missing daemon.pid (unlinked when teardown begins), nor an unprovable lock.
// The probe repeats after the loop: the last sleep can wake past the deadline.
func waitForDaemonExit(pid int, deadline time.Time) bool {
	if pid == os.Getpid() {
		pid = 0 // never watch ourselves
	}
	exited := func() bool {
		if pid > 0 {
			return !shutdownWaitPIDAliveFn(pid)
		}
		return homeLockState() == daemonExited
	}
	for time.Now().Before(deadline) {
		if exited() {
			return true
		}
		time.Sleep(shutdownCompletePoll)
	}
	return exited()
}

// shutdownWaitPIDAliveFn and shutdownWaitHomeLockFn are the exit probes. Vars
// only so tests can script when a daemon leaves; production never assigns them.
var (
	shutdownWaitPIDAliveFn = pidLooksAlive
	shutdownWaitHomeLockFn = homeLockReleased
)

// daemonState is the consumer's view of this home's daemon in the #5007
// shutdown state machine; pingState and probeDaemonState alone classify it.
type daemonState int

const (
	daemonServing  daemonState = iota // answers, not quiescing
	daemonDraining                    // acked Shutdown, or silent while holding the lock
	daemonExited                      // the home lock is takeable
	daemonUnknown                     // no probe established the above
)

// pingState classifies one Ping reply, with the PID the responder reported. It
// is the whole classification for waitForDaemonReady, which must not touch the
// home lock while a child is acquiring it.
func pingState(resp PingResponse, err error) (daemonState, int) {
	switch {
	case err != nil:
		return daemonUnknown, 0
	case resp.Phase == DaemonPhaseQuiescing:
		return daemonDraining, resp.PID
	default:
		return daemonServing, resp.PID
	}
}

// probeDaemonState is the one state probe (#5007): Ping, then the home lock
// when nothing answers. A daemon.pid still on disk means no teardown has begun
// (drainDaemon unlinks it first), so there a failed ping is unknown and the
// caller's usual recovery applies. Without one, a held lock is a daemon
// draining past its closed socket — or, for an instant, one between taking its
// lock and binding its socket, which callers recheck.
func probeDaemonState(deadline time.Time) (daemonState, int) {
	state, pid := pingState(pingDaemonResponseUntil(deadline))
	if state != daemonUnknown {
		return state, pid
	}
	if path, err := daemonPIDFilePath(); err != nil {
		return daemonUnknown, 0
	} else if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return daemonUnknown, 0
	}
	return homeLockState(), 0
}

// homeLockState reads this home's lock through shutdownWaitHomeLockFn; an
// unresolvable home is unknown.
func homeLockState() daemonState {
	dir, err := config.GetConfigDir()
	if err != nil {
		return daemonUnknown
	}
	return shutdownWaitHomeLockFn(dir)
}

// homeLockReleased asks what a replacement daemon's acquireHomeLock will ask:
// could this home's lock be taken right now? Takeable is exited, held
// (EWOULDBLOCK) is draining, and any other failure to look is unknown. A
// missing lock file is exited: a daemon that ever held the lock created the
// file first, and the replacement creates and takes it.
//
// Deliberately not ProbeHomeLock, which answers doctor's may-this-home-be-
// deleted question and so reads a missing file or an unrecognized filesystem
// as unknown — that would stall every PID-less wait on a home that never ran a
// daemon (af reset) or lives on NFS/FUSE. The probe holds the lock only for the
// instant between its flock and its unlock.
func homeLockReleased(dir string) daemonState {
	f, err := os.OpenFile(daemonLockPathIn(dir), os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return daemonExited
		}
		return daemonUnknown
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return daemonDraining
		}
		return daemonUnknown
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return daemonExited
}

// daemonAlreadyServing is RunDaemon's startup liveness guard. Only a serving
// daemon counts: a draining one is leaving (#5007), so exiting on its answer
// would leave no daemon once it finishes. For a draining one it waits (bounded)
// for the exit and reports false either way; the per-home lock the caller takes
// next arbitrates a drain that outlived the bound — a held lock is a non-zero
// exit, which the unit's Restart=on-failure retries. Unlike EnsureDaemon this
// never stops anything, so proceeding is safe here.
func daemonAlreadyServing() bool {
	state, pid := probeDaemonState(time.Time{})
	switch state {
	case daemonServing:
		return true
	case daemonDraining:
		log.InfoLog.Printf("the daemon for this home (pid %d, 0 if unknown) is draining after shutdown; waiting for it to exit", pid)
		if !waitForDaemonExit(pid, time.Now().Add(shutdownCompleteGrace)) {
			log.InfoLog.Printf("the daemon for this home is still draining at the bound; proceeding to home-lock arbitration")
		}
	}
	return false
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
