//go:build darwin

package sessionenv

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Pure Go: the official darwin artifacts are built with CGO_ENABLED=0, so
// proc_pidpath is unavailable. LOCAL_PEERPID names the peer, and
// kern.procargs2 reports the path string the peer was exec'd with.
func peerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var serr error
	if err := raw.Control(func(fd uintptr) {
		pid, serr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	if serr != nil || pid <= 0 {
		return 0, fmt.Errorf("LOCAL_PEERPID: %v", serr)
	}
	return pid, nil
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

func enableAttestation(net.Conn) error { return nil }

func readAttestedLine(conn net.Conn) ([]byte, int, error) {
	pid, err := peerPID(conn)
	if err != nil {
		return nil, 0, err
	}
	var line []byte
	one := make([]byte, 1)
	for len(line) < claimMaxMessage {
		if _, err := conn.Read(one); err != nil {
			return nil, 0, err
		}
		if one[0] == '\n' {
			return line, pid, nil
		}
		line = append(line, one[0])
	}
	return nil, 0, fmt.Errorf("message too long")
}
