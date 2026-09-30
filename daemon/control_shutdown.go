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

// ShutdownPID is the stopped daemon's PID and its provenance (#5007). Only the
// PID a daemon reports in its own Shutdown ack is Confirmed: the acker names
// itself. Every other source — the Ping sent before the Shutdown, a verified
// PID file, the process the SIGTERM fallback signaled — proves a daemon for
// this home existed, not that it was the one that acked: Ping and Shutdown are
// separate connections, and an exit and rebind between them can swap
// responders. An unconfirmed PID is kept for reporting only; the exit wait
// uses the home-lock proof instead. PID 0 means none was identified.
type ShutdownPID struct {
	PID       int
	Confirmed bool
}

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
// The ShutdownPID names the daemon that was stopped, for WaitForShutdownCompletion
// and for reporting (#5007). See ShutdownPID for which sources are confirmed.
func RequestShutdown() (ShutdownResult, ShutdownPID, error) {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return ShutdownNoDaemon, ShutdownPID{}, err
	}
	if _, statErr := os.Stat(socketPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return ShutdownNoDaemon, ShutdownPID{}, nil
		}
		return ShutdownNoDaemon, ShutdownPID{}, statErr
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
			return ShutdownNoDaemon, ShutdownPID{}, nil
		}
		if isRPCMethodNotFoundErr(rpcErr) {
			// Daemon is alive on the socket but does not speak Shutdown
			// (pre-#501 binary). Fall through to the PID-based fallback.
			result, pid, err := sigtermFallback()
			return result, ShutdownPID{PID: pid}, err
		}
		// The socket was present (os.Stat above succeeded) and the error is
		// neither daemon-absent (ECONNREFUSED/ENOENT) nor method-not-found:
		// EACCES, ECONNRESET/EPIPE, or a dial timeout. Something was listening,
		// so ShutdownNoDaemon would mislabel this — report the ambiguous
		// contacted-but-errored outcome instead (#978).
		return ShutdownError, ShutdownPID{}, rpcErr
	}
	if !resp.OK {
		return ShutdownNoDaemon, ShutdownPID{}, fmt.Errorf("daemon Shutdown RPC returned OK=false")
	}
	if resp.PID != 0 {
		return ShutdownViaRPC, ShutdownPID{PID: resp.PID, Confirmed: true}, nil
	}
	// A reply predating ShutdownResponse.PID: the pre-Shutdown Ping's PID, else a
	// verified PID file, is advisory — reported, never waited on.
	if pingPID > 0 {
		return ShutdownViaRPC, ShutdownPID{PID: pingPID}, nil
	}
	if pidFilePID > 0 && isAgentFactoryDaemon(pidFilePID) {
		return ShutdownViaRPC, ShutdownPID{PID: pidFilePID}, nil
	}
	return ShutdownViaRPC, ShutdownPID{}, nil
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
// returns nil (#854). stopped is RequestShutdown's second return, or zero.
//
// shutdownCompleteGrace is where it gives up with ErrShutdownIncomplete, never
// where it signals: drainDaemon JOINS root-agent creates and admitted mutations
// (#3721) with its socket already closed, so a still-alive daemon cannot be
// told apart from a wedged one and a kill could corrupt session state. Callers
// must not respawn on that error — the replacement would lose the home lock.
func WaitForShutdownCompletion(stopped ShutdownPID) error {
	if waitForDaemonExit(stopped.PID, stopped.Confirmed, time.Now().Add(shutdownCompleteGrace)) {
		return nil
	}
	if stopped.PID > 0 {
		return fmt.Errorf("%w: daemon pid %d still running %s after shutdown was acknowledged (it may still be draining durable work)", ErrShutdownIncomplete, stopped.PID, shutdownCompleteGrace)
	}
	return fmt.Errorf("%w: could not confirm within %s that the daemon exited (it may still be draining durable work)", ErrShutdownIncomplete, shutdownCompleteGrace)
}

// waitForDaemonExit is the one bounded draining→exited wait (#5007): true once
// the daemon is provably gone, false if not proven by deadline. It never
// signals. With a confirmed PID (see ShutdownPID) the proof is that process
// dying — never re-checked against argv, since a renamed install is not `af`.
// Otherwise pid is advisory: never waited on, only used by exitState to tell
// the target from a different responder. A quiet
// socket is never proof for a lock-era daemon (drainDaemon closes it before its
// durable joins), nor is a missing daemon.pid (unlinked when teardown begins).
// The probe repeats after the loop: the last sleep can wake past the deadline.
func waitForDaemonExit(pid int, confirmed bool, deadline time.Time) bool {
	exited := func() bool {
		if confirmed && pid > 0 && pid != os.Getpid() {
			return !shutdownWaitPIDAliveFn(pid)
		}
		return exitState(pid, deadline) == daemonExited
	}
	for time.Now().Before(deadline) {
		if exited() {
			return true
		}
		time.Sleep(shutdownCompletePoll)
	}
	return exited()
}

