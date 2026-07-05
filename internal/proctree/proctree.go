// Package proctree inspects the local process table via /proc. It exists so
// session teardown can reap every descendant of a tmux pane (#1104) and so
// `af doctor` can trace leaked processes back to the session that spawned
// them.
//
// Every operation that signals a process guards against PID reuse by pairing
// the PID with its kernel start time (/proc/<pid>/stat field 22): a
// (pid, starttime) pair names a process *instance*, not just a slot. A PID
// that has been recycled since the snapshot fails the identity check and is
// never signalled.
//
// On non-Linux platforms (no /proc) Snapshot returns an error and callers
// degrade to doing nothing — reaping and doctor scans are best-effort
// diagnostics, never load-bearing for correctness.
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

// clkTck is the kernel clock-tick rate (_SC_CLK_TCK) used to convert
// /proc stat fields to seconds. It is 100 on every mainstream Linux
// configuration; Go's runtime makes the same assumption. Only used for
// approximate CPU/age reporting, never for identity checks.
const clkTck = 100

// Process identifies one live process at snapshot time.
type Process struct {
	PID  int
	PPID int
	// StartTicks is /proc/<pid>/stat field 22 (starttime): clock ticks
	// between boot and process start. Together with PID it uniquely
	// identifies a process instance until reboot.
	StartTicks uint64
	// SID is the kernel session id (stat field 6). Every process spawned
	// inside a tmux pane shares the pane root's SID unless it called
	// setsid, so SID membership proves pane ancestry even after a process
	// is reparented to init.
	SID int
	// Comm is the kernel task name (stat field 2, max 15 chars).
	Comm string
	// UTicks and STicks are cumulative user/system CPU ticks (stat
	// fields 14/15) at snapshot time.
	UTicks uint64
	STicks uint64
}

// Snapshot reads the whole process table once. The result is a point-in-time
// view: processes may die (or PIDs be recycled) immediately after. Callers
// must use Signal/AliveSame — which re-verify identity — before acting on an
// entry.
func Snapshot() (map[int]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	procs := make(map[int]Process, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, err := readStat(pid)
		if err != nil {
			// Process exited between ReadDir and the stat read; skip.
			continue
		}
		procs[pid] = p
	}
	return procs, nil
}

// readStat parses /proc/<pid>/stat. Format: `pid (comm) state ppid ...`.
// Comm may itself contain spaces and ')' — the parse anchors on the LAST ')'.
func readStat(pid int) (Process, error) {
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
		idxUtime = 14 - 3
		idxStime = 15 - 3
		idxStart = 22 - 3
	)
	if len(fields) <= idxStart {
		return Process{}, fmt.Errorf("truncated stat for pid %d", pid)
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
	// CPU counters are for reporting only; a parse failure degrades to 0.
	utime, _ := strconv.ParseUint(fields[idxUtime], 10, 64)
	stime, _ := strconv.ParseUint(fields[idxStime], 10, 64)
	return Process{PID: pid, PPID: ppid, SID: sid, StartTicks: start, Comm: comm, UTicks: utime, STicks: stime}, nil
}

// TreeOf returns root plus every descendant of root present in snap, in BFS
// order (root first). Returns nil when root is not in the snapshot.
func TreeOf(snap map[int]Process, root int) []Process {
	rp, ok := snap[root]
	if !ok {
		return nil
	}
	children := make(map[int][]int, len(snap))
	for pid, p := range snap {
		children[p.PPID] = append(children[p.PPID], pid)
	}
	tree := []Process{rp}
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if seen[c] {
				continue
			}
			seen[c] = true
			tree = append(tree, snap[c])
			queue = append(queue, c)
		}
	}
	return tree
}

// SessionMembers returns every process in snap whose kernel session id is
// sid. Complements TreeOf: a pane descendant that was reparented to init
// (its spawner exited first) drops out of the ppid tree but keeps the pane
// root's SID unless it called setsid.
func SessionMembers(snap map[int]Process, sid int) []Process {
	var members []Process
	for _, p := range snap {
		if p.SID == sid {
			members = append(members, p)
		}
	}
	return members
}

// AliveSame reports whether the same process instance (matching PID and
// start time) is still running.
func AliveSame(p Process) bool {
	cur, err := readStat(p.PID)
	if err != nil {
		return false
	}
	return cur.StartTicks == p.StartTicks
}

// ErrIdentityChanged is returned by Signal when the PID no longer names the
// snapshotted process instance (it exited, or the PID was recycled).
var ErrIdentityChanged = errors.New("process exited or pid was recycled")

// kill is the syscall used to deliver signals. It is a package variable only
// so tests can simulate the TOCTOU window (a process reaped between the
// identity check and the signal, making the kernel return ESRCH).
var kill = syscall.Kill

