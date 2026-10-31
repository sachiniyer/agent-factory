//go:build darwin

package sessionenv

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// peerState is the peer identity read once, when the connection is made. macOS
// answers LOCAL_PEERPID only while the peer still holds its end open: a client
// that has sent its request and then waits for the answer is fine, but a server
// that has written its last reply and closed (every claim ends that way) leaves
// the client's getsockopt with ENOTCONN. Reading lazily, at the first read after
// the request, raced exactly that close and refused a live shim's claim (#6455).
// The identity is the kernel's record of who connected, so it does not change
// afterwards; reading it before any exchange removes the race.
type peerState struct{ pid int }

// errPeerGone is the refusal for a peer that was already gone when its identity
// was read, so it is never confused with a failed identity check.
var errPeerGone = errors.New("the claim peer closed the connection before it could be identified")

// Pure Go: the official darwin artifacts are built with CGO_ENABLED=0, so
// proc_pidpath is unavailable. LOCAL_PEERPID names the peer, and
// kern.procargs2 reports the path string the peer was exec'd with.
func peerPID(conn net.Conn) (int, error) {
	pid, _, err := peerIdentity(conn)
	return pid, err
}

// peerIdentity reads the peer's pid (LOCAL_PEERPID) and uid (LOCAL_PEERCRED).
func peerIdentity(conn net.Conn) (pid int, uid uint32, err error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		pid, serr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if serr != nil {
			serr = fmt.Errorf("LOCAL_PEERPID: %w", serr)
			return
		}
		var cred *unix.Xucred
		if cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED); serr != nil {
			serr = fmt.Errorf("LOCAL_PEERCRED: %w", serr)
			return
		}
		uid = cred.Uid
	}); err != nil {
		return 0, 0, err
	}
	if serr != nil {
		if errors.Is(serr, unix.ENOTCONN) {
			return 0, 0, fmt.Errorf("%w (%v)", errPeerGone, serr)
		}
		return 0, 0, serr
	}
	if pid <= 0 {
		return 0, 0, fmt.Errorf("LOCAL_PEERPID returned pid %d", pid)
	}
	return pid, uid, nil
}

// platformExeIdentity returns the exec path of pid. It is the path string given
// to execve, not the vnode, so it is a weaker identity than Linux's: a process
// exec'd through a path equal to the shim's is accepted. Both sides are
// resolved through symlinks first.
func platformExeIdentity(pid int) (exeID, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return exeID{}, err
	}
	if len(buf) < 5 {
		return exeID{}, fmt.Errorf("kern.procargs2 for pid %d is too short", pid)
	}
	rest := buf[4:] // skip argc
	end := bytes.IndexByte(rest, 0)
	if end <= 0 {
		return exeID{}, fmt.Errorf("kern.procargs2 for pid %d has no exec path", pid)
	}
	path := string(rest[:end])
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if pid == os.Getpid() {
		if exe, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				path = resolved
			}
		}
	}
	return exeID{path: path}, nil
}

// perMessageAttestation is false: macOS offers LOCAL_PEERPID (per connection)
// and no per-message sender attestation, so the answering process cannot be
// told apart from the process that created the socket. The check below is
// therefore only as strong as per-connection identity: a process that binds the
// socket, forks a child that serves it, and execs the real af in the creator
// defeats it. Documented residual (#4731); nothing here pretends otherwise.
const perMessageAttestation = false

// enableAttestation reads the peer's identity now, at connect/accept, and
// refuses a peer that is another user.
func enableAttestation(conn net.Conn) (peerState, error) {
	pid, uid, err := peerIdentity(conn)
	if err != nil {
		return peerState{}, err
	}
	if uid != uint32(os.Getuid()) {
		return peerState{}, fmt.Errorf("the claim peer (pid %d) runs as uid %d, not this user", pid, uid)
	}
	return peerState{pid: pid}, nil
}

func readAttestedLine(conn net.Conn, st peerState) ([]byte, int, error) {
	var line []byte
	one := make([]byte, 1)
	for len(line) < claimMaxMessage {
		if _, err := conn.Read(one); err != nil {
			return nil, 0, err
		}
		if one[0] == '\n' {
			return line, st.pid, nil
		}
		line = append(line, one[0])
	}
	return nil, 0, fmt.Errorf("message too long")
}
