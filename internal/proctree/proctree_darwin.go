//go:build darwin

package proctree

import (
	"bytes"
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The darwin backend reads the kernel process table through sysctl, which is
// the platform's answer to Linux's /proc. See proctree.go for the contract
// every backend owes its callers.
//
// Sources, and why each:
//
//   - kern.proc.all (KERN_PROC_ALL) — the whole table as []kinfo_proc, giving
//     pid, ppid, start time and comm in one call.
//   - getsid(2) — the kernel session id. kinfo_proc carries e_sess, but it is a
//     POINTER into kernel memory, not an id, so it is useless to us; getsid is
//     the only readable source. It costs one syscall per process, which is why
//     snapshot pays it once rather than callers paying it per lookup.
//   - kern.procargs2 (KERN_PROCARGS2) — argv AND envp with their NUL
//     separators intact. This is the darwin equivalent of /proc/<pid>/cmdline
//     and /proc/<pid>/environ, and preserving those separators is what makes
//     spaced-install detection work here (#1942).
//   - proc_info(PROC_PIDTASKINFO) — cumulative CPU. kinfo_proc's p_uticks /
//     p_sticks are legacy fields the modern XNU kernel does not populate, so
//     reading them would report a confident 0% for a process pegging a core.
//   - proc_info(PROC_PIDVNODEPATHINFO) — the working directory, this platform's
//     answer to /proc/<pid>/cwd. kinfo_proc carries no cwd at all, so there is no
//     cheaper source. Its buffer is decoded in vnodepathinfo.go (#2050).
//
// Nothing here reports a read failure as an empty result: an unreadable
// process table returns an error, and an unreadable CPU counter returns
// ErrCPUUnknown rather than zero.

// snapshot reads the whole kernel process table via sysctl.
func snapshot() (map[int]Process, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("reading the kernel process table (kern.proc.all): %w", err)
	}
	if len(kps) == 0 {
		// Unreachable on a running system — launchd is pid 1 and always
		// present — so an empty table means the read did not work rather
		// than that nothing is running. Saying so is the whole point of
		// this package (#1939): an empty snapshot handed back as success is
		// how blindness gets rendered as health.
		return nil, errors.New("kern.proc.all returned an empty process table, which cannot happen on a running system")
	}
	procs := make(map[int]Process, len(kps))
	for i := range kps {
		p, ok := processFromKinfo(&kps[i])
		if !ok {
			continue
		}
		procs[p.PID] = p
	}
	return procs, nil
}

// szomb is SZOMB from <sys/proc.h>: the p_stat of a process that has exited
// and is waiting for its parent to collect it. x/sys/unix exposes the P_stat
// field but not the state constants, so the value is spelled out here.
const szomb = 5

// readProc reads one process's kinfo_proc. Returns an error when the pid names
// no live process, which is what makes it usable as an identity check.
func readProc(pid int) (Process, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Process{}, err
	}
	// kern.proc.pid answers for zombies too, with ppid, session and start time
	// all intact — so without this the identity check matches a process that
	// has already exited. Same defect as the Linux state field, same fix
	// (#2103); see ErrProcessExited.
	if kp.Proc.P_stat == szomb {
		return Process{}, ErrProcessExited
	}
	p, ok := processFromKinfo(kp)
	if !ok {
		return Process{}, fmt.Errorf("kern.proc.pid returned no usable entry for pid %d", pid)
	}
	return p, nil
}

// processFromKinfo converts one kinfo_proc into a Process. Zombies are refused
// for the reason readProc gives: they are exited processes that every
// existence-shaped check still finds.
func processFromKinfo(kp *unix.KinfoProc) (Process, bool) {
	pid := int(kp.Proc.P_pid)
	if pid <= 0 || kp.Proc.P_stat == szomb {
		return Process{}, false
	}
	// P_starttime is wall-clock (a timeval), unlike Linux's ticks-since-boot,
	// so it serves as both the identity stamp and the age basis. Nano() keeps
	// the field-width differences between arm64 and amd64 out of this file.
	startedAt := time.Unix(0, kp.Proc.P_starttime.Nano())
	return Process{
		PID:       pid,
		PPID:      int(kp.Eproc.Ppid),
		StartID:   uint64(kp.Proc.P_starttime.Nano()),
		StartedAt: startedAt,
		SID:       sessionID(pid),
		Comm:      cString(kp.Proc.P_comm[:]),
	}, true
}

