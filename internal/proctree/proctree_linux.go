//go:build linux

package proctree

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The Linux backend reads /proc. See proctree.go for the contract every
// backend owes its callers; the short version is that "cannot read" must never
// be reported as "nothing there".

// clkTck is the kernel clock-tick rate (_SC_CLK_TCK) used to convert /proc
// stat fields to seconds. It is 100 on every mainstream Linux configuration;
// Go's runtime makes the same assumption. Only used for approximate CPU/age
// reporting, never for identity checks.
const clkTck = 100

// snapshot reads every /proc/<pid>/stat once, then stamps each entry with a
// wall-clock start time derived from the single /proc/uptime read below.
//
// Boot time is resolved ONCE per snapshot rather than per process: it is the
// same instant for all of them, and re-reading it per entry would let a
// snapshot's processes disagree about when the machine booted.
//
// AN UNREADABLE /proc/uptime IS NOT FATAL, and that is deliberate. It used to
// fail the whole snapshot, which meant a procfs mounted `subset=pid` — where
// /proc/<pid>/stat reads fine but /proc/uptime is not there — reported "cannot
// read the process table" and turned off orphan reaping and doctor's process
// map entirely. That is this package's own disease pointing the other way:
// manufacturing NO DATA where data exists, having been built to stop
// manufacturing health where no data exists.
//
// So the table still comes back; only StartedAt is left zero. It feeds nothing
// but CPUFraction, which already reports a zero StartedAt as unknown — so we
// lose a nice-to-have (runaway-CPU detection says it could not measure, out
// loud) and keep the load-bearing part (reaping, orphan and escape detection).
func snapshot() (map[int]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	booted, bootErr := bootTime()
	procs := make(map[int]Process, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, err := readProc(pid)
		if err != nil {
			// Exited between ReadDir and the stat read, or exited already and
			// is waiting to be collected (ErrProcessExited). A zombie has no
			// children of its own — the kernel reparents them when a process
			// exits, not when it is collected — so dropping it here loses no
			// descendant, and keeping it would only hand the reapers a corpse
			// to kill and then wait for. Skip.
			continue
		}
		if bootErr == nil {
			p.StartedAt = booted.Add(time.Duration(float64(p.StartID) / clkTck * float64(time.Second)))
		}
		procs[pid] = p
	}
	return procs, nil
}

// uptimePath is /proc/uptime, indirected so tests can reproduce a procfs that
// does not serve it (subset=pid) without mounting one.
var uptimePath = "/proc/uptime"

// bootTime returns the wall-clock instant the machine booted, from
// /proc/uptime.
func bootTime() (time.Time, error) {
	data, err := os.ReadFile(uptimePath)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading %s: %w", uptimePath, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return time.Time{}, errors.New("malformed /proc/uptime")
	}
	uptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("malformed /proc/uptime: %w", err)
	}
	return time.Now().Add(-time.Duration(uptime * float64(time.Second))), nil
}

