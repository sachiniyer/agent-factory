package proctree

import (
	"bytes"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// procPIDPathInfo is PROC_PIDPATHINFO, the proc_info flavor returning the
// process's executable path directly — the same kernel-attested source Linux
// reads as /proc/<pid>/exe (#7491). It answers char buf[MAXPATHLEN] rather
// than a struct.
const procPIDPathInfo = 11

// exePath asks the kernel for pid's executable path via proc_info, cgo-free
// for the same reason readWorkingDir and readCPUTime take this syscall shape.
// The kernel refuses it for processes we may not inspect; every failure —
// refusal, short write, missing NUL — reports "cannot resolve", which the
// attestation caller treats as unverified (fail-closed, never a verdict).
func exePath(pid int) (string, bool) {
	const maxPathLen = 1024 // MAXPATHLEN
	var buf [maxPathLen]byte
	n, _, errno := syscall.Syscall6(
		uintptr(unix.SYS_PROC_INFO),
		uintptr(procInfoCallPIDInfo),
		uintptr(pid),
		uintptr(procPIDPathInfo),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(maxPathLen),
	)
	if errno != 0 {
		return "", false
	}
	end := bytes.IndexByte(buf[:], 0)
	if end <= 0 {
		return "", false
	}
	_ = n
	return string(buf[:end]), true
}
