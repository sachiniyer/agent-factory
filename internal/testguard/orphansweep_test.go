package testguard

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testresidue"
)

// stampSelf writes a stamp for the current test process through the real
// writer, so the round trip exercises the same fields production writes.
func stampSelf(t *testing.T, dir string) ownerStamp {
	t.Helper()
	if err := writeOwnerStamp(dir); err != nil {
		t.Skipf("proctree cannot identify this process on this platform: %v", err)
	}
	stamp, ok := readOwnerStamp(dir)
	if !ok {
		t.Fatalf("wrote a stamp that readOwnerStamp will not parse")
	}
	return stamp
}

// deadPID returns a pid that provably no longer exists: a child spawned and
// fully waited, so the kernel has released the process table entry.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawning short-lived child: %v", err)
	}
	return cmd.Process.Pid
}

func writeStamp(t *testing.T, dir string, s ownerStamp) {
	t.Helper()
	line := fmt.Sprintf("%d\t%d\t%s\t%s\n", s.pid, s.startID, s.bootID, s.nsID)
	if err := os.WriteFile(filepath.Join(dir, ownerStampFile), []byte(line), 0o644); err != nil {
		t.Fatalf("writing stamp: %v", err)
	}
}

// fakeEnv returns a sweepEnv whose process table and scope are fully
// scripted. lookup responds per-pid from table; any pid absent from it gets
// os.ErrNotExist — the same "definitely gone" the real backend reports.
func fakeEnv(bootID, nsID string, table map[int]proctree.Process, lookupErr map[int]error) *sweepEnv {
	return &sweepEnv{
		lookup: func(pid int) (proctree.Process, error) {
			if err := lookupErr[pid]; err != nil {
				return proctree.Process{}, err
			}
			if p, ok := table[pid]; ok {
				return p, nil
			}
			return proctree.Process{}, os.ErrNotExist
		},
		bootID:     bootID,
		nsID:       nsID,
		killSocket: func(string) error { return nil },
		now:        time.Now,
	}
}

func TestOwnerStamp_RoundTripsSelfIdentity(t *testing.T) {
	dir := t.TempDir()
	stamp := stampSelf(t, dir)
	if stamp.pid != os.Getpid() {
		t.Fatalf("stamp pid %d, want own pid %d", stamp.pid, os.Getpid())
	}
	boot, err := proctree.BootID()
	if err != nil || stamp.bootID != boot {
		t.Fatalf("stamp bootID %q vs current %q (err %v)", stamp.bootID, boot, err)
	}
	ns, err := proctree.PIDNamespaceID()
	if err != nil || stamp.nsID != ns {
		t.Fatalf("stamp nsID %q vs current %q (err %v)", stamp.nsID, ns, err)
	}
	// The stamp must be a real instance identity: the same pid with a
	// different StartID is a DIFFERENT process and must not match.
	self, err := proctree.Lookup(os.Getpid())
	if err != nil {
		t.Skipf("cannot read own process: %v", err)
	}
	if stamp.startID != self.StartID {
		t.Fatalf("stamp StartID %d vs live %d", stamp.startID, self.StartID)
	}
}

func TestClassifyDir_LiveOwnerIsLeft(t *testing.T) {
	dir := t.TempDir()
	stampSelf(t, dir)
	env := defaultSweepEnv()
	if env.bootID == "" || env.nsID == "" {
		t.Skip("process scope unreadable on this platform")
	}
	verdict, stamped, _ := env.classifyDir(dir)
	if !stamped || verdict != ownerAlive {
		t.Fatalf("self-stamped dir: verdict=%v stamped=%v, want ownerAlive", verdict, stamped)
	}
}

func TestClassifyDir_DeadPIDIsReaped(t *testing.T) {
	dir := t.TempDir()
	boot, _ := proctree.BootID()
	ns, _ := proctree.PIDNamespaceID()
	if boot == "" || ns == "" {
		t.Skip("process scope unreadable on this platform")
	}
	writeStamp(t, dir, ownerStamp{pid: deadPID(t), startID: 1, bootID: boot, nsID: ns})
	env := defaultSweepEnv()
	verdict, _, _ := env.classifyDir(dir)
	if verdict != ownerDead {
		t.Fatalf("stamped dead pid: verdict=%v, want ownerDead", verdict)
	}
}