// sessionID returns pid's kernel session id, or sidUnknown when the kernel
// will not say.
//
// The failure value matters more than it looks: SessionMembers selects every
// process sharing an id, so a failure that returned 0 would make every
// unreadable process look like a member of session 0 — and reap.go feeds that
// set straight into KillEscalating. sidUnknown is a value no real session
// holds, and SessionMembers refuses to match it.
func sessionID(pid int) int {
	sid, err := unix.Getsid(pid)
	if err != nil {
		return sidUnknown
	}
	return sid
}

// readUID returns the real uid owning pid, from kinfo_proc's process
// credentials.
//
// p_ruid (the REAL uid) rather than e_ucred's effective uid, to match what
// Linux's /proc/<pid> ownership reports for the processes af inspects, and to
// match os.Getuid() — which is what the caller compares against. Nothing af
// inspects is setuid, so the two coincide in practice; picking the one the
// comparison is written against keeps it correct if that ever stops being true.
func readUID(pid int) (int, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, false
	}
	return int(kp.Eproc.Pcred.P_ruid), true
}

// readEnviron returns whatever the kernel gives us for pid's environment. It
// does NOT try to work out in advance whether we are allowed to have it.
//
// There WAS a permission gate here (uid match + P_SUGID), and deleting it is
// the fix. It modelled XNU's rule — and a model of someone else's policy is
// wrong the moment that policy has a clause you did not know about. It always
// has one. XNU's sysctl_procargsx withholds the environment on at least two
// INDEPENDENT grounds: a uid mismatch, and a cs_restricted (code-signing
// restricted) target — which SIP makes ordinary on a real Mac. The gate modelled
// the first and missed the second, so a same-uid cs_restricted process walked
// straight through it, came back with an empty environment, and was read as a
// definite "this variable is not set". The same fabricated negative, through the
// door I had not modelled. Adding a cs_restricted clause would just be the same
// mistake with one more clause; entitlements, hardened runtime and platform
// binaries are all waiting behind it.
//
// So: ask, then look at what came back. The kernel is the authority on its own
// policy, and our job is not to predict it — only to notice when we did not get
// an answer. Environ does that classification, in one place, for every ground at
// once (see Environ).
func readEnviron(pid int) ([]string, error) {
	_, env, err := procArgs(pid)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnvUnreadable, err)
	}
	return env, nil
}

// readArgv returns pid's argv with boundaries intact.
func readArgv(pid int) ([]string, error) {
	argv, _, err := procArgs(pid)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("pid %d has no argv", pid)
	}
	return argv, nil
}

// procArgs reads and parses KERN_PROCARGS2 for pid.
//
// The kernel withholds this from us for reasons of its own — a foreign uid and
// a code-signing-restricted target are two, and the list is Apple's to extend —
// so a failure here is routine rather than a malfunction. Deliberately NOT
// stated as a rule: writing down when the kernel refuses is how the last bug
// got in. What matters is that a refusal is an ERROR and never an empty result,
// and that a refusal it declines to report at all is caught by Environ's
// classification instead.
func procArgs(pid int) (argv []string, env []string, err error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, nil, fmt.Errorf("reading argv for pid %d (kern.procargs2): %w", pid, err)
	}
	return parseProcArgs2(buf)
}

// cString converts a NUL-padded fixed-size kernel char array to a string.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// procTaskInfo mirrors darwin's struct proc_taskinfo (sys/proc_info.h). It is
// declared here rather than pulled from x/sys because x/sys does not wrap
// proc_info. The field order and widths are load-bearing: the kernel writes
// this struct by offset, and taskInfoSize below is checked against what the
// syscall says it wrote.
type procTaskInfo struct {
	VirtualSize      uint64
	ResidentSize     uint64
	TotalUser        uint64 // nanoseconds
	TotalSystem      uint64 // nanoseconds
	ThreadsUser      uint64
	ThreadsSystem    uint64
	Policy           int32
	Faults           int32
	Pageins          int32
	CowFaults        int32
	MessagesSent     int32
	MessagesReceived int32
	SyscallsMach     int32
	SyscallsUnix     int32
	Csw              int32
	Threadnum        int32
	Numrunning       int32
	Priority         int32
}

