//go:build linux

package proctree

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// lookupForkShape is Lookup plus the process's fork shape, both from ONE read
// of /proc/<pid>/stat, so the decision IsTmuxServer makes from them is about a
// single moment of a single process.
func lookupForkShape(pid int) (Process, forkShape, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Process{}, forkShape{}, err
	}
	p, _, err := parseStat(pid, data)
	if err != nil {
		return Process{}, forkShape{}, err
	}
	shape, err := parseForkShape(pid, data)
	if err != nil {
		return Process{}, forkShape{}, err
	}
	return p, shape, nil
}

// parseForkShape reads the fork shape off a /proc/<pid>/stat line: field 7,
// tty_nr, the controlling terminal's device number (0 for none), and field 32,
// blocked, the blocked-signal mask for signals 1 to 31 as a decimal number.
// SIGUSR1 is below 32 on every Linux architecture.
func parseForkShape(pid int, data []byte) (forkShape, error) {
	close_ := bytes.LastIndexByte(data, ')')
	if close_ < 0 || close_+2 > len(data) {
		return forkShape{}, fmt.Errorf("malformed stat for pid %d", pid)
	}
	fields := strings.Fields(string(data[close_+2:]))
	// fields[0] is stat field 3 (state); stat field N lives at fields[N-3].
	const (
		idxTTY     = 7 - 3
		idxBlocked = 32 - 3
	)
	if len(fields) <= idxBlocked {
		return forkShape{}, fmt.Errorf("truncated stat for pid %d", pid)
	}
	tty, err := strconv.ParseInt(fields[idxTTY], 10, 64)
	if err != nil {
		return forkShape{}, fmt.Errorf("bad tty_nr for pid %d: %w", pid, err)
	}
	blocked, err := strconv.ParseUint(fields[idxBlocked], 10, 64)
	if err != nil {
		return forkShape{}, fmt.Errorf("bad blocked mask for pid %d: %w", pid, err)
	}
	return forkShape{
		known:       true,
		ctty:        tty != 0,
		usr1Blocked: blocked&(1<<(uint(syscall.SIGUSR1)-1)) != 0,
	}, nil
}