func TestClassifyDir_RecycledPIDIsReaped(t *testing.T) {
	dir := t.TempDir()
	const pid = 424242
	writeStamp(t, dir, ownerStamp{pid: pid, startID: 111, bootID: "boot-a", nsID: "ns-a"})
	// Same pid, different StartID: the pid was recycled — the stamped
	// instance is gone even though a process answers to that number.
	env := fakeEnv("boot-a", "ns-a",
		map[int]proctree.Process{pid: {PID: pid, StartID: 222}}, nil)
	verdict, stamped, _ := env.classifyDir(dir)
	if !stamped || verdict != ownerDead {
		t.Fatalf("recycled pid: verdict=%v, want ownerDead", verdict)
	}
}

func TestClassifyDir_LookupFailureLeavesDir(t *testing.T) {
	dir := t.TempDir()
	const pid = 424243
	writeStamp(t, dir, ownerStamp{pid: pid, startID: 111, bootID: "boot-a", nsID: "ns-a"})
	env := fakeEnv("boot-a", "ns-a", nil,
		map[int]error{pid: errors.New("permission denied")})
	verdict, stamped, _ := env.classifyDir(dir)
	if !stamped || verdict != ownerUnknown {
		t.Fatalf("unreadable owner: verdict=%v, want ownerUnknown — could-not-look must not read as dead", verdict)
	}
}

