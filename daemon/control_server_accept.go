package daemon

import (
	"net"
	"sync"
	"sync/atomic"
)

// testHookControlListener wraps the control socket listener immediately after
// bind and chmod, before the accept loop consumes it. Production is the
// identity wrapper (the listener is used as-is); a test sets it to wrap the
// listener with one that injects Accept errors, so the accept loop's
// transient-error retry / fatal-close path — unreachable otherwise, since
// startControlServer owns its listener and never returns it — can be
// exercised. The wrapper's Close delegates to the underlying listener so net
// still unlinks the socket file. See control_server_accept_test.go.
var testHookControlListener = func(l net.Listener) net.Listener { return l }

// controlAcceptFatalErr carries a fatal control-socket accept error from the
// accept goroutine to runDaemon so the process exits non-zero and the autostart
// supervisor (systemd Restart=on-failure / launchd KeepAlive.SuccessfulExit)
// restarts the daemon — instead of the clean nil return the Shutdown RPC's
// shutdownCh close produces, which leaves the wedged daemon's scheduler
// stopped until a client explicitly starts it again. fatalAcceptClose stores
// the error BEFORE closing shutdownCh, so the close→receive happens-before edge
// makes the Store visible to runDaemon's Load after <-shutdownCh fires. runDaemon
// clears it at startup so a fatal error from a prior daemon in this process
// (tests run runDaemon multiple times in one binary) cannot leak into a later
// clean shutdown. The fatal path still closes shutdownCh so the graceful
// drain + SaveInstancesForShutdown runs (state survives the restart); only the
// exit code changes (non-zero) — the supervisor restart policy keys on that.
var controlAcceptFatalErr atomic.Pointer[error]

func setFatalAcceptErr(err error) {
	e := err
	controlAcceptFatalErr.Store(&e)
}

// fatalAcceptShutdownErr returns the fatal accept error if the shutdown was
// triggered by a fatal control-socket accept error, else nil. Called by runDaemon
// at each shutdown exit point to decide the exit code.
func fatalAcceptShutdownErr() error {
	if p := controlAcceptFatalErr.Load(); p != nil {
		return *p
	}
	return nil
}

func resetFatalAcceptErr() {
	controlAcceptFatalErr.Store(nil)
}

// fatalAcceptClose handles a non-recoverable accept error: it closes the
// listener so net unlinks the socket file (clients then get the recoverable
// "no such file" refusal instead of a dialable-but-dead socket that hangs
// forever) and propagates the failure to the daemon shutdown path so the
// supervisor (systemd/launchd) restarts the daemon rather than keeping a
// wedged-but-running process whose control plane is dead. It records the error
// via setFatalAcceptErr BEFORE closing shutdownCh so runDaemon returns non-zero
// (see controlAcceptFatalErr); the close still reuses the Shutdown RPC's own
// sync.Once so two independent closers of shutdownCh cannot panic. A nil
// shutdownCh (the test configuration that does not wire the daemon main loop)
// skips the shutdown trigger but still closes the listener.
func fatalAcceptClose(listener net.Listener, shutdownCh chan struct{}, shutdownOnce *sync.Once, err error) {
	_ = listener.Close()
	setFatalAcceptErr(err)
	if shutdownCh != nil {
		shutdownOnce.Do(func() { close(shutdownCh) })
	}
}
