package proctree

import (
	"unsafe"
)

// exeImageForMatch reads the kernel-attested executable for pid the way
// exeIdentity does for the attestation memo: PROC_PIDPATHINFO names the
// executable path, and the vnode walk confirms which file the process's
// own memory map is executing — the inode, never the name (#7492). The
// errno from the path read is kept for the verdict: ESRCH is a dead
// process, EPERM a refused one.
func exeImageForMatch(pid int) (ExeImage, error) {
	var buf [procPIDPathInfoMaxSize]byte
	_, err := procInfoRaw(procInfoCallPIDInfo, pid, procPIDPathInfo, 0,
		unsafe.Pointer(&buf[0]), procPIDPathInfoMaxSize)
	if err != nil {
		return ExeImage{}, err
	}
	path := cString(buf[:procPIDPathInfoMaxSize])
	if path == "" {
		return ExeImage{}, errExeImageUnreadable
	}
	dev, ino, ok := exeTextVnode(pid, path)
	if !ok {
		return ExeImage{}, errExeImageUnreadable
	}
	return ExeImage{dev: uint64(uint32(dev)), ino: ino}, nil
}
