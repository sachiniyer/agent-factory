//go:build darwin

package proctree

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// XNU constants for the two inputs to sysctl_procargsx's env redaction
// (bsd/kern/kern_sysctl.c). The env section is omitted for a same-uid process
// that is not us, carries CS_RESTRICT, runs while SIP's dtrace restriction is
// in force, and is being read by a caller with no Apple-private entitlement.
// Every Apple system binary (/bin/sh, /bin/zsh, /usr/bin/*) carries
// CS_RESTRICT, so on a SIP-enabled Mac all of them are withheld (#3584).
const (
	csOpsStatus                = 0          // CS_OPS_STATUS, bsd/sys/codesign.h
	csRestrict                 = 0x00000800 // CS_RESTRICT
	csrSyscallCheck            = 0          // CSR_SYSCALL_CHECK, bsd/sys/csr.h
	csrAllowUnrestrictedDtrace = 1 << 5     // CSR_ALLOW_UNRESTRICTED_DTRACE
)

func withheldEnvCause(pid int) (string, bool) {
	// One KERN_PROCARGS2 read, so argv and env describe the same answer: argv
	// served with no env section is the shape the redaction produces.
	argv, env, err := procArgs(pid)
	if err != nil || len(argv) == 0 || len(env) != 0 {
		return "", false
	}
	flags, err := codeSigningFlags(pid)
	if err != nil || flags&csRestrict == 0 {
		return "", false
	}
	restricted, err := sipRestrictsDtrace()
	if err != nil || !restricted {
		return "", false
	}
	return "it is a code-signing-restricted system binary and System Integrity Protection is on", true
}

// codeSigningFlags is csops(pid, CS_OPS_STATUS): the code-signing status word
// the kernel's cs_restricted reads. It needs no privilege for a same-uid pid.
func codeSigningFlags(pid int) (uint32, error) {
	var flags uint32
	_, _, errno := syscall.Syscall6(
		uintptr(unix.SYS_CSOPS),
		uintptr(pid),
		csOpsStatus,
		uintptr(unsafe.Pointer(&flags)),
		unsafe.Sizeof(flags),
		0, 0,
	)
	if errno != 0 {
		return 0, errno
	}
	return flags, nil
}

// sipRestrictsDtrace asks the kernel the exact question sysctl_procargsx
// asks, csr_check(CSR_ALLOW_UNRESTRICTED_DTRACE), through csrctl's check op
// rather than parsing `csrutil status`. 0 means allowed (SIP off, or dtrace
// unrestricted) and EPERM means restricted.
func sipRestrictsDtrace() (bool, error) {
	mask := uint32(csrAllowUnrestrictedDtrace)
	_, _, errno := syscall.Syscall(
		uintptr(unix.SYS_CSRCTL),
		csrSyscallCheck,
		uintptr(unsafe.Pointer(&mask)),
		unsafe.Sizeof(mask),
	)
	switch errno {
	case 0:
		return false, nil
	case syscall.EPERM:
		return true, nil
	default:
		return false, errno
	}
}
