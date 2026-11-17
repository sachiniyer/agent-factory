package proctree

import (
	"os"
	"strconv"
	"strings"
)

// exePath resolves the kernel-attested executable for pid: /proc/<pid>/exe is
// a symlink the KERNEL maintains to the inode being executed — a process can
// write its argv and its task name but not this link (#7491). An executable
// deleted after exec reports with a " (deleted)" suffix; the binary still
// exists as an inode and is still that af build, so the suffix is stripped
// rather than refused.
func exePath(pid int) (string, bool) {
	path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return "", false
	}
	path = strings.TrimSuffix(path, " (deleted)")
	if path == "" {
		return "", false
	}
	return path, true
}
