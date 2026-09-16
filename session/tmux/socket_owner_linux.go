//go:build linux

package tmux

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// unixSocketsPath is the kernel's unix-socket table, indirected so a test can
// point it at a fixture.
var unixSocketsPath = "/proc/net/unix"

// attestedSocketOwnerPIDs narrows candidates to the subset positively
// observed holding a unix listener bound at socketPath — kernel-attested
// ownership of THIS socket, which is the answer "which server owns this
// socket" rather than "which pids look like tmux" (#4349). The table keeps
// naming an unlinked socket's bound path, which is why an orphaned server
// can be found at all: its listener survives rm with its name intact.
//
//   - owners found -> exactly those pids.
//   - a listener exists at socketPath but no candidate holds it -> {0}: the
//     socket is owned by something unsignalable — a foreign uid, a squatter,
//     an fd table we cannot walk — claimed, but unnameable.
//   - nothing bound there, or the table unreadable -> nil: NOT "no owner".
//     A server in another network namespace is invisible to this table while
//     still holding the socket, so the unattested answer keeps every
//     candidate rather than turning an unknown into a confident unclaimed.
func attestedSocketOwnerPIDs(socketPath string, candidates []int) []int {
	if socketPath == "" {
		return nil
	}
	inodes, ok := unixListenerInodesAt(socketPath)
	if !ok || len(inodes) == 0 {
		return nil
	}
	var owners []int
	for _, pid := range candidates {
		if holdsUnixSocketInode(pid, inodes) {
			owners = append(owners, pid)
		}
	}
	if len(owners) == 0 {
		return []int{0}
	}
	return owners
}

// unixListenerInodesAt returns the inodes of LISTENING unix sockets bound at
// path. A bound listener sets __SO_ACCEPTCON (0x00010000) in Flags; the
// connection sockets a client pair adds name the same path with a different
// state, so the flag — not the path alone — is what keeps clients out of the
// answer. Columns: Num RefCount Protocol Flags Type St Inode Path.
func unixListenerInodesAt(path string) (map[uint64]bool, bool) {
	data, err := os.ReadFile(unixSocketsPath)
	if err != nil {
		return nil, false
	}
	inodes := map[uint64]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x10000 == 0 {
			continue
		}
		// The path is the remainder of the line after the inode column; it is
		// unescaped, so an embedded space joins back rather than truncates.
		if strings.Join(fields[7:], " ") != path {
			continue
		}
		inode, err := strconv.ParseUint(fields[6], 10, 64)
		if err != nil {
			continue
		}
		inodes[inode] = true
	}
	return inodes, true
}

// holdsUnixSocketInode reports whether pid's fd table holds any of inodes —
// the positive proof that the process owns a socket bound at the path. An
// unreadable fd table is "not proven", never "disproven": the process stays a
// candidate through the caller's fallback rather than being ruled out.
func holdsUnixSocketInode(pid int, inodes map[uint64]bool) bool {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		target, err := os.Readlink(dir + "/" + fd.Name())
		if err != nil {
			continue
		}
		ino, ok := strings.CutPrefix(target, "socket:[")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(ino, "]"), 10, 64)
		if err == nil && inodes[n] {
			return true
		}
	}
	return false
}
