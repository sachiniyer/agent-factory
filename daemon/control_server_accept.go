package daemon

import "net"

// testHookControlListener wraps the control socket listener immediately after
// bind and chmod, before the accept loop consumes it. Production is the
// identity wrapper (the listener is used as-is); a test sets it to wrap the
// listener with one that injects Accept errors, so the accept loop's
// transient-error retry / fatal-close path — unreachable otherwise, since
// startControlServer owns its listener and never returns it — can be
// exercised. The wrapper's Close delegates to the underlying listener so net
// still unlinks the socket file. See control_server_accept_test.go.
var testHookControlListener = func(l net.Listener) net.Listener { return l }
