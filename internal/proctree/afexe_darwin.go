package proctree

import (
	"bytes"
	"unsafe"
)

// procPIDPathInfo is PROC_PIDPATHINFO, the proc_info flavor returning the
// process's executable path directly — the same kernel-attested source Linux
// reads as /proc/<pid>/exe (#7491). It answers char buf[MAXPATHLEN] rather
// than a struct.
const procPIDPathInfo = 11

// exePath asks the kernel for pid's executable path via proc_info, through
// the same procInfoRaw helper readCPUTime and openfiles use (Syscall6, never
// RawSyscall6 — proc_find can wait uninterruptibly mid-exec, #7241). The
// kernel refuses it for processes we may not inspect; every failure —
// refusal, short write, missing NUL — reports "cannot resolve", which the
// attestation caller treats as unverified (fail-closed, never a verdict).
func exePath(pid int) (string, bool) {
	const maxPathLen = 1024 // MAXPATHLEN
	var buf [maxPathLen]byte
	n, err := procInfoRaw(procInfoCallPIDInfo, pid, procPIDPathInfo, 0,
		unsafe.Pointer(&buf[0]), maxPathLen)
	if err != nil || n <= 0 {
		return "", false
	}
	end := bytes.IndexByte(buf[:n], 0)
	if end <= 0 {
		return "", false
	}
	return string(buf[:end]), true
}
