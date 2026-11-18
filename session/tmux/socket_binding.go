package tmux

import (
	"errors"
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
	// socketBindingGone: the server exited while its sockets were being read,
	// and an independent lookup of the pid confirms it (#7541). A server that
	// is gone owns nothing, so it is disproved, as the probe's ESRCH arm treats
	// a server that vanishes before its signal. It is not signalled.
	socketBindingGone
)

// tmuxServerSocketBinding classifies pid against socketPath and returns the
// listening path that decided it: the matching path when bound, every path it
// holds when bound elsewhere, and "" otherwise. A package var so a test can
// stage a binding for a real server; the real one reads the kernel.
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
		// compare. Neither rules the server out, unless the server is proved
		// gone: an independent lookup says the pid names no live process, an
		// exited pid or a zombie, which holds no sockets at all. That covers a
		// server that exits between the snapshot and the read, whatever error
		// the read met on the way. A process found there, even one that
		// reused the pid, proves nothing.
		if serverPIDExited(pid) {
			return socketBindingGone, ""
		}
		return socketBindingUnresolved, ""
	}
	return bindingAgainst(socketPath, paths)
}

// serverPIDExited reports whether pid is confirmed to name no live process: the
// lookup itself says it exited (a zombie included). Any other answer, an error
// included, is not proof.
var serverPIDExited = func(pid int) bool {
	_, err := proctree.Lookup(pid)
	return errors.Is(err, proctree.ErrProcessExited)
}

// bindingAgainst classifies a server whose listening sockets are bound to
// paths against the absent socketPath. A comparison that cannot be made
// certain (a symlink loop on either side) leaves the server unresolved: only a
// certain mismatch rules it out.
func bindingAgainst(socketPath string, paths []string) (socketBinding, string) {
	want, ok := canonicalSocketPath(socketPath)
	if !ok {
		return socketBindingUnresolved, ""
	}
	uncertain := false
	for _, p := range paths {
		got, ok := canonicalSocketPath(p)
		if !ok {
			uncertain = true
			continue
		}
		if got == want || sameSocketDir(p, socketPath) {
			return socketBindingBound, p
		}
	}
	if uncertain {
		return socketBindingUnresolved, ""
	}
	return socketBindingElsewhere, strings.Join(paths, ", ")
}

// sameSocketPath reports whether two absolute socket paths certainly name the
// same socket (see canonicalSocketPath and sameSocketDir).
func sameSocketPath(a, b string) bool {
	ca, okA := canonicalSocketPath(a)
	cb, okB := canonicalSocketPath(b)
	return (okA && okB && ca == cb) || sameSocketDir(a, b)
}

// sameSocketDir reports whether two paths have the same last component in
// directories that both exist and are the same directory: a bind mount, say,
// which no spelling reveals. os.Stat lets the kernel walk each spelling,
// symlinks and ".." included.
func sameSocketDir(a, b string) bool {
	dirA, baseA := splitSocketPath(a)
	dirB, baseB := splitSocketPath(b)
	if baseA != baseB {
		return false
	}
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

// maxSocketPathLinks bounds the dangling symlinks canonicalSocketPath follows,
// as the kernel bounds a path walk (Linux MAXSYMLINKS is 40).
const maxSocketPathLinks = 40

// canonicalSocketPath resolves p the way the kernel walks it, so two spellings
// of one socket compare equal (#7533):
//
//   - The longest prefix of p's raw components that exists is resolved with
//     filepath.EvalSymlinks, which walks symlinks and ".." the way the kernel
//     does. Nothing is cleaned lexically first: a ".." after a symlink names
//     the symlink TARGET's parent, so /tmp/base/link/../s, with link pointing
//     at /tmp/base/real/child, is /tmp/base/real/s (Codex on #7537).
//   - The first component past that prefix may be a symlink whose target is
//     gone. EvalSymlinks fails on a dangling link, but the kernel still
//     follows it, so it is followed here: a client that reached the socket
//     through /tmp/alias.sock -> /tmp/actual.sock names the server bound to
//     /tmp/actual.sock after that file is unlinked (Codex on #7537).
//   - Whatever lies past that is kept as written. It cannot be walked: the
//     kernel would fail there too.
//
// false means the walk did not end within maxSocketPathLinks links (a loop):
// the path names nothing certain, and a comparison with it rules nothing out.
func canonicalSocketPath(p string) (string, bool) {
	for range maxSocketPathLinks + 1 {
		parts := strings.Split(p, "/")
		resolved, i := "", len(parts)
		for ; i > 0; i-- {
			prefix := strings.Join(parts[:i], "/")
			if prefix == "" {
				prefix = "/"
			}
			if r, err := filepath.EvalSymlinks(prefix); err == nil {
				resolved = r
				break
			}
		}
		if i == 0 {
			return filepath.Clean(p), true
		}
		rest := parts[i:]
		if len(rest) == 0 {
			return resolved, true
		}
		dir := strings.TrimSuffix(resolved, "/")
		target, err := os.Readlink(dir + "/" + rest[0])
		if err != nil {
			return filepath.Join(append([]string{resolved}, rest...)...), true
		}
		if !filepath.IsAbs(target) {
			target = dir + "/" + target
		}
		p = target
		if len(rest) > 1 {
			p += "/" + strings.Join(rest[1:], "/")
		}
	}
	return "", false
}

// possibleSocketOwners is the socket-absent refusal's list of who could still
// own socketPath: every candidate the probe could not rule out, read the way
// the probe reads them but with nothing sent. servers are positively
// identified tmux servers that are bound to the path or unresolved against
// it. unidentified are candidates not positively identified as a server, the
// 0 sentinel for a table that could not be read among them; the refusal
// offers them no signal (#7541). A server bound to another path, or proved
// gone, is left out.
func possibleSocketOwners(socketPath string, pids []int) (servers, unidentified []int) {
	for _, pid := range pids {
		if pid <= 0 || !tmuxServerIdentified(pid) {
			unidentified = append(unidentified, pid)
			continue
		}
		switch binding, _ := tmuxServerSocketBinding(pid, socketPath); binding {
		case socketBindingElsewhere, socketBindingGone:
		default:
			servers = append(servers, pid)
		}
	}
	return servers, unidentified
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