func TestClassifyDir_MissingAndMalformedStampsAreUnknown(t *testing.T) {
	dir := t.TempDir()
	env := fakeEnv("boot-a", "ns-a", nil, nil)
	if verdict, stamped, _ := env.classifyDir(dir); stamped || verdict != ownerUnknown {
		t.Fatalf("unstamped dir: verdict=%v stamped=%v, want unknown/unstamped", verdict, stamped)
	}
	for _, body := range []string{"", "garbage", "123\t456", "123\tabc\tb\tn", "-5\t1\tb\tn"} {
		if err := os.WriteFile(filepath.Join(dir, ownerStampFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if verdict, stamped, _ := env.classifyDir(dir); stamped || verdict != ownerUnknown {
			t.Fatalf("stamp %q: verdict=%v stamped=%v, want unknown/unstamped", body, verdict, stamped)
		}
	}
}

func TestClassifyDir_ForeignBootIsDeadWithoutLookup(t *testing.T) {
	dir := t.TempDir()
	// Same namespace, different boot: the stamped instance cannot still
	// exist — a reboot ended every process it could name.
	writeStamp(t, dir, ownerStamp{pid: os.Getpid(), startID: 1, bootID: "other-boot", nsID: "ns-a"})
	env := fakeEnv("boot-a", "ns-a", nil, nil)
	verdict, _, reason := env.classifyDir(dir)
	if verdict != ownerDead {
		t.Fatalf("different boot: verdict=%v (%s), want ownerDead — a stamp from another boot cannot name a live process here", verdict, reason)
	}
}

func TestClassifyDir_ForeignNamespaceIsUnproven(t *testing.T) {
	dir := t.TempDir()
	// A different PID namespace means the stamped pid lives in a number
	// space we cannot read — the owner may be alive there (two test
	// containers sharing a host /tmp). Unverifiable is not dead.
	writeStamp(t, dir, ownerStamp{pid: os.Getpid(), startID: 1, bootID: "boot-a", nsID: "ns-other"})
	env := fakeEnv("boot-a", "ns-a", nil, nil)
	verdict, stamped, _ := env.classifyDir(dir)
	if !stamped || verdict != ownerUnknown {
		t.Fatalf("foreign namespace: verdict=%v, want ownerUnknown — an alive-elsewhere owner must never be killed", verdict)
	}
}

func TestClassifyDir_UnreadableCurrentScopeIsUnknown(t *testing.T) {
	dir := t.TempDir()
	// A stamp is only actionable inside the scope it was written in; if the
	// current scope cannot be read, the pid check proves nothing.
	writeStamp(t, dir, ownerStamp{pid: os.Getpid(), startID: 1, bootID: "boot-a", nsID: "ns-a"})
	for _, env := range []*sweepEnv{
		fakeEnv("", "ns-a", nil, nil),
		fakeEnv("boot-a", "", nil, nil),
	} {
		if verdict, _, _ := env.classifyDir(dir); verdict != ownerUnknown {
			t.Fatalf("unreadable scope: verdict=%v, want ownerUnknown", verdict)
		}
	}
}

// unixSocket creates a real socket-mode file without tmux, so the sweep's
// kill-server path is exercised on the file type it actually keys on.
func unixSocket(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("creating unix socket fixture: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return path
}

// sweepBase returns a short temp dir to hold socket fixtures — t.TempDir's
// name-embedded path can push a unix socket past sun_path on darwin.
func sweepBase(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "af-swt-")
	if err != nil {
		t.Fatalf("creating sweep base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestSweep_ReapsDeadOwnerAndItsSockets(t *testing.T) {
	base := sweepBase(t)
	dead := filepath.Join(base, testresidue.TestTmuxPrefix+"123")
	live := filepath.Join(base, testresidue.TestTmuxPrefix+"456")
	unstamped := filepath.Join(base, testresidue.TestTmuxPrefix+"789")
	for _, d := range []string{dead, live, unstamped} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The real layout is dir/tmux-<uid>/default — TMUX_TMPDIR is the base,
	// not the socket's parent. A sweep that only reads the top level sees a
	// directory and never kills the server (#4503 review).
	sockDir := filepath.Join(dead, "tmux-1000")
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := unixSocket(t, sockDir, "default")
	writeStamp(t, dead, ownerStamp{pid: 424244, startID: 1, bootID: "b", nsID: "n"})
	writeStamp(t, live, ownerStamp{pid: 424245, startID: 7, bootID: "b", nsID: "n"})

	var killed []string
	env := fakeEnv("b", "n",
		map[int]proctree.Process{424245: {PID: 424245, StartID: 7}}, nil)
	env.killSocket = func(s string) error { killed = append(killed, s); return nil }

	stats := env.sweep(base)
	if stats.reaped != 1 || stats.live != 1 || stats.unattributed != 1 {
		t.Fatalf("stats %+v, want reaped=1 live=1 unattributed=1", stats)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("dead-owner dir still present")
	}
	if len(killed) != 1 || killed[0] != sock {
		t.Fatalf("kill-server ran on %v, want exactly %s", killed, sock)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live-owner dir was removed: %v", err)
	}
	if _, err := os.Stat(unstamped); err != nil {
		t.Fatalf("unstamped dir was removed: %v", err)
	}
}

func TestSweep_KillFailureStillRemovesDeadOwnerDir(t *testing.T) {
	base := sweepBase(t)
	dir := filepath.Join(base, testresidue.PackageTmuxPrefix+"4246")
	sockDir := filepath.Join(dir, "tmux-1000")
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unixSocket(t, sockDir, "default")
	writeStamp(t, dir, ownerStamp{pid: 424246, startID: 1, bootID: "b", nsID: "n"})
	env := fakeEnv("b", "n", nil, nil)
	env.killSocket = func(string) error { return errors.New("server not responding") }
	stats := env.sweep(base)
	if stats.reaped != 1 || stats.killFailed != 1 {
		t.Fatalf("stats %+v, want reaped=1 killFailed=1 — a wedged server must not keep the dir", stats)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir survived despite dead owner")
	}
}

func TestSweep_UnprovenDirIsLeftAndCounted(t *testing.T) {
	base := sweepBase(t)
	dir := filepath.Join(base, testresidue.TestTmuxPrefix+"4247")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStamp(t, dir, ownerStamp{pid: 424247, startID: 1, bootID: "b", nsID: "n"})
	env := fakeEnv("b", "n", nil, map[int]error{424247: errors.New("procfs unavailable")})
	stats := env.sweep(base)
	if stats.unproven != 1 || stats.reaped != 0 {
		t.Fatalf("stats %+v, want unproven=1 reaped=0", stats)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("unproven dir was removed — could-not-look became permission to delete")
	}
	if !stats.noteworthy() || !strings.Contains(stats.String(), "unproven") {
		t.Fatalf("left dirs must stay visible in the report: %q", stats)
	}
}

func TestSweep_BudgetStopsAndCountsRemainder(t *testing.T) {
	base := sweepBase(t)
	names := []string{testresidue.TestTmuxPrefix + "1", testresidue.PackageTmuxPrefix + "2", testresidue.SandboxHomePrefix + "3"}
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := fakeEnv("b", "n", nil, nil)
	start := time.Now()
	// First call inside the budget, everything after it expired.
	calls := 0
	env.now = func() time.Time {
		calls++
		if calls <= 1 {
			return start
		}
		return start.Add(2 * orphanSweepBudget)
	}
	stats := env.sweep(base)
	if stats.unproven != 3 {
		t.Fatalf("stats %+v, want all 3 counted unproven after budget expiry", stats)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(base, name)); err != nil {
			t.Fatalf("%s removed after budget expiry", name)
		}
	}
}

func TestSweep_IgnoresUnrelatedPrefixesAndFiles(t *testing.T) {
	base := sweepBase(t)
	// af-* dirs (SocketTempDir) and unrelated names are not this package's
	// to judge; a plain FILE wearing a harness name is skipped too.
	for _, name := range []string{"af-other1", "unrelated", testresidue.TestTmuxPrefix + "4248"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := fakeEnv("b", "n", nil, nil)
	stats := env.sweep(base)
	if stats.reaped != 0 || stats.live != 0 || stats.unattributed != 0 || stats.unproven != 0 {
		t.Fatalf("stats %+v on foreign entries, want all zero", stats)
	}
}

// The sweep judges exactly the names testresidue says this package creates.
// A dir that merely starts with one of the prefixes is not the harness's, so
// even a stamp naming a dead owner does not make it this sweep's to delete.
func TestSweep_OnlyHarnessShapedNamesAreJudged(t *testing.T) {
	base := sweepBase(t)
	names := []string{"af-tmux-abc", "af-tmux-pkg-", "af-tmux-pkg-12a", "af-test-home-12x", "af-tmux-"}
	for _, name := range names {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeStamp(t, dir, ownerStamp{pid: 424249, startID: 1, bootID: "b", nsID: "n"})
	}
	env := fakeEnv("b", "n", nil, nil)
	stats := env.sweep(base)
	if stats.reaped != 0 || stats.live != 0 || stats.unattributed != 0 || stats.unproven != 0 {
		t.Fatalf("stats %+v on names the harness does not make, want all zero", stats)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(base, name)); err != nil {
			t.Fatalf("%s was removed although the harness never makes that name", name)
		}
	}
}

// kill-server runs only in a tmux socket dir — the kind that is a TMUX_TMPDIR.
// A SandboxHome with a socket in it is removed without one.
func TestSweep_KillsServersOnlyInTmuxSocketDirs(t *testing.T) {
	base := sweepBase(t)
	socketIn := map[string]string{}
	for i, prefix := range []string{testresidue.TestTmuxPrefix, testresidue.PackageTmuxPrefix, testresidue.SandboxHomePrefix} {
		dir := filepath.Join(base, fmt.Sprintf("%s%d", prefix, 500+i))
		sub := filepath.Join(dir, "tmux-1000")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		socketIn[prefix] = unixSocket(t, sub, "default")
		writeStamp(t, dir, ownerStamp{pid: 424250 + i, startID: 1, bootID: "b", nsID: "n"})
	}
	var killed []string
	env := fakeEnv("b", "n", nil, nil)
	env.killSocket = func(s string) error { killed = append(killed, s); return nil }
	stats := env.sweep(base)
	if stats.reaped != 3 {
		t.Fatalf("stats %+v, want all three dead-owner dirs reaped", stats)
	}
	want := map[string]bool{socketIn[testresidue.TestTmuxPrefix]: true, socketIn[testresidue.PackageTmuxPrefix]: true}
	if len(killed) != len(want) {
		t.Fatalf("kill-server ran on %v, want exactly the two tmux socket dirs' sockets", killed)
	}
	for _, sock := range killed {
		if !want[sock] {
			t.Fatalf("kill-server ran on %s, which is not in a tmux socket dir", sock)
		}
	}
}

func TestSweepOnce_DisableEnvSkipsSweep(t *testing.T) {
	// Both halves of the cache must reset: under -shuffle a prior test's real
	// sweep can leave nonzero orphanSweepStats, and a disabled run must return
	// empty stats regardless of order (#4503 review).
	orphanSweepOnce = sync.Once{}
	orphanSweepStats = sweepStats{}
	t.Cleanup(func() { orphanSweepOnce = sync.Once{}; orphanSweepStats = sweepStats{} })
	t.Setenv("AF_DISABLE_ORPHAN_SWEEP", "1")
	stats, ran := sweepOrphanTempDirsOnce()
	if !ran || stats.noteworthy() {
		t.Fatalf("disabled sweep: ran=%v stats=%+v, want ran with empty stats", ran, stats)
	}
}
