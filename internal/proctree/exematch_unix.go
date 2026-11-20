//go:build linux || darwin

package proctree

import "syscall"

// exeImageOfFile stats a path through symlinks to the inode the kernel's
// exe records attest — the same object /proc/<pid>/exe names on Linux and
// the executable's text vnode names on darwin.
func exeImageOfFile(path string) (ExeImage, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return ExeImage{}, err
	}
	return ExeImage{dev: uint64(uint32(st.Dev)), ino: st.Ino}, nil
}