// exitState is the post-ack exit proof without a confirmed PID (#5007 spec
// amendment and addenda 1–3); targetPID is the advisory PID of the daemon asked
// to stop, or 0. Only an unprovable lock settles it alone (unknown). Every other
// lock reading is consulted against Ping, because none proves exit by itself: a
// held lock may be a drainer past its socket close or a new daemon not yet
// bound, and a takeable or absent lock may sit beside a daemon that predates
// the lock — daemon.lock persists once created and such a daemon never flocks
// it.
//
// A serving answer proves exit only from a provably different process: a
// responder PID that is known and differs from a known target. The target
// itself can answer serving — a daemon predating quiescing-at-ack does so for
// its whole ack grace, and one never asked to stop does so indefinitely — so
// any other serving answer, like a quiescing one, is draining. With no answer,
// a held lock is draining; otherwise a quiet socket is exited unless a live
// daemon.pid PID still names the drainer (old-version daemons keep it until
// exit), and any other probe failure is unknown. A home that never ran a
// daemon reads exited.
func exitState(targetPID int, deadline time.Time) daemonState {
	dir, err := config.GetConfigDir()
	if err != nil {
		return daemonUnknown
	}
	lock := shutdownWaitHomeLockFn(dir)
	if lock == daemonUnknown {
		return daemonUnknown
	}
	held := lock == daemonDraining
	resp, err := pingDaemonResponseUntil(boundedPingDeadline(deadline))
	state, respPID := pingState(resp, err)
	switch {
	case state == daemonServing && respPID > 0 && targetPID > 0 && respPID != targetPID:
		return daemonExited
	case state != daemonUnknown, held:
		return daemonDraining
	case isDaemonAbsentErr(err):
		if pid := livePIDFilePID(); pid > 0 {
			return daemonDraining
		}
		return daemonExited
	default:
		return daemonUnknown
	}
}

// livePIDFilePID returns the PID daemon.pid names while that process is still
// alive, else 0. Old-version daemons keep the file until process exit (the
// early unlink is #5007's behavior), and pre-lock daemons never take
// daemon.lock — so on a quiet socket a live pidfile PID is a drainer either
// way. A stale file naming a dead or recycled PID reads 0, never proof of a
// drainer — but a live PID there means keep waiting, never signal.
func livePIDFilePID() int {
	pid, ok := readPIDFromFile()
	if !ok || pid <= 0 || pid == os.Getpid() {
		return 0
	}
	if !shutdownWaitPIDAliveFn(pid) {
		return 0
	}
	return pid
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

// boundedPingDeadline gives one classification probe a dial-timeout-sized
// share of deadline: a responder that accepts the dial but never replies
// costs one probe — never the caller's whole budget. A nearer deadline
// tightens it; a passed one (the post-loop recheck) still gets a genuine
// probe, and no deadline gets the same fixed bound.
func boundedPingDeadline(deadline time.Time) time.Time {
	pingDeadline := time.Now().Add(daemonDialTimeout)
	if remaining := time.Until(deadline); remaining > 0 && remaining < daemonDialTimeout {
		pingDeadline = deadline
	}
	return pingDeadline
}

// probeDaemonState is the one state probe (#5007): Ping, then the home lock,
// then daemon.pid when nothing answers. A held lock is a daemon — draining
// past its socket close or, for an instant, between taking the lock and
// binding — whatever daemon.pid says: old-version drainers keep the file until
// exit, so it must never override a held lock (stopDaemonUntil would SIGKILL
// that drainer). Only with the lock takeable or absent does the pidfile speak:
// a live PID it names is a drainer predating the early unlink — or the lock.
func probeDaemonState(deadline time.Time) (daemonState, int) {
	state, pid := pingState(pingDaemonResponseUntil(boundedPingDeadline(deadline)))
	if state != daemonUnknown {
		return state, pid
	}
	if lock := homeLockState(); lock != daemonExited {
		return lock, 0
	}
	if pid := livePIDFilePID(); pid > 0 {
		return daemonDraining, pid
	}
	return daemonExited, 0
}

// homeLockState is probeDaemonState's instant read of this home's lock. An
// absent lock file reads exited — a pre-lock drainer is indistinguishable from
// no daemon in one instant, and the spawned child's own socket arbitration
// converges it. An unresolvable home is unknown.
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
// missing daemon.lock is exited too — the replacement would create and take
// it — which is why exitState never trusts a non-held lock without Ping.
//
// Deliberately not ProbeHomeLock, which answers doctor's may-this-home-be-
// deleted question and reads an unrecognized filesystem as unknown — that
// would stall every PID-less wait on a home on NFS/FUSE. The probe holds the
// lock only for the instant between its flock and its unlock.
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
// out the drain (see waitOutDrain) and reports true only if a new daemon comes
// up serving; the per-home lock the caller takes next arbitrates a drain that
// outlived the bound — a held lock is a non-zero
// exit, which the unit's Restart=on-failure retries. Unlike EnsureDaemon this
// never stops anything, so proceeding is safe here.
func daemonAlreadyServing() bool {
	state, pid := probeDaemonState(time.Time{})
	switch state {
	case daemonServing:
		return true
	case daemonDraining:
		log.InfoLog.Printf("the daemon for this home (pid %d, 0 if unknown) is draining after shutdown; waiting for it to exit", pid)
		switch waitOutDrain(time.Time{}, time.Now().Add(shutdownCompleteGrace)) {
		case daemonServing:
			return true
		case daemonDraining:
			log.InfoLog.Printf("the daemon for this home is still draining at the bound; proceeding to home-lock arbitration")
		}
	}
	return false
}

// waitOutDrain polls the state probe (#5007) until a drain seen by a pre-launch
// consumer ends: serving, when a new lock holder has come up — a drainer's
// socket never re-opens, so inside a drain wait a serving answer can only be
// that — or exited, once nothing holds the home. Draining and unknown keep
// polling; at until it reports draining. probeDeadline bounds each probe's
// dial. It is not the post-ack exit wait: that one must not read serving as
// exit (see exitState), because there the target itself may still answer.
func waitOutDrain(probeDeadline, until time.Time) daemonState {
	for {
		if state, _ := probeDaemonState(probeDeadline); state == daemonServing || state == daemonExited {
			return state
		}
		if !time.Now().Before(until) {
			return daemonDraining
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
