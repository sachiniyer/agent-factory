//go:build linux

package daemon

import "github.com/sachiniyer/agent-factory/internal/proctree"

// unixSocketsOf lists every named unix socket the kernel reports pid holding:
// the authoritative answer to "which named sockets does this process hold". A
// client socket the kernel left unnamed never appears, so holding a path means
// the process bound or adopted that name. An abstract socket appears with its
// Abstract flag, which tmuxServerSocketPrivate rejects outright. ok=false
// means the kernel would not show them: no socket evidence, and a tmux server
// without socket evidence is never proven private.
//
// It reads through proctree.UnixSockets, one socket at a time (#7536). The
// proof needs EVERY socket a server holds to lie in the private directory, and
// a walk of /proc/<pid>/net/unix, which is not atomic, could miss the one row
// that lies outside it and so prove a shared server private. A socket the
// per-socket read cannot answer for, or a process that exits or is replaced
// mid-read, fails the whole read, and with it the proof.
func unixSocketsOf(pid int) ([]proctree.UnixSocket, bool) {
	socks, err := proctree.UnixSockets(pid)
	if err != nil {
		return nil, false
	}
	var named []proctree.UnixSocket
	for _, s := range socks {
		if s.Path != "" {
			named = append(named, s)
		}
	}
	return named, true
}
