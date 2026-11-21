package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

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
	// socketBindingBoundByTable: the per-socket read could not answer (no
	// unix_diag, or the server is in another network namespace), and the
	// /proc/<pid>/net/unix table shows a listener bound to the absent path
	// (#7536). The table walk can miss a row, which only ever loses a match,
	// so a match it does show is the owner, signalled like socketBindingBound.
	// The table never rules a server out: without the per-socket read, a
	// server with no match stays unresolved.
	socketBindingBoundByTable
	// socketBindingGone: the server exited while its sockets were being read,
	// and an independent lookup of the pid confirms it (#7541). A server that
	// is gone owns nothing, so it is disproved, as the probe's ESRCH arm treats
	// a server that vanishes before its signal. It is not signalled.
	socketBindingGone
)

// listenerPathsOf and listenerPathsByTableOf are the two readers
// readTmuxServerSocketBinding consults: the per-socket read, which may rule a
// server out, and the table walk, which may only find one bound. Package vars
// so a test can stand in a host without unix_diag.
var (
	listenerPathsOf        = proctree.UnixListenerPaths
	listenerPathsByTableOf = proctree.UnixListenerPathsByTable
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
	paths, err := listenerPathsOf(pid)
	if err != nil && !errors.Is(err, proctree.ErrProcessExited) {
		// The per-socket read could not answer. The table may still show the
		// owner (positive evidence only; see socketBindingBoundByTable).
		if tablePaths, tableErr := listenerPathsByTableOf(pid); tableErr == nil {
			if b, p := bindingAgainst(socketPath, tablePaths); b == socketBindingBound {
				return socketBindingBoundByTable, p
			}
		}
	}
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

// serverPIDExited reports whether pid is PROVED to name no live process (#7541):
//
//   - proctree.Lookup answers ErrProcessExited: a zombie, which holds no
//     sockets, or (darwin) a pid kern.proc no longer has.
//   - On Linux a pid with no process at all reads ENOENT from
//     /proc/<pid>/stat, not ErrProcessExited (measured on Linux 6.17 for a
//     reaped child). ENOENT alone is not proof, since a /proc entry hidden by
//     hidepid reads the same. So it counts only when the kernel's own answer
//     agrees: kill(pid, 0), which delivers nothing, is ESRCH only when no
//     process has the pid. A live process this uid may not signal answers
//     EPERM (measured: pid 1).
//   - ESRCH from the read itself is the kernel's own answer already — a pid
//     it answers ESRCH for is gone, whichever errno the /proc read met
//     (#7695 R3). It needs no kill(pid, 0) to agree: hidepid fakes a missing
//     entry, not an interrupted stat.
//
// Any other answer, an error included, is not proof.
var serverPIDExited = func(pid int) bool {
	_, err := proctree.Lookup(pid)
	if errors.Is(err, proctree.ErrProcessExited) || errors.Is(err, syscall.ESRCH) {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
	return false
}

// bindingAgainst classifies a server whose listening sockets are bound to
// paths against the absent socketPath. Only a CERTAIN mismatch rules it out
// (#7533): a symlink loop, or a client spelling truncated at a deleted or
// unclassifiable component that may have been an alias reaching this
// server's bound path, leaves the server unresolved (#7562). A bound path
// needs no such benefit: the kernel keeps the spelling bind() was given
// verbatim (internal/proctree/unixsocket.go), and af launches its servers
// bare, so tmux realpaths the socket's base dir before binding — a missing
// component in an af server's recorded path is a deleted real directory,
// never an alias. A server bound verbatim — through tmux -S, or reached
// through $TMUX — may carry a deleted alias in its path; treating it as
// literal is an accepted residual, and it reads Elsewhere exactly as on
// master.
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
		if got.path == want.path || sameSocketDir(p, socketPath) {
			return socketBindingBound, p
		}
		if !certainSocketMismatch(want, got) {
			uncertain = true
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
	return (okA && okB && ca.path == cb.path) || sameSocketDir(a, b)
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

// socketPathStop is why canonicalSocketPath's walk stopped past its resolved
// prefix — what the first unresolvable component is known to be (#7562).
type socketPathStop int

const (
	// walkStopAbsent: rest[0] provably does not exist (ENOENT) — the deleted
	// component. Whatever it was, an alias included, is unknowable now.
	walkStopAbsent socketPathStop = iota
	// walkStopNotLink: rest[0] exists and is provably not a symlink (EINVAL):
	// the spelling is literal — a file at the basename — or provably broken
	// when components remain below it. Either way it names at most itself.
	walkStopNotLink
	// walkStopNotDir: the resolved prefix ends at a non-directory (ENOTDIR),
	// so rest[0] cannot exist under it: the spelling is provably broken.
	walkStopNotDir
	// walkStopUnknown: rest[0] could not be classified — a search-denied
	// parent, a raced re-creation, an opaque error. It may still be an alias.
	walkStopUnknown
)

// socketPathWalk is what canonicalSocketPath learned about one spelling
// (#7562): the spelling it reached, and where it stopped reaching.
type socketPathWalk struct {
	// path is the spelling comparisons use: the resolved prefix plus whatever
	// components the walk could not enter, joined verbatim — never cleaned,
	// so a "." or ".." the walk could not test cannot make two spellings
	// converge. Two spellings with the same path name the same socket — the
	// unwalked parts are spelled identically, so anything hidden under them
	// lands on both alike.
	path string
	// resolved is the longest prefix the filesystem resolved, and rest the
	// raw components past it — both empty when every component resolved.
	// rest[0] is the first component the walk could not enter, and stop says
	// what the walk learned about it.
	resolved string
	rest     []string
	stop     socketPathStop
	// names is the basename of every component the walk itself entered —
	// each rest[0] it followed or stopped on, every name this spelling's
	// own symlink chain passed through (#7736). A live owner's verbatim
	// bind spelling can carry an intermediate chain name after that name
	// was rerouted through another link, so boundRowMatcher admits a row
	// on any of them; recording them here keeps the admission set on the
	// same snapshot as the verdict — no second read of a mutable chain.
	// A path whose links all resolved inside EvalSymlinks records nothing;
	// the names matter only where the walk had to follow links itself,
	// which is exactly when the chain's end cannot be resolved.
	names map[string]bool
}

// certain reports whether the walk's spelling is one a comparison may TRUST
// for a mismatch — it names at most one place:
//
//   - every component resolved; or
//   - the walk stopped only at the socket's own provably-absent basename —
//     the ordinary "socket file is gone" shape, a resolved directory plus a
//     literal name, and the probe's own premise: treating it as a possible
//     alias would leave every absent socket undecidable, the over-refusal
//     #7562 had to avoid. The residual it accepts is a hand-placed symlink
//     at the socket's own basename — af's socket paths do not produce one.
//   - the walk learned the spelling is literal (a non-link at rest[0]) or
//     provably broken (a non-directory in the middle): nothing hidden.
func (w socketPathWalk) certain() bool {
	switch {
	case len(w.rest) == 0:
		return true
	case w.stop == walkStopAbsent && len(w.rest) == 1:
		return true
	case w.stop == walkStopNotLink || w.stop == walkStopNotDir:
		return true
	}
	return false
}

// certainSocketMismatch reports whether the spelling being read for a
// socket (want — the client's absent socket, or a carrier's env socket)
// PROVABLY names a different socket than the trusted bound or recorded path
// (got) it already compares unequal to — the only comparison that may rule
// a live server out (#7533, #7562).
//
// got's path is literal even where its own walk truncated. The kernel
// keeps the spelling bind() was given verbatim — getsockname,
// /proc/net/unix and sock_diag all return it unresolved — and af launches
// its servers bare, so tmux realpaths the socket's base dir before binding
// (server_scope_linux.go, server_scope_other.go): a missing component in
// an af server's recorded path is a deleted real directory, never an
// alias. A server bound verbatim — through tmux -S, or reached through
// $TMUX — may carry a deleted alias in its path; treating it as literal
// is the accepted residual alongside the last-component link below, and it
// reads Elsewhere exactly as on master.
//
// Only want's truncation can hide an alias, and only through rest[0]: if
// it was a symlink to any dir D, the spelling resolves to
// resolve(D + "/" + tail), which can land only on a path ending in the
// tail's last component — a link inside the hidden tree redirects to a
// path still ending in that name, and a link AT that last component is
// the residual certain() already accepts. So want can name got only when
// got's basename equals the tail's last component; on any other unequal
// pair the mismatch is provable, and a non-plain tail
// ("", ".", "..") is unbounded, hence uncertain.
func certainSocketMismatch(want, got socketPathWalk) bool {
	if want.certain() {
		return true
	}
	tail := want.rest[1:]
	if len(tail) == 0 || !plainSocketComponents(tail) {
		return false
	}
	_, boundBase := splitSocketPath(got.path)
	return boundBase != tail[len(tail)-1]
}

// plainSocketComponents reports whether every unwalked component is a plain
// name the literal tail compare can trust — no "", "." or "..", each of which
// the kernel walks somewhere the spelling does not say.
func plainSocketComponents(rest []string) bool {
	for _, c := range rest {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return true
}

// canonicalSocketPath resolves p the way the kernel walks it, so two spellings
// of one socket compare equal (#7533):
//
//   - The longest prefix of p's raw components that exists is resolved with
//     filepath.EvalSymlinks, which walks symlinks and ".." the way the kernel
//     does. Nothing is cleaned lexically first: a ".." after a symlink names
//     the symlink TARGET's parent, so /tmp/base/link/../s, with link pointing
//     at /tmp/base/real/child, is /tmp/base/real/s (Codex on #7537). One
//     caveat: EvalSymlinks also cleans a ".." LEXICALLY, so dir/../s would
//     resolve when dir is deleted — verifiedSocketPrefix rejects that, since
//     a deleted dir may have been an alias whose ".." lands elsewhere.
//   - The first component past that prefix may be a symlink whose target is
//     gone. EvalSymlinks fails on a dangling link, but the kernel still
//     follows it, so it is followed here: a client that reached the socket
//     through /tmp/alias.sock -> /tmp/actual.sock names the server bound to
//     /tmp/actual.sock after that file is unlinked (Codex on #7537).
//   - Whatever lies past that is kept as written. It cannot be walked: the
//     kernel would fail there too — and the component that stopped it may
//     itself be a deleted alias (#7562), so the answer carries where the walk
//     ended for the comparison to weigh, not a bare "resolved" flag.
//
// false means the walk did not end within maxSocketPathLinks links (a loop)
// or could not place the path at all (a relative spelling): the path names
// nothing certain, and a comparison with it rules nothing out.
func canonicalSocketPath(p string) (socketPathWalk, bool) {
	var names map[string]bool
	for range maxSocketPathLinks + 1 {
		parts := strings.Split(p, "/")
		resolved, i := "", len(parts)
		for ; i > 0; i-- {
			prefix := strings.Join(parts[:i], "/")
			if prefix == "" {
				prefix = "/"
			}
			if r, err := filepath.EvalSymlinks(prefix); err == nil {
				if !verifiedSocketPrefix(parts, i) {
					continue
				}
				resolved = r
				break
			}
		}
		if i == 0 {
			// Not even "/" resolved — a relative spelling, which no
			// absolute bound path can be placed against.
			return socketPathWalk{}, false
		}
		rest := parts[i:]
		if len(rest) == 0 {
			return socketPathWalk{path: resolved, resolved: resolved, names: names}, true
		}
		if names == nil {
			names = map[string]bool{}
		}
		names[rest[0]] = true
		dir := strings.TrimSuffix(resolved, "/")
		target, err := os.Readlink(dir + "/" + rest[0])
		if err != nil {
			return socketPathWalk{
				path:     dir + "/" + strings.Join(rest, "/"),
				resolved: resolved,
				rest:     rest,
				stop:     socketPathStopOf(err),
				names:    names,
			}, true
		}
		if !filepath.IsAbs(target) {
			target = dir + "/" + target
		}
		p = target
		if len(rest) > 1 {
			p += "/" + strings.Join(rest[1:], "/")
		}
	}
	return socketPathWalk{}, false
}

// verifiedSocketPrefix reports whether parts[:i] is a prefix the kernel could
// have reached. EvalSymlinks cleans a ".." lexically, so dir/../s resolves
// even when dir is deleted — an answer the kernel's walk would not give, and
// a deleted dir may have been an alias whose ".." landed on a different
// parent (#7562). Each ".." must therefore cancel a component that verifies
// on its own spelling; un-canceled components were verified by the resolve.
func verifiedSocketPrefix(parts []string, i int) bool {
	open := make([]int, 0, i)
	for j := 0; j < i; j++ {
		switch parts[j] {
		case "..":
			if len(open) == 0 {
				continue // a ".." at the root cancels nothing
			}
			k := open[len(open)-1]
			open = open[:len(open)-1]
			if _, err := os.Lstat(strings.Join(parts[:k+1], "/")); err != nil {
				return false
			}
		case "", ".":
		default:
			open = append(open, j)
		}
	}
	return true
}

// socketPathStopOf classifies why a walk could not enter rest[0] (#7562):
// provably absent, provably not a link, provably unenterable, or unknown.
func socketPathStopOf(err error) socketPathStop {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return walkStopAbsent
	case errors.Is(err, syscall.EINVAL):
		return walkStopNotLink
	case errors.Is(err, syscall.ENOTDIR):
		return walkStopNotDir
	}
	return walkStopUnknown
}

// missingSocketDir names the directory chain the walk could not enter — from
// the resolved prefix down to the socket's basename — when the component
// that stopped it is provably absent and stands before the basename. That is
// the deleted-directory shape whose uncertainty recreating the chain
// discharges (#7647): every component between the prefix and the basename
// then exists, so the next walk reaches the absent basename — the certain
// arm — and a server bound elsewhere is ruled out. The discharge is safe
// only after each candidate is cleared through its own bound socket: the
// deleted component may have been a link to a live server's dir (#7562).
// "" means the walk did not stop at a deleted non-final component, so
// recreating a directory cannot help — a missing basename is already
// certain, and anything else is unknowable or provably broken rather than
// deleted.
func missingSocketDir(socketPath string) string {
	w, ok := canonicalSocketPath(socketPath)
	if !ok || w.stop != walkStopAbsent || len(w.rest) < 2 {
		return ""
	}
	return strings.TrimSuffix(w.resolved, "/") + "/" + strings.Join(w.rest[:len(w.rest)-1], "/")
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
		if base := filepath.Base(f.File); base != "server_presence.go" && base != "socket_binding.go" &&
			base != "socket_owner.go" && base != "socket_owner_linux.go" && base != "socket_owner_other.go" {
			return f.Function + " (" + base + ":" + strconv.Itoa(f.Line) + ")"
		}
		if !more {
			return "unknown"
		}
	}
}
