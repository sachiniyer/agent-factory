package proctree

import (
	"os"
	"strconv"
)

// exeOpen opens the kernel-attested executable for pid: /proc/<pid>/exe is a
// symlink the KERNEL maintains to the inode being executed — a process can
// write its argv and its task name but not this link (#7491). The read opens
// the link, it does not resolve it: readlink would answer "/path (deleted)"
// for an unlinked image, and reopening that path asks whatever file holds
// the NAME now — a deleted binary fails outright and a replaced one yields
// the replacement's bytes, never the running process's (#7303's
// reopen-by-path class, #7492). Opening the link always binds to the inode
// the kernel is executing, named or not.
func exeOpen(pid int) (*os.File, bool) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return nil, false
	}
	return f, true
}
