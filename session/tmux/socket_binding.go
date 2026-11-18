package tmux

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// socketBinding is what a tmux server's own sockets say about the absent socket
// the probe is trying to recover (#7533). The kernel keeps the address each
// listening socket was bound to, and it survives an unlink of the socket file,
// so a server can be asked about directly instead of signalled to find out.
type socketBinding int

const (
	// socketBindingUnresolved: the server's sockets could not be read
	// completely, it holds no listening socket, or the absent path cannot be
	// placed (it is relative). It may be the owner. It is not signalled, and
	// the probe cannot answer "unclaimed" while it remains.
	socketBindingUnresolved socketBinding = iota
	// socketBindingElsewhere: every listening socket it holds is bound to some
	// other path. It does not own the absent one, and it is not signalled.
	socketBindingElsewhere
	// socketBindingBound: a listening socket it holds is bound to the absent
	// path. It is the owner, and SIGUSR1 is how it recreates the socket.
	socketBindingBound
)

// tmuxServerSocketBinding classifies pid against socketPath and returns the
// listening path that decided it ("" when unresolved). A package var so a test
// can stage a binding for a real server; the real one reads the kernel.
var tmuxServerSocketBinding = readTmuxServerSocketBinding

func readTmuxServerSocketBinding(pid int, socketPath string) (socketBinding, string) {
	if !filepath.IsAbs(socketPath) {
		// tmux reports the path as its client resolved it. A relative one is
		// relative to a working directory this process cannot know.
		return socketBindingUnresolved, ""
	}
	paths, err := proctree.UnixListenerPaths(pid)
	if err != nil || len(paths) == 0 {
		// A read that failed, or one that found no listener, names nothing to
		// compare. Neither rules the server out.
		return socketBindingUnresolved, ""
	}
	for _, p := range paths {
		if sameSocketPath(p, socketPath) {
			return socketBindingBound, p
		}
	}
	return socketBindingElsewhere, strings.Join(paths, ", ")
}

// sameSocketPath reports whether two absolute socket paths name the same
// socket. Each is compared with its deepest existing ancestor resolved, so
// /tmp/... and /private/tmp/... agree on darwin even after the socket file, or
// its directory, is gone. Two paths whose directories both still exist and are
// the same directory (a bind mount, say) also agree.
//
// Nothing is cleaned lexically before symlinks are resolved. The kernel keeps a
// bound address as the binder spelled it, and a ".." after a symlink names the
// symlink target's parent: /tmp/base/link/../s, with link pointing at
// /tmp/base/real/child, is /tmp/base/real/s, not /tmp/base/s (Codex on #7537).
func sameSocketPath(a, b string) bool {
	if canonicalSocketPath(a) == canonicalSocketPath(b) {
		return true
	}
	dirA, baseA := splitSocketPath(a)
	dirB, baseB := splitSocketPath(b)
	if baseA != baseB {
		return false
	}
	// os.Stat lets the kernel walk each spelling, symlinks and ".." included.
	da, errA := os.Stat(dirA)
	db, errB := os.Stat(dirB)
	return errA == nil && errB == nil && os.SameFile(da, db)
}

// splitSocketPath splits p at its last separator, without cleaning it.
func splitSocketPath(p string) (dir, base string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ".", p
	}
	if i == 0 {
		return "/", p[1:]
	}
	return p[:i], p[i+1:]
}

// canonicalSocketPath resolves the longest prefix of p's components that
// exists, symlinks and ".." walked the way the kernel walks them
// (filepath.EvalSymlinks), and appends the components past it as written.
func canonicalSocketPath(p string) string {
	parts := strings.Split(p, "/")
	for i := len(parts); i > 0; i-- {
		prefix := strings.Join(parts[:i], "/")
		if prefix == "" {
			prefix = "/"
		}
		if resolved, err := filepath.EvalSymlinks(prefix); err == nil {
			return filepath.Join(append([]string{resolved}, parts[i:]...)...)
		}
	}
	return filepath.Clean(p)
}

// possibleSocketOwners is the socket-absent refusal's list of who could still
// own socketPath: every candidate the probe could not rule out. That is a tmux
// server bound to the path or unresolved against it, a candidate not positively
// identified as a server, and the 0 sentinel for a table that could not be
// read. A server bound to another path is left out. It reads the facts the
// probe reads but sends nothing.
func possibleSocketOwners(socketPath string, pids []int) []int {
	var owners []int
	for _, pid := range pids {
		if pid > 0 && tmuxServerIdentified(pid) {
			if binding, _ := tmuxServerSocketBinding(pid, socketPath); binding == socketBindingElsewhere {
				continue
			}
		}
		owners = append(owners, pid)
	}
	return owners
}

// socketProbeCaller names the code that asked for the socket-absent probe: the
// first frame outside this package's probe files, for the log line each SIGUSR1
// gets.
func socketProbeCaller() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if base := filepath.Base(f.File); base != "server_presence.go" && base != "socket_binding.go" {
			return f.Function + " (" + base + ":" + strconv.Itoa(f.Line) + ")"
		}
		if !more {
			return "unknown"
		}
	}
}