const (
	// procInfoCallPIDInfo is PROC_INFO_CALL_PIDINFO: the proc_info callnum
	// that libproc's proc_pidinfo() wraps.
	procInfoCallPIDInfo = 2
	// procPIDTaskInfo is PROC_PIDTASKINFO, the flavor returning procTaskInfo.
	procPIDTaskInfo = 4
	// procPIDVnodePathInfo is PROC_PIDVNODEPATHINFO, the flavor returning
	// struct proc_vnodepathinfo — the process's cwd and root directory.
	procPIDVnodePathInfo = 9
)

// readCPUTime returns pid's cumulative user+system CPU time.
//
// This goes through the proc_info syscall directly because the alternatives do
// not survive contact with this repo: libproc's proc_pidinfo() needs cgo, and
// darwin builds here run cgo-free (a -race build on darwin has no cgo at all).
// kinfo_proc's legacy tick counters are not an option either — the modern
// kernel leaves them at zero, which would report every runaway process as idle.
func readCPUTime(pid int) (time.Duration, error) {
	var ti procTaskInfo
	size := unsafe.Sizeof(ti)
	n, _, errno := syscall.Syscall6(
		uintptr(unix.SYS_PROC_INFO),
		uintptr(procInfoCallPIDInfo),
		uintptr(pid),
		uintptr(procPIDTaskInfo),
		0,
		uintptr(unsafe.Pointer(&ti)),
		size,
	)
	if errno != 0 {
		return 0, fmt.Errorf("proc_info(PROC_PIDTASKINFO) for pid %d: %w", pid, errno)
	}
	if n != uintptr(size) {
		// A short write means the kernel's struct is not the one declared
		// above. Report it rather than reading whatever landed in the
		// fields, which would be a plausible-looking wrong number.
		return 0, fmt.Errorf("proc_info(PROC_PIDTASKINFO) for pid %d wrote %d bytes, want %d", pid, n, size)
	}
	return time.Duration(ti.TotalUser + ti.TotalSystem), nil
}

// readWorkingDir reads pid's cwd from proc_info(PROC_PIDVNODEPATHINFO), the
// darwin equivalent of reading Linux's /proc/<pid>/cwd symlink (#2050).
//
// This goes through the proc_info syscall rather than libproc's proc_pidinfo()
// for the same reason readCPUTime does: the wrapper needs cgo, and darwin builds
// here run cgo-free. The flavor is the only difference between the two calls —
// PROC_PIDVNODEPATHINFO instead of PROC_PIDTASKINFO — so this reuses a syscall
// shape the package already relies on rather than introducing a cgo dependency
// on the one path that must keep cross-compiling.
//
// The kernel refuses this for processes we do not own (and for others it does
// not explain — SIP and code-signing restrictions among them). Per this
// package's rule, that refusal is NOT modelled in advance: we ask, and classify
// what comes back. Every failure — a refusal, a short write, a path that does
// not validate — becomes the honest unknown, which the callers treat as "cannot
// resolve" and skip (#1044). Deliberately no error channel: WorkingDir's
// contract is two-valued, and every caller here already handles false.
//
// The decode is NOT done in this file. It lives in vnodepathinfo.go, untagged,
// so Linux CI exercises the offset arithmetic that this file can only feed —
// see that file for why the offsets are validated rather than trusted, and for
// what a wrong one would cost on the destructive reap path.
func readWorkingDir(pid int) (string, bool) {
	var buf [vnodePathInfoSize]byte
	n, _, errno := syscall.Syscall6(
		uintptr(unix.SYS_PROC_INFO),
		uintptr(procInfoCallPIDInfo),
		uintptr(pid),
		uintptr(procPIDVnodePathInfo),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if errno != 0 {
		// Includes the forward-compatible case: proc_info returns ENOMEM when the
		// buffer is smaller than the flavor's struct, so if a future macOS grows
		// proc_vnodepathinfo this reports unknown and the reap goes back to
		// no-opping — the same safe degradation it had before #2050, never a
		// half-decoded path.
		return "", false
	}
	if n != uintptr(len(buf)) {
		// A short write means the kernel's struct is not the one vnodepathinfo.go
		// describes, so the bytes at the cwd offset are not the cwd. Same reasoning
		// as readCPUTime's length check, with a sharper consequence: this value is
		// what reapWorktreeWriters signals on.
		return "", false
	}
	return cwdFromVnodePathInfo(buf[:n])
}