// Signal delivers sig to p only if the PID still names the same process
// instance. The verify-then-kill pair has an unavoidable microsecond TOCTOU
// window; PID recycling within it would require the kernel to cycle through
// the entire PID space between the two syscalls, which does not happen in
// practice. If the process is reaped inside that window, syscall.Kill returns
// ESRCH — indistinguishable from the AliveSame failure path, so it is coerced
// to ErrIdentityChanged and callers treat "already gone" as success.
func Signal(p Process, sig syscall.Signal) error {
	if p.PID <= 1 || p.PID == os.Getpid() {
		return fmt.Errorf("refusing to signal pid %d", p.PID)
	}
	if !AliveSame(p) {
		return ErrIdentityChanged
	}
	if err := kill(p.PID, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrIdentityChanged
		}
		return err
	}
	return nil
}

// WaitForExits polls until every process in procs is gone (or its PID was
// recycled — same thing for our purposes) or the timeout elapses, and
// returns the ones still alive.
func WaitForExits(procs []Process, timeout time.Duration) []Process {
	deadline := time.Now().Add(timeout)
	for {
		var alive []Process
		for _, p := range procs {
			if AliveSame(p) {
				alive = append(alive, p)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			return alive
		}
		procs = alive
		time.Sleep(50 * time.Millisecond)
	}
}

// KillEscalating gives procs the grace period to exit on their own, SIGTERMs
// survivors, waits termWait, SIGKILLs what remains, and returns anything
// still alive after a final bounded wait (should be empty). Every signal is
// identity-verified (see Signal) and reported through logf, one line per
// process. logf may be nil.
func KillEscalating(procs []Process, grace, termWait time.Duration, logf func(format string, args ...any)) []Process {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	survivors := WaitForExits(procs, grace)
	if len(survivors) == 0 {
		return nil
	}
	for _, p := range survivors {
		err := Signal(p, syscall.SIGTERM)
		switch {
		case err == nil:
			logf("reaping leaked process %d (%s) with SIGTERM: %s", p.PID, p.Comm, Cmdline(p.PID))
		case !errors.Is(err, ErrIdentityChanged):
			logf("failed to SIGTERM leaked process %d (%s): %v", p.PID, p.Comm, err)
		}
	}
	survivors = WaitForExits(survivors, termWait)
	for _, p := range survivors {
		err := Signal(p, syscall.SIGKILL)
		switch {
		case err == nil:
			logf("leaked process %d (%s) ignored SIGTERM; sent SIGKILL", p.PID, p.Comm)
		case !errors.Is(err, ErrIdentityChanged):
			logf("failed to SIGKILL leaked process %d (%s): %v", p.PID, p.Comm, err)
		}
	}
	remaining := WaitForExits(survivors, time.Second)
	for _, p := range remaining {
		logf("leaked process %d (%s) survived SIGKILL", p.PID, p.Comm)
	}
	return remaining
}

// CPUFraction returns the process's lifetime-average CPU usage as a fraction
// of one core (1.0 = a full core since it started), plus its age in seconds.
// Uses /proc/uptime; returns (0, 0, error) when that is unreadable. A brand
// new process (age < 1s) reports 0 to avoid a noisy division.
func CPUFraction(p Process) (frac float64, ageSeconds float64, err error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, 0, fmt.Errorf("reading /proc/uptime: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, 0, errors.New("malformed /proc/uptime")
	}
	uptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("malformed /proc/uptime: %w", err)
	}
	age := uptime - float64(p.StartTicks)/clkTck
	if age < 1 {
		return 0, age, nil
	}
	busy := float64(p.UTicks+p.STicks) / clkTck
	return busy / age, age, nil
}

// EnvValue reads key from /proc/<pid>/environ. Returns ("", false) when the
// variable is absent or the environ is unreadable (different UID, kernel
// thread, or the process exited). The environ reflects the process's
// *initial* environment — exactly what we want for ancestry markers, since a
// process cannot retroactively lose the marker it inherited.
func EnvValue(pid int, key string) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return "", false
	}
	prefix := []byte(key + "=")
	for _, kv := range bytes.Split(data, []byte{0}) {
		if bytes.HasPrefix(kv, prefix) {
			return string(kv[len(prefix):]), true
		}
	}
	return "", false
}

// Cmdline returns the process's argv joined with spaces, or "" when
// unreadable. For kernel threads (empty cmdline) it returns "". This is a
// lossy, display-oriented view: it collapses argv boundaries, so a binary path
// containing spaces cannot be recovered from it — use Argv for classification
// that must survive spaced paths (#1214).
func Cmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(bytes.ReplaceAll(data, []byte{0}, []byte{' '})))
}

// Argv returns the process's argv with argument boundaries preserved (each
// element a distinct argv entry, spaces within an argument kept intact), or nil
// when unreadable or for kernel threads (empty cmdline). Unlike Cmdline it does
// not collapse the NUL separators, so a binary path containing spaces stays in
// a single element (#1214).
func Argv(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	parts := bytes.Split(data, []byte{0})
	// Drop the trailing empty element left by the final NUL terminator.
	for len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return nil
	}
	argv := make([]string, len(parts))
	for i, p := range parts {
		argv[i] = string(p)
	}
	return argv
}
