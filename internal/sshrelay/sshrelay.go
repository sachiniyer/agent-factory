// Package sshrelay is the stdio↔TCP relay af hands to ssh(1) as a
// ProxyCommand, so every step of a remote session dials ONE resolved address
// (#3086).
//
// WHY A PROXYCOMMAND AND NOT AN ssh OPTION. A multi-address `ssh.host` can split
// a session across machines: the workspace is created on host A while the clone,
// the tunnel or the reap runs on host B, and the reap then removes B's directory,
// reports success and leaves A's agent-server and workspace leaking silently. The
// two previous attempts at fixing that both lost, and their measurements bound
// what is left:
//
//   - Pinning the ADDRESS as ssh's destination forces `-o HostKeyAlias`, and no
//     alias value is correct — a plain known_hosts entry on a non-default port is
//     keyed `[name]:port` while a certificate principal is the bare `name`, and
//     HostKeyAlias is one string (#3090, reverted by #3100).
//   - A `ControlMaster` multiplex keeps the name, but a master is a PROCESS, and
//     `restoreRuntimeCleanup` composes a teardown from PERSISTED data in a FRESH
//     daemon, where no master exists and none can be adopted.
//
// A ProxyCommand pins BELOW ssh's naming layer. ssh's destination stays the NAME,
// so ssh itself computes the known_hosts key and the certificate principal
// exactly as it does today, and only the TCP dial is pinned. Measured against
// OpenSSH_9.6p1 and a real sshd whose host key is certified for the principal
// `pinned.invalid` on port 2299:
//
//	ProxyCommand pinning                -> cert ACCEPTED
//	HostKeyAlias=[pinned.invalid]:2299  -> Host key verification failed
//
// Same sshd, same certificate. And because the pinned address travels in the
// persisted cleanup handle rather than in a live process, a reap after a daemon
// restart composes the same command.
//
// WHY af's OWN BINARY AND NOT nc/socat. An external binary is the same failure
// class as an ssh option that is too new: it is a dependency af cannot guarantee,
// and its absence breaks the whole backend rather than degrading. af is by
// definition present, because af is the process composing the command.
package sshrelay

import (
	"fmt"
	"io"
	"net"
)

// Subcommand is the hidden `af` subcommand that runs the relay.
//
// It lives HERE, in a leaf package, rather than in commands/: session composes
// the ProxyCommand and commands registers the cobra command, session cannot
// import commands (the import already runs the other way), and a duplicated
// string literal in two packages is the drift class this repo keeps paying for —
// a "mirrors the other one" comment is unenforced and false the moment either
// side is edited (#2097). One constant cannot disagree with itself.
const Subcommand = "ssh-relay"

// closeWriter is *net.TCPConn's half-close. Named as an interface so the relay
// can be driven over a pipe pair in a test without a real socket.
type closeWriter interface{ CloseWrite() error }

// Run dials host:port and shuttles bytes between that connection and in/out
// until the remote closes.
//
// OUT IS THE SSH TRANSPORT STREAM. Every byte written to it is protocol, so a
// single stray one — a log line, a banner, a usage dump, a fmt.Println — corrupts
// the session rather than merely being noisy. Nothing in this function or in the
// command that calls it may write to stdout except the copy below; diagnostics go
// to stderr, and the error returned here is rendered there by the caller.
//
// HALF-CLOSE IS LOAD-BEARING, not tidiness. af streams its own `af` binary to the
// remote over ssh's stdin (`cat > af`), and the remote `cat` finishes only when it
// sees EOF. Closing the whole connection at that point would also tear down the
// reply direction before the remote's answer arrived, and NOT closing the write
// half at all hangs the copy forever — so the write half is shut down on stdin
// EOF while reading continues.
func Run(host, port string, in io.Reader, out io.Writer) error {
	// JoinHostPort, never fmt.Sprintf("%s:%s"): an IPv6 literal needs brackets,
	// and af pins literal addresses by construction — including the scoped
	// `fe80::1%eth0` form, which JoinHostPort renders as `[fe80::1%eth0]:22` and
	// net.Dial parses back.
	addr := net.JoinHostPort(host, port)

	// No dial deadline, deliberately. ssh applies none of its own to a
	// ProxyCommand, so imposing one here would make af stricter than the plain
	// `ssh` this replaces and could fail a slow link that works by hand. Every
	// caller already bounds it from outside: the provision steps run under a
	// context deadline whose cancel SIGKILLs the whole process group (this relay
	// included), and the long-lived tunnel child is bounded by the readiness probe
	// that fails the provision and kills the same group.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("af %s: dialling the pinned address %s failed: %w", Subcommand, addr, err)
	}
	defer func() { _ = conn.Close() }()

	// stdin → remote. NOT waited on: once the remote has closed, a read on stdin
	// can block forever (ssh may hold the pipe open), and a relay that will not
	// exit wedges the step that spawned it.
	go func() {
		_, _ = io.Copy(conn, in)
		if cw, ok := conn.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}()

	// remote → stdout. This returning IS the exit condition: it means the far side
	// closed, which is exactly when ssh expects its proxy to go away.
	_, err = io.Copy(out, conn)
	return err
}
