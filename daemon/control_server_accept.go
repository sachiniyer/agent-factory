package daemon

import (
	"net"
	"sync"
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

// fatalAcceptClose handles a non-recoverable accept error: it closes the
// listener so net unlinks the socket file (clients then get the recoverable
// "no such file" refusal instead of a dialable-but-dead socket that hangs
// forever) and propagates the failure to the daemon shutdown path so the
// supervisor (systemd/launchd) restarts the daemon rather than keeping a
// wedged-but-running process whose control plane is dead. It reuses the
// Shutdown RPC's own sync.Once so two independent closers of shutdownCh cannot
// panic; a nil shutdownCh (the test configuration that does not wire the
// daemon main loop) skips the shutdown trigger but still closes the listener.
func fatalAcceptClose(listener net.Listener, shutdownCh chan struct{}, shutdownOnce *sync.Once) {
	_ = listener.Close()
	if shutdownCh != nil {
		shutdownOnce.Do(func() { close(shutdownCh) })
	}
}
