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

// procPIDPathInfoMaxSize is PROC_PIDPATHINFO_MAXSIZE from XNU's
// <sys/proc_info.h> — 4 * MAXPATHLEN. The kernel bounds the buffer to
// [MAXPATHLEN, MAXSIZE] (ENOMEM under, EOVERFLOW over); the top of the range
// is what vn_getpath_fsenter needs for the longest path it can return.
const procPIDPathInfoMaxSize = 4 * 1024

// exePath asks the kernel for pid's executable path via proc_info, through
// the same procInfoRaw helper readCPUTime and openfiles use (Syscall6, never
// RawSyscall6 — proc_find can wait uninterruptibly mid-exec, #7241).
//
// PROC_PIDPATHINFO answers differently from the struct flavors: its retval is
// UNUSED — proc_pidpathinfo_internal never writes it — so a SUCCESSFUL read
// returns 0 with the NUL-terminated path copied into the buffer, exactly what
// libproc's proc_pidpath reads with strlen(buffer), not the return value
// (xnu bsd/kern/proc_info.c). Interpreting that 0 as "no answer" made every
// real af build unverifiable on darwin (#7492). The kernel refuses the call
// for processes we may not inspect via errno; a non-error answer whose first
// byte is NUL is no path at all. Both report "cannot resolve", which the
// attestation caller treats as unverified (fail-closed, never a verdict).
func exePath(pid int) (string, bool) {
	var buf [procPIDPathInfoMaxSize]byte
	_, err := procInfoRaw(procInfoCallPIDInfo, pid, procPIDPathInfo, 0,
		unsafe.Pointer(&buf[0]), procPIDPathInfoMaxSize)
	if err != nil {
		return "", false
	}
	end := bytes.IndexByte(buf[:], 0)
	if end <= 0 {
		return "", false
	}
	return string(buf[:end]), true
}
