//go:build linux

package daemon

import "golang.org/x/sys/unix"

// waitExitedUnreaped blocks until pid, a child of this process, has exited,
// and leaves it unreaped (WNOWAIT): its pid, and with it its process group's
// number, stays reserved until a later wait reaps it.
func waitExitedUnreaped(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err
		}
	}
}
