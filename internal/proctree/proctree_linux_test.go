//go:build linux

package proctree

import (
	"errors"
	"strings"
	"testing"
)

// This file is linux-tagged because its subject is: /proc/uptime, and the
// backend variable that points at it. An untagged test referencing a
// build-tagged symbol fails to COMPILE on the other platform — a runtime
// t.Skip cannot rescue it, which `GOOS=darwin go vet` says immediately and
// which is why this file exists rather than a skip.

// TestSnapshotSurvivesUnreadableBootTime is the subset=pid regression: a procfs
// that serves /proc/<pid>/stat but hides /proc/uptime must still yield a
// process table.
//
// It used to fail the whole snapshot, which turned off orphan reaping and
// doctor's process map on a machine where both would have worked perfectly —
// this package's own disease inverted. It was built to stop manufacturing
// health where there is no data; failing here manufactured NO DATA where there
// is data.
//
// Boot time is unreadable only in the sense that matters: the test points the
// backend at a path that does not exist, which is exactly what subset=pid
// presents. StartedAt then stays zero and CPUFraction reports unknown — a
// nice-to-have lost, loudly — while the table itself is intact.
func TestSnapshotSurvivesUnreadableBootTime(t *testing.T) {
	child := startSleeper(t)

	orig := uptimePath
	t.Cleanup(func() { uptimePath = orig })
	uptimePath = "/proc/definitely-not-uptime" // what subset=pid looks like

	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot with an unreadable boot time = %v; want the process table anyway — "+
			"/proc/<pid>/stat still reads, so refusing here reports NO DATA where data exists", err)
	}
	if _, ok := snap[child.Process.Pid]; !ok {
		t.Errorf("snapshot missing child pid %d: the table must survive an unreadable boot time",
			child.Process.Pid)
	}

	// And the part we genuinely lost must say so rather than read as idle.
	p := snap[child.Process.Pid]
	if _, _, err := CPUFraction(p); !errors.Is(err, ErrCPUUnknown) {
		t.Errorf("CPUFraction without a boot time = %v, want ErrCPUUnknown: an unmeasurable process "+
			"must never report as 0%% CPU, which is indistinguishable from idle", err)
	}
}

// TestBootIDSurvivesPIDOnlyProcfs covers the other subset=pid consequence:
// /proc/<pid> remains readable while /proc/sys/kernel/random/boot_id is hidden.
// Persisted process ownership still needs a stable namespace-scoped identity in
// that environment; refusing to start every editor is not a safe degradation.
func TestBootIDSurvivesPIDOnlyProcfs(t *testing.T) {
	orig := bootIDPath
	t.Cleanup(func() { bootIDPath = orig })
	bootIDPath = "/proc/definitely-not-a-boot-id"

	first, err := BootID()
	if err != nil {
		t.Fatalf("BootID with /proc/sys hidden = %v; want a PID-namespace fallback from /proc/<pid>", err)
	}
	second, err := BootID()
	if err != nil {
		t.Fatalf("second BootID with /proc/sys hidden = %v", err)
	}
	if first != second {
		t.Fatalf("PID-namespace boot fallback changed between reads: %q then %q", first, second)
	}
	if !strings.HasPrefix(first, "pidns:") {
		t.Fatalf("BootID fallback = %q, want explicit pidns: identity", first)
	}
}
