package proctree

import (
	"strconv"
	"syscall"
)

// exeImageForMatch reads the kernel-attested executable for pid:
// /proc/<pid>/exe is a link the KERNEL maintains to the inode being
// executed — a process can write its argv and its task title but not this
// link (#7491). Stat'ing the link answers the inode itself, so an
// unlinked running image still reports its real identity, and the errno
// is kept for the verdict: ENOENT is a dead entry, EACCES a refused one.
func exeImageForMatch(pid int) (ExeImage, error) {
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/"+strconv.Itoa(pid)+"/exe", &st); err != nil {
		return ExeImage{}, err
	}
	return ExeImage{dev: uint64(uint32(st.Dev)), ino: st.Ino}, nil
}
