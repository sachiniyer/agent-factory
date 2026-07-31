//go:build linux

package proctree

import (
	"strings"
	"testing"
	"time"
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
// Boot time is unreadable from procfs only: the test points the backend at a
// path that does not exist, exactly what subset=pid presents. The kernel's
// CLOCK_BOOTTIME must still supply StartedAt, because doctor uses process age
// to distinguish durable leaks from teardown churn.
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

	p := snap[child.Process.Pid]
	if p.StartedAt.IsZero() {
		t.Fatal("StartedAt is zero when /proc/uptime is hidden: subset=pid must fall back to CLOCK_BOOTTIME")
	}
	if age := time.Since(p.StartedAt); age < 0 || age > time.Minute {
		t.Errorf("child age with CLOCK_BOOTTIME fallback = %s, want a recent process", age)
	}
	if _, _, err := CPUFraction(p); err != nil {
		t.Errorf("CPUFraction with CLOCK_BOOTTIME fallback = %v, want a measurable process", err)
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
