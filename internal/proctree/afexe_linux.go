package proctree

import (
	"os"
	"strconv"
	"syscall"
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

// exeIdentity stats the same kernel link exeOpen reads: /proc/<pid>/exe
// resolves to the inode the process is executing NOW — the running image
// even after its name is unlinked, and a new inode the moment the process
// execs. The memo compares it against the inode a positive verdict was
// read from, so an exec invalidates the cached verdict (#7496).
func exeIdentity(pid int) (exeID, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/"+strconv.Itoa(pid)+"/exe", &st); err != nil {
		return exeID{}, false
	}
	return exeID{dev: uint64(uint32(st.Dev)), ino: st.Ino}, true
}