// readProc parses /proc/<pid>/stat. Format: `pid (comm) state ppid ...`.
// Comm may itself contain spaces and ')' — the parse anchors on the LAST ')'.
//
// A zombie is reported as ErrProcessExited rather than returned as a Process
// (#2103). The state field is the ONLY thing that distinguishes a corpse here:
// a zombie keeps its stat entry, its ppid, its session id and — the part that
// defeated the identity check — its unchanging start time, so PID+StartID match
// perfectly for a process that has already exited.
//
// StartedAt is deliberately left zero here: it needs boot time, which is a
// per-snapshot fact rather than a per-process one. snapshot() fills it in.
func readProc(pid int) (Process, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Process{}, err
	}
	open := bytes.IndexByte(data, '(')
	close_ := bytes.LastIndexByte(data, ')')
	if open < 0 || close_ < open || close_+2 > len(data) {
		return Process{}, fmt.Errorf("malformed stat for pid %d", pid)
	}
	comm := string(data[open+1 : close_])
	fields := strings.Fields(string(data[close_+2:]))
	// fields[0] is stat field 3 (state); stat field N lives at fields[N-3].
	const (
		idxPPID  = 4 - 3
		idxSID   = 6 - 3
		idxStart = 22 - 3
	)
	if len(fields) <= idxStart {
		return Process{}, fmt.Errorf("truncated stat for pid %d", pid)
	}
	// Z is the zombie state; X/x is EXIT_DEAD, the sliver between the two halves
	// of teardown. Neither will ever run again or answer a signal.
	switch fields[0] {
	case "Z", "X", "x":
		return Process{}, ErrProcessExited
	}
	ppid, err := strconv.Atoi(fields[idxPPID])
	if err != nil {
		return Process{}, fmt.Errorf("bad ppid for pid %d: %w", pid, err)
	}
	sid, err := strconv.Atoi(fields[idxSID])
	if err != nil {
		return Process{}, fmt.Errorf("bad sid for pid %d: %w", pid, err)
	}
	start, err := strconv.ParseUint(fields[idxStart], 10, 64)
	if err != nil {
		return Process{}, fmt.Errorf("bad starttime for pid %d: %w", pid, err)
	}
	return Process{
		PID:     pid,
		PPID:    ppid,
		SID:     sid,
		StartID: start,
		Comm:    comm,
	}, nil
}

// readCPUTime returns pid's cumulative user+system CPU from /proc/<pid>/stat
// fields 14 and 15.
func readCPUTime(pid int) (time.Duration, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	close_ := bytes.LastIndexByte(data, ')')
	if close_ < 0 || close_+2 > len(data) {
		return 0, fmt.Errorf("malformed stat for pid %d", pid)
	}
	fields := strings.Fields(string(data[close_+2:]))
	// stat field N lives at fields[N-3]; 14 is utime and 15 is stime.
	const (
		idxUtime = 14 - 3
		idxStime = 15 - 3
	)
	if len(fields) <= idxStime {
		return 0, fmt.Errorf("truncated stat for pid %d", pid)
	}
	utime, err := strconv.ParseUint(fields[idxUtime], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad utime for pid %d: %w", pid, err)
	}
	stime, err := strconv.ParseUint(fields[idxStime], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad stime for pid %d: %w", pid, err)
	}
	return time.Duration(float64(utime+stime) / clkTck * float64(time.Second)), nil
}

// readUID returns the uid owning pid, from the ownership of its /proc
// directory. That directory is owned by the process's effective uid, which for
// every process af inspects (nothing here is setuid) is also its real uid.
func readUID(pid int) (int, bool) {
	fi, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// readEnviron reads /proc/<pid>/environ and splits it on its NUL separators.
//
// Linux needs no permission gate of its own (compare the darwin backend, which
// does): /proc/<pid>/environ is mode 0400 owned by the process, so a refusal
// arrives honestly as EACCES rather than as a silently empty result. The error
// is wrapped in ErrEnvUnreadable so callers classify it as "could not read"
// rather than "not set" — the distinction the reaping paths turn on.
func readEnviron(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnvUnreadable, err)
	}
	return splitNUL(data), nil
}

// readArgv reads /proc/<pid>/cmdline, whose NUL separators preserve the argv
// boundaries a spaced binary path depends on (#1214).
func readArgv(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	argv := splitNUL(data)
	if len(argv) == 0 {
		// Kernel threads and reaped-but-unwaited zombies have an empty
		// cmdline. Nothing to classify; say so rather than returning a
		// zero-length argv that reads as a successful parse.
		return nil, fmt.Errorf("pid %d has no argv", pid)
	}
	return argv, nil
}

// splitNUL splits a NUL-separated /proc blob, dropping the trailing empty
// element left by the final NUL terminator.
func splitNUL(data []byte) []string {
	parts := strings.Split(string(data), "\x00")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// readWorkingDir reads pid's cwd from /proc/<pid>/cwd. The symlink is readable
// only by the process owner (or root), so a foreign process reports false —
// the honest unknown, which WorkingDir's caller handles as "cannot resolve".
func readWorkingDir(pid int) (string, bool) {
	dir, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return "", false
	}
	return dir, true
}
