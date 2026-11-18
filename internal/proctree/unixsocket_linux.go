//go:build linux

package proctree

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// unixSockets reads every unix socket pid holds, one at a time (#7536). For
// each fd that is a socket, its protocol name says whether it is a unix
// socket, and an exact sock_diag lookup by inode reads it. A step that finds
// the fd closed, or pointing at something else, skips it: it is no longer
// held. Any other failure fails the whole read.
func unixSockets(pid int) ([]UnixSocket, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, err
	}
	var diag *unixDiagConn
	defer func() {
		if diag != nil {
			diag.close()
		}
	}()
	var out []UnixSocket
	for _, e := range ents {
		link := fdDir + "/" + e.Name()
		ino, ok, err := fdSocketInode(link)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		unixSock, held, err := isUnixSocketFd(link, ino)
		if err != nil {
			return nil, fmt.Errorf("fd %s: %w", e.Name(), err)
		}
		if !held || !unixSock {
			continue
		}
		if diag == nil {
			if diag, err = openUnixDiagFn(); err != nil {
				return nil, fmt.Errorf("sock_diag: %w", err)
			}
		}
		a, err := unixDiagLookup(diag, ino)
		if errors.Is(err, errUnixDiagNoSuchSocket) {
			if still, stillErr := fdStillSocket(link, ino); stillErr == nil && !still {
				continue // closed since the listing: no longer held
			}
			// The fd still holds it and the kernel cannot find it: a socket in
			// another network namespace, or no unix_diag to look it up.
			return nil, fmt.Errorf("fd %s: socket inode %d is held but sock_diag cannot find it: %w", e.Name(), ino, err)
		}
		if err != nil {
			return nil, fmt.Errorf("fd %s: %w", e.Name(), err)
		}
		out = append(out, UnixSocket{Path: a.path, Abstract: a.abstract, Listening: a.listening})
	}
	return out, nil
}

// fdSocketInode reads the inode of the socket fd link names. ok is false when
// the fd is not a socket or has closed since the listing.
func fdSocketInode(link string) (ino uint32, ok bool, err error) {
	target, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	s, isSock := strings.CutPrefix(target, "socket:[")
	if !isSock {
		return 0, false, nil
	}
	s, isSock = strings.CutSuffix(s, "]")
	if !isSock {
		return 0, false, fmt.Errorf("unexpected socket link %q", target)
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false, fmt.Errorf("unexpected socket inode in %q: %w", target, err)
	}
	return uint32(n), true, nil
}

// fdStillSocket reports whether link still names socket inode ino.
func fdStillSocket(link string, ino uint32) (bool, error) {
	now, ok, err := fdSocketInode(link)
	if err != nil {
		return false, err
	}
	return ok && now == ino, nil
}

// isUnixSocketFd reports whether the socket at link, inode ino, is a unix
// socket, from its sockfs protocol name. held is false when the fd no longer
// names that socket, by the time of the read or just after it, so the name
// read was not about it. Measured on Linux 6.17: a unix stream socket reads
// "UNIX-STREAM" (its own proto since Linux 5.15; "UNIX" before), datagram and
// seqpacket "UNIX", TCP "TCP"/"MPTCP", UDP "UDP", netlink "NETLINK"; a
// non-socket answers EOPNOTSUPP.
func isUnixSocketFd(link string, ino uint32) (unixSock, held bool, err error) {
	buf := make([]byte, 64)
	n, xerr := unix.Getxattr(link, "system.sockprotoname", buf)
	still, err := fdStillSocket(link, ino)
	if err != nil {
		return false, false, err
	}
	if !still {
		return false, false, nil
	}
	if xerr != nil {
		// The fd still names the socket, so the kernel would not say what it
		// is. Not knowing, nothing may be skipped.
		return false, true, fmt.Errorf("socket inode %d: system.sockprotoname: %w", ino, xerr)
	}
	name := strings.TrimRight(string(buf[:n]), "\x00")
	return name == "UNIX" || strings.HasPrefix(name, "UNIX-"), true, nil
}

// unixDiagConn is one NETLINK_SOCK_DIAG socket, used for every lookup of one
// read. portid is the netlink port id the kernel bound it to, which every
// reply must be addressed to.
type unixDiagConn struct {
	fd     int
	seq    uint32
	portid uint32
}

// unixDiagReplyTimeout bounds one lookup's wait for its reply. An exact lookup
// is answered at once; a reply that does not come is an error, never a socket.
const unixDiagReplyTimeout = 2 * time.Second

func openUnixDiag() (*unixDiagConn, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, netlinkSockDiag)
	if err != nil {
		return nil, err
	}
	tv := unix.NsecToTimeval(unixDiagReplyTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	// Bind explicitly so the kernel assigns the port id now, and read it back:
	// every reply must be addressed to it.
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	sa, err := unix.Getsockname(fd)
	nl, ok := sa.(*unix.SockaddrNetlink)
	if err != nil || !ok {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("sock_diag: cannot read the socket's own port id: %v", err)
	}
	return &unixDiagConn{fd: fd, portid: nl.Pid}, nil
}

func (c *unixDiagConn) close() {
	if c.fd >= 0 {
		_ = unix.Close(c.fd)
	}
}

// openUnixDiagFn and unixDiagLookup are package vars so a test can stand in a
// kernel without unix_diag, or one that cannot find a socket the process
// still holds; nothing else sets them.
var (
	openUnixDiagFn = openUnixDiag
	unixDiagLookup = (*unixDiagConn).lookup
)

func (c *unixDiagConn) lookup(ino uint32) (unixDiagAnswer, error) {
	c.seq++
	if err := unix.Sendto(c.fd, unixDiagRequest(c.seq, ino), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return unixDiagAnswer{}, err
	}
	buf := make([]byte, 16<<10)
	n, from, err := unix.Recvfrom(c.fd, buf, 0)
	if err != nil {
		return unixDiagAnswer{}, err
	}
	// Only the kernel may answer: port id 0, no multicast group (#7543 review
	// R4). Another process able to send to this port could otherwise forge a
	// socket's state and name. Measured on Linux 6.17: every sock_diag reply,
	// answer or error, comes from port 0.
	if nl, ok := from.(*unix.SockaddrNetlink); !ok || nl.Pid != 0 || nl.Groups != 0 {
		return unixDiagAnswer{}, fmt.Errorf("sock_diag: a reply that did not come from the kernel (%#v)", from)
	}
	return parseUnixDiagReply(buf[:n], c.seq, ino, c.portid)
}

// unixListenerPathsByTable reads which sockets pid holds from /proc/<pid>/fd
// and the listening unix sockets among them from /proc/<pid>/net/unix, which
// lists the sockets of pid's own network namespace. It is the table walk
// UnixListenerPathsByTable documents: positive evidence only. Paths come back
// as bound, relative ones included.
func unixListenerPathsByTable(pid int) ([]string, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, err
	}
	inodes := make(map[string]bool, len(ents))
	for _, e := range ents {
		target, err := os.Readlink(fdDir + "/" + e.Name())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // closed since the listing: no longer held
			}
			return nil, err
		}
		if ino, ok := strings.CutPrefix(target, "socket:["); ok {
			if ino, ok := strings.CutSuffix(ino, "]"); ok {
				inodes[ino] = true
			}
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/unix", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, err
	}
	return parseNetUnixListeners(data, inodes)
}
