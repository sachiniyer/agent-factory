package proctree

import (
	"errors"
	"fmt"
	"syscall"
)

// readProcArgs2 is the Darwin sysctl boundary. Keeping the syscall injectable
// lets every platform exercise exit races without relying on their timing.
// Linux continues to read /proc and does not call this helper.
func readProcArgs2(pid int, sysctl func(string, ...int) ([]byte, error), lookup func(int) (Process, error)) (argv, env []string, err error) {
	buf, err := sysctl("kern.procargs2", pid)
	if err != nil {
		gone := errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ESRCH)
		// XNU's sysctl_procargsx also returns EINVAL when proc_find cannot
		// find the PID (including zombies). But it uses the SAME errno for
		// uid restrictions and invalid buffers, so require positive absence.
		// https://github.com/apple-oss-distributions/xnu/blob/f6217f891ac0bb64f3d375211650a4c1ff8ca1ea/bsd/kern/kern_sysctl.c#L1300-L1384
		if errors.Is(err, syscall.EINVAL) {
			_, lookupErr := lookup(pid)
			gone = isGone(lookupErr)
		}
		if gone {
			return nil, nil, fmt.Errorf("%w: reading argv for pid %d (kern.procargs2): %w", ErrProcessExited, pid, err)
		}
		return nil, nil, fmt.Errorf("reading argv for pid %d (kern.procargs2): %w", pid, err)
	}
	return parseProcArgs2(buf)
}
