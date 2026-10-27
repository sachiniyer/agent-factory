// Package peercred reads the kernel-verified identity of the process holding
// the other end of a Unix socket connection.
//
// It exists for #5182: when a process running inside a session's own pane asks
// the daemon to tear that session down (`af sessions archive --self` and the
// kill/tab-close equivalents), teardown must know WHICH captured pane process
// is the caller blocked on the reply, or the reaper waits out its grace period
// on it and then SIGTERMs it — so the archive commits daemon-side while the
// requester is logged as a leak and never reads its outcome.
//
// The answer must come from the kernel and nowhere else. A client-supplied pid
// field would let any process claim the exemption the requester is owed, which
// is precisely property (d) of the fix: a connection's peer pid is read by the
// kernel at connect time (SO_PEERCRED on Linux, LOCAL_PEERPID on darwin) and no
// request the client sends can influence it.
//
// Platform support mirrors internal/proctree's rule: a platform that cannot
// answer fails LOUDLY (ErrUnsupportedPlatform) rather than reporting a
// zero pid that would read as "no requester" — callers that can degrade do so
// knowing they degraded.
package peercred

import (
	"errors"
	"fmt"
	"net"
)

// ErrUnsupportedPlatform is returned by ConnPID on a platform whose kernel
// cannot report a unix-socket peer's pid. It is distinguishable with
// errors.Is so a caller can degrade to "no requester known" on purpose rather
// than conflating it with a real read failure.
var ErrUnsupportedPlatform = errors.New("peercred: reading the socket peer's pid is not supported on this platform")

// ErrNotUnixConn is returned by ConnPID when the connection is not an accepted
// unix socket (a TCP listener's conn, or a wrapped/wrapped-once type). There is
// no kernel peer pid to read across a network transport.
var ErrNotUnixConn = errors.New("peercred: connection is not a unix socket")

// ConnPID returns the kernel-verified pid of the process that connected the
// given unix socket — the peer at CONNECT time, which for a request blocked on
// its reply is exactly the process waiting to read it.
//
// The pid names a process SLOT, not an identity: callers must resolve it
// through proctree.Lookup (pairing it with the process's start stamp) before
// acting on it, or a pid recycled between accept and lookup would be
// misattributed — the same rule every destructive read in this repo follows.
func ConnPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("%w: %T", ErrNotUnixConn, conn)
	}
	return unixConnPeerPID(uc)
}
