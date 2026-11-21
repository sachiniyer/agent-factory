//go:build darwin

package proctree

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDarwinSocketOwnerCensus is the darwin side of #7695's measurement: the
// Linux socket-table sweep has no direct equivalent here, so the #7689
// candidacy fallback stays in place this PR. This test measures the two
// candidate mechanisms a follow-up could choose between, on the real
// process set of whatever mac runs it, and LOGS the census — it never fails
// on the answer, because the point is the number, not an assertion baked
// before a real login session was counted:
//
//   - The per-process path the fallback already takes: PROC_PIDFDSOCKETINFO
//     reads (UnixListenerPaths) over every same-uid pid. Its cost and its
//     refusal count are what a full-census fallback would pay and where it
//     would lose.
//   - The system table a Linux-style sweep would want:
//     net.local.stream.pcblist, XNU's unix-domain pcb list. Whether it
//     answers at all, and how big the answer is, decides if a follow-up is
//     even possible.
func TestDarwinSocketOwnerCensus(t *testing.T) {
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("the process table must be enumerable for a census: %v", err)
	}
	uid := os.Getuid()
	var pids []int
	for pid := range snap {
		if owner, ok := UID(pid); ok && owner == uid {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)

	type outcome int
	const (
		read outcome = iota
		exited
		refused
		failed
	)
	var counts [4]int
	var refusedNames []string
	var worst time.Duration
	var total time.Duration
	for _, pid := range pids {
		start := time.Now()
		_, uerr := UnixListenerPaths(pid)
		d := time.Since(start)
		total += d
		if d > worst {
			worst = d
		}
		switch {
		case uerr == nil:
			counts[read]++
		case uerr == ErrProcessExited:
			counts[exited]++
		case os.IsPermission(uerr):
			counts[refused]++
			if p, lerr := Lookup(pid); lerr == nil {
				refusedNames = append(refusedNames, fmt.Sprintf("%d:%s", pid, p.Comm))
			} else {
				refusedNames = append(refusedNames, fmt.Sprintf("%d", pid))
			}
		default:
			counts[failed]++
		}
	}
	t.Logf("same-uid pids=%d per-process socket reads: ok=%d exited=%d refused=%d failed=%d",
		len(pids), counts[read], counts[exited], counts[refused], counts[failed])
	t.Logf("per-process read cost: total=%v mean=%v worst=%v", total,
		total/time.Duration(max(len(pids), 1)), worst)
	if len(refusedNames) > 0 {
		t.Logf("refused pids: %s", strings.Join(refusedNames, ", "))
	}

	data, serr := unix.SysctlRaw("net.local.stream.pcblist")
	t.Logf("net.local.stream.pcblist: bytes=%d err=%v", len(data), serr)
}
