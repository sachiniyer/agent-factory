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
