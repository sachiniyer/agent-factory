//go:build linux

package sessionenv

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func peerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil || cred == nil {
		return 0, fmt.Errorf("SO_PEERCRED: %v", serr)
	}
	return int(cred.Pid), nil
}

// platformExeIdentity reads /proc/<pid>/exe. The " (deleted)" suffix is trusted
// only when the file really has no links: a live file literally named
// "af (deleted)" reads identically, and nlink==0 is what tells them apart.
func platformExeIdentity(pid int) (exeID, error) {
	link := fmt.Sprintf("/proc/%d/exe", pid)
	target, err := os.Readlink(link)
	if err != nil {
		return exeID{}, err
	}
	var st unix.Stat_t
	if err := unix.Stat(link, &st); err != nil {
		return exeID{}, err
	}
	return normalizeExeLink(target, uint64(st.Nlink), st.Ino), nil
}
