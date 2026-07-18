//go:build darwin

package proctree

import "golang.org/x/sys/unix"

// observedZombie reports whether pid is in state SZOMB, read straight from
// kinfo_proc rather than through any proctree helper: these tests must observe
// the zombie independently of the code under test.
//
// The sysctl answering AT ALL for an exited process is the darwin half of
// #2103 — kern.proc.pid returns a full entry for a corpse, which is exactly
// what made the identity check match one.
func observedZombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false
	}
	return kp.Proc.P_stat == szomb
}
