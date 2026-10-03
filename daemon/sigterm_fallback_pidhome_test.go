package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestLocateDaemonPID_CountsRejectedPIDFileCandidateAfterScan pins the case-1
// half of the rejected-PID-file-candidate count: when the PID file names a
// foreign/unverifiable daemon that the host scan OMITS (a /tmp/go-build* binary
// the PID-file classifier accepts but pgrepDaemonCandidates filters out, or any
// foreign daemon the scan did not surface), locateDaemonPID selects this home's
// one scanned PID (case 1) but must still surface the rejected PID-file
// candidate toward `scanned` — otherwise a signaling failure on that PID sees
// `scanned == 1` and recommends the blanket `pkill -f -- '--daemon'`, which
// would kill exactly the foreign daemon the PID-file binding refused to touch.
// The zero-match and no-scan branches already fold rejectedPIDFilePID in; this
// pins the successful-scan branch to do the same.
func TestLocateDaemonPID_CountsRejectedPIDFileCandidateAfterScan(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// This home's own daemon: the one proven-ours candidate the scan surfaces.
	ours := spawnFakeDaemonWithHome(t, home)
	// A foreign daemon the PID file names. The home binding rejects it
	// (rejectedPIDFilePID), and the host scan is stubbed to OMIT it — the shape
	// a /tmp/go-build* binary takes, which the PID-file classifier accepts but
	// pgrepDaemonCandidates filters out.
	foreign := spawnFakeDaemonWithHome(t, testguard.SocketTempDir(t))

	if err := os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(foreign)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	// The scan surfaces only this home's own daemon; the foreign PID-file
	// candidate is absent from the scan results.
	stubDaemonScan(t, []int{ours}, nil)

	pid, _, scanned, err := locateDaemonPID()
	if err != nil {
		t.Fatalf("locateDaemonPID: %v", err)
	}
	if pid != ours {
		t.Fatalf("locateDaemonPID returned pid=%d, want this home's own pid=%d; the rejected foreign "+
			"PID-file candidate must not be signalled", pid, ours)
	}
	// scanned must count the rejected PID-file candidate the scan omitted, so a
	// signaling failure on the one proven-ours PID does not fall back to the
	// blanket pkill (sigtermFallback sees scanned > 1 and carries the scoped
	// recovery). Pre-fix this returned len(pids) == 1.
	if scanned != 2 {
		t.Errorf("locateDaemonPID returned scanned=%d, want 2 — the rejected foreign PID-file "+
			"candidate (pid=%d) absent from the scan must be counted toward scanned so a signaling "+
			"failure does not recommend the blanket pkill against it", scanned, foreign)
	}

	// The foreign daemon must not have been signalled by locateDaemonPID
	// (classifyDaemonHome only classifies; it does not signal). It is still alive.
	if !pidLooksAlive(foreign) {
		t.Fatalf("foreign daemon pid=%d is no longer alive; locateDaemonPID must not signal a "+
			"PID the home binding rejected", foreign)
	}
}

// TestClassifyDaemonHome_ProcSelfHomeIsUnverifiable pins the /proc/self
// process-frame hazard: a same-UID daemon launched from a different directory
// with AGENT_FACTORY_HOME=/proc/self/cwd/state serves <its cwd>/state, but
// resolveHomeInDaemonFrame returns the absolute spelling unchanged and
// canonicalDir resolves /proc/self/cwd against the CALLER's cwd, not the
// daemon's. Without the guard a same-frame daemon (same root and mount
// namespace, so sameProcessRoot does not catch it) whose home spelling resolves
// to the caller's home compares equal and is classified daemonOurs — signalled
// on a stale PID file or lone pgrep result (#4793 via a /proc/self magic link).
// classifyDaemonHome fails closed (daemonUnverifiable) on a process-relative
// procfs home rather than guessing ours. A daemon whose home resolves the same
// way in the caller's frame (a plain absolute home, exercised by the same-frame
// assertions in the mount-namespace tests in sigterm_fallback_test.go) is not
// affected.
func TestClassifyDaemonHome_ProcSelfHomeIsUnverifiable(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	// The caller's AGENT_FACTORY_HOME resolves to <caller-cwd>/state; without the
	// guard the daemon's /proc/self/cwd/state resolves to the same path in the
	// caller's frame, so the foreign daemon (launched from a different cwd) is
	// misclassified daemonOurs.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(cwd, "state"))
	// The daemon runs from a different cwd with a /proc/self/cwd home, so its
	// resolved home (<its cwd>/state) is not the caller's.
	daemonCwd := t.TempDir()
	argv0 := filepath.Join(fakeBinDir(t), "af")
	cmd := fakeDaemonCmd(t, argv0, "sleep 300; :", "--daemon")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"AGENT_FACTORY_HOME=/proc/self/cwd/state",
	}
	cmd.Dir = daemonCwd
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	waitForArgv(t, pid, argv0)
	if scope := classifyDaemonHome(pid); scope != daemonUnverifiable {
		t.Errorf("AGENT_FACTORY_HOME=/proc/self/cwd/state classified %v; want daemonUnverifiable — "+
			"a /proc/self home resolves in the caller's frame, not the daemon's, so the classifier "+
			"must not guess ours and signal a cross-cwd daemon", scope)
	}
}

// TestClassifyDaemonHome_NonCanonicalProcSelfHomeIsUnverifiable pins the bypass
// of the /proc/self guard by a non-canonical spelling: a same-UID daemon
// launched from a different directory with AGENT_FACTORY_HOME=/proc//self/cwd/state
// (a doubled slash) serves <its cwd>/state, but the raw "/proc/self/" prefix
// check returned false while canonicalDir cleaned the doubled slash and
// resolved /proc/self in the caller's frame, so the foreign daemon compared
// equal to the caller's home and was classified daemonOurs. isProcessRelativeProcfsHome
// now cleans the home before the prefix test, so the non-canonical form is
// caught the same way the canonical one is. The /dev/fd alias (a symlink to
// /proc/self/fd on Linux) is the same process-relative hazard and is rejected
// for the same reason.
func TestClassifyDaemonHome_NonCanonicalProcSelfHomeIsUnverifiable(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(cwd, "state"))
	for _, spelling := range []string{
		"/proc//self/cwd/state",        // doubled slash
		"/proc/self/../self/cwd/state", // .. that lexical-cleans to /proc/self/cwd/state
		"/dev/fd/3/state",              // /dev/fd alias (symlink to /proc/self/fd on Linux)
	} {
		spelling := spelling
		t.Run(spelling, func(t *testing.T) {
			daemonCwd := t.TempDir()
			argv0 := filepath.Join(fakeBinDir(t), "af")
			cmd := fakeDaemonCmd(t, argv0, "sleep 300; :", "--daemon")
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"AGENT_FACTORY_HOME=" + spelling,
			}
			cmd.Dir = daemonCwd
			if err := cmd.Start(); err != nil {
				t.Fatalf("start fake daemon: %v", err)
			}
			pid := cmd.Process.Pid
			t.Cleanup(func() {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_, _ = cmd.Process.Wait()
			})
			waitForArgv(t, pid, argv0)
			if scope := classifyDaemonHome(pid); scope != daemonUnverifiable {
				t.Errorf("AGENT_FACTORY_HOME=%s classified %v; want daemonUnverifiable — "+
					"a non-canonical procfs home cleans to a /proc/self/... path that "+
					"resolves in the caller's frame, not the daemon's, so the classifier "+
					"must not guess ours and signal a cross-cwd daemon", spelling, scope)
			}
		})
	}
}

// TestClassifyDaemonHome_ProcfsIndirectedSymlinkHomeIsUnverifiable pins the
// procfs-indirection hazard isProcessRelativeProcfsHome's spelling guard does
// not catch: AGENT_FACTORY_HOME is a normal-looking symlink whose TARGET is a
// process-relative procfs path (link -> /proc/self/cwd/state). The daemon
// resolves link in its own frame (<its cwd>/state), but canonicalDir follows
// the link in the CALLER's frame (/proc/self/cwd is the reading process's cwd),
// so a same-UID, same-namespace daemon launched from a different cwd compares
// equal to wantHome and is misclassified daemonOurs on a stale PID file or lone
// pgrep result. sameProcessRoot does not catch it (root and mount-namespace
// match). homeSymlinkEntersProcessRelativeProcfs walks the link target and
// rejects a home whose resolved chain enters a process-relative procfs path.
func TestClassifyDaemonHome_ProcfsIndirectedSymlinkHomeIsUnverifiable(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// The caller's home resolves to <caller-cwd>/state; without the fix the
	// daemon's AGENT_FACTORY_HOME=link resolves the same way in the caller's
	// frame and the foreign daemon is misclassified daemonOurs.
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(cwd, "state"))
	daemonCwd := t.TempDir()
	// link is a relative symlink in the daemon's cwd whose target is a
	// process-relative procfs path: the daemon resolves it against its own
	// cwd (<its cwd>/state), but canonicalDir follows /proc/self in the
	// caller's frame.
	if err := os.Symlink("/proc/self/cwd/state", filepath.Join(daemonCwd, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	argv0 := filepath.Join(fakeBinDir(t), "af")
	cmd := fakeDaemonCmd(t, argv0, "sleep 300; :", "--daemon")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"AGENT_FACTORY_HOME=link",
	}
	cmd.Dir = daemonCwd
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	waitForArgv(t, pid, argv0)
	if scope := classifyDaemonHome(pid); scope != daemonUnverifiable {
		t.Errorf("AGENT_FACTORY_HOME=link (symlink to /proc/self/cwd/state) classified %v; "+
			"want daemonUnverifiable — a symlink whose target is a process-relative procfs "+
			"path resolves in the caller's frame under canonicalDir, not the daemon's, so the "+
			"classifier must not guess ours and signal a cross-cwd daemon", scope)
	}
}

// TestIsProcessRelativeProcfsHome_ProcNumericPidMagicLinks pins the /proc/<pid>/...
// extension of isProcessRelativeProcfsHome. The /proc/self guard caught the
// caller-relative spellings, but /proc/<pid>/{cwd,root,exe,fd,fdinfo,ns,map_files}
// resolve against <pid>, not the caller: canonicalDir follows /proc/<pid>/cwd in
// the READING process's frame, so a foreign same-UID, same-namespace daemon
// launched with AGENT_FACTORY_HOME=/proc/<other-pid>/cwd/state (where <other-pid>
// later chdir's) is read in the caller's frame and can match wantHome where the
// daemon that owns the socket did not (#4793). Each of these magic links is the
// same cross-frame hazard as /proc/self/... and must be unverifiable. A bare
// /proc/<pid> or a non-magic entry (/proc/<pid>/cmdline, /proc/<pid>/stat) is not
// a process-relative path and must not be rejected — those are regular files.
func TestIsProcessRelativeProcfsHome_ProcNumericPidMagicLinks(t *testing.T) {
	for _, home := range []string{
		"/proc/1/cwd",
		"/proc/1/cwd/state",
		"/proc/12345/root/state",
		"/proc/12345/root",
		"/proc/9999/exe",
		"/proc/9999/fd/3",
		"/proc/9999/fdinfo/5",
		"/proc/9999/ns/mnt",
		"/proc/9999/map_files/foo",
		// The per-task form /proc/<pid>/task/<tid>/{cwd,root,exe,fd,fdinfo,ns,map_files}
		// is the same per-thread magic link the kernel resolves against <pid>/<tid>,
		// not the caller, so a home spelled through it is the same cross-frame hazard.
		"/proc/1/task/2/cwd",
		"/proc/1/task/2/cwd/state",
		"/proc/12345/task/67/root/state",
		"/proc/12345/task/67/root",
		"/proc/9999/task/3/exe",
		"/proc/9999/task/3/fd/7",
		"/proc/9999/task/3/fdinfo/5",
		"/proc/9999/task/3/ns/mnt",
		"/proc/9999/task/3/map_files/foo",
		// Non-canonical spellings clean to the same magic link.
		"/proc//1/cwd/state",
		"/proc/1/../1/cwd/state",
		// /proc/<pid>/{thread-self, fdinfo}... and the existing /dev/fd alias are
		// covered by the /proc/thread-self and /dev/fd branches.
	} {
		if !isProcessRelativeProcfsHome(home) {
			t.Errorf("isProcessRelativeProcfsHome(%q) = false; want true — a /proc/<pid>/magic-link "+
				"home resolves against <pid>, not the caller, the same cross-frame hazard as /proc/self", home)
		}
	}
	// A bare /proc/<pid> or a non-magic entry is not a process-relative path: the
	// former names a per-process directory, the latter a regular file (/proc/<pid>/stat,
	// /proc/<pid>/cmdline). Rejecting them would block legitimate absolute homes that
	// happen to live under /proc/<pid> by coincidence.
	for _, home := range []string{
		"/proc/1",
		"/proc/1/stat",
		"/proc/1/cmdline",
		"/proc/1/environ",
		"/proc/1/io",
		"/proc/1/status",
		// The per-task directory /proc/<pid>/task and /proc/<pid>/task/<tid>
		// themselves are not magic links — only the entries under <tid> are.
		"/proc/1/task",
		"/proc/1/task/2",
		"/proc/1/task/2/stat",
		"/proc/1/task/2/cmdline",
		"/home/user/.agent-factory",
		"/tmp/agent-factory/state",
	} {
		if isProcessRelativeProcfsHome(home) {
			t.Errorf("isProcessRelativeProcfsHome(%q) = true; want false — a bare /proc/<pid>, "+
				"a per-task directory, or a non-magic entry is not a process-relative path", home)
		}
	}
}

// TestHomeSymlinkEntersProcessRelativeProcfs_AncestorAliasToProc pins the
// ancestor-alias hazard the leaf-only symlink walk missed: AGENT_FACTORY_HOME
// reaches procfs through an ancestor symlink (here /tmp/<pid-alias> -> /proc
// with /tmp/<pid-alias>/self/cwd/state). The daemon resolves the home in its
// own frame (<its cwd>/state via /proc/self/cwd), but the symlink chain walk
// followed the ancestor /tmp/<pid-alias> to /proc and dropped the unresolved
// self/cwd/state suffix, so isProcessRelativeProcfsHome(/proc) returned false
// and classifyDaemonHome canonicalized the path in the CALLER's frame — marking
// a same-UID, same-namespace daemon launched from another cwd daemonOurs
// (#4793 via an ancestor procfs alias). Carrying the suffix re-joins it to
// /proc/self/cwd/state, so the home is treated as unverifiable.
func TestHomeSymlinkEntersProcessRelativeProcfs_AncestorAliasToProc(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	root := t.TempDir()
	// An ancestor alias to /proc: /tmp/<alias>/self/cwd/state resolves through
	// /tmp/<alias> -> /proc to /proc/self/cwd/state, a process-relative path the
	// leaf-only Readlink dropped the suffix for.
	alias := filepath.Join(root, "alias")
	if err := os.Symlink("/proc", alias); err != nil {
		t.Fatalf("symlink /proc -> %s: %v", alias, err)
	}
	home := filepath.Join(alias, "self", "cwd", "state")
	if !homeSymlinkEntersProcessRelativeProcfs(home) {
		t.Errorf("homeSymlinkEntersProcessRelativeProcfs(%q) = false; want true — "+
			"an ancestor alias to /proc rejoins to /proc/self/cwd/state, a "+
			"process-relative path the guard must catch", home)
	}
}

// TestClassifyDaemonHome_ProcAncestorAliasHomeIsUnverifiable pins the same
// ancestor-alias hazard end-to-end: a same-UID, same-namespace daemon launched
// from a different cwd with AGENT_FACTORY_HOME=/tmp/<alias>/self/cwd/state (where
// /tmp/<alias> -> /proc) resolves its home through /proc/self/cwd in its own
// frame, but the previous leaf-only symlink walk read the alias target as /proc
// and dropped the self/cwd/state suffix, so the home was canonicalized in the
// CALLER's frame and the foreign daemon was misclassified daemonOurs. The
// suffix-carrying walk rejoins /proc/self/cwd/state and classifies it
// daemonUnverifiable instead.
func TestClassifyDaemonHome_ProcAncestorAliasHomeIsUnverifiable(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(cwd, "state"))
	root := t.TempDir()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink("/proc", alias); err != nil {
		t.Fatalf("symlink /proc -> %s: %v", alias, err)
	}
	daemonCwd := t.TempDir()
	argv0 := filepath.Join(fakeBinDir(t), "af")
	cmd := fakeDaemonCmd(t, argv0, "sleep 300; :", "--daemon")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"AGENT_FACTORY_HOME=" + filepath.Join(alias, "self", "cwd", "state"),
	}
	cmd.Dir = daemonCwd
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	waitForArgv(t, pid, argv0)
	if scope := classifyDaemonHome(pid); scope != daemonUnverifiable {
		t.Errorf("AGENT_FACTORY_HOME=%s/self/cwd/state (alias to /proc) classified %v; "+
			"want daemonUnverifiable — an ancestor alias to /proc rejoins to "+
			"/proc/self/cwd/state, which resolves in the caller's frame, not the "+
			"daemon's, so the classifier must not guess ours and signal a cross-cwd daemon",
			alias, scope)
	}
}

// TestClassifyDaemonHome_ProcNumericPidCwdHomeIsUnverifiable pins the
// /proc/<pid>/cwd hazard that the /proc/self guard did not cover. The caller's
// AGENT_FACTORY_HOME resolves to <caller-cwd>/state; a same-UID, same-namespace
// daemon launched from a different cwd with AGENT_FACTORY_HOME=/proc/<pid>/cwd/state
// serves the directory <pid> points at, but canonicalDir resolves /proc/<pid>/cwd
// in the CALLER's frame (the reading process's frame), so without the guard a
// foreign daemon whose home resolves to the caller's <caller-cwd>/state compares
// equal and is classified daemonOurs on a stale PID file or lone pgrep result.
// isProcessRelativeProcfsHome now treats /proc/<pid>/{cwd,...} as unverifiable
// too, so classifyDaemonHome fails closed (#4793 via a PID-addressed procfs path).
func TestClassifyDaemonHome_ProcNumericPidCwdHomeIsUnverifiable(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// The caller's home resolves to <caller-cwd>/state; without the guard the
	// daemon's /proc/<pid>/cwd/state (with <pid> the caller's own PID) resolves
	// to the same path in the caller's frame, so the foreign daemon would compare
	// equal to the caller's home.
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(cwd, "state"))
	pid := os.Getpid()
	daemonCwd := t.TempDir()
	argv0 := filepath.Join(fakeBinDir(t), "af")
	cmd := fakeDaemonCmd(t, argv0, "sleep 300; :", "--daemon")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"AGENT_FACTORY_HOME=" + fmt.Sprintf("/proc/%d/cwd/state", pid),
	}
	cmd.Dir = daemonCwd
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	daemonPid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-daemonPid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	waitForArgv(t, daemonPid, argv0)
	if scope := classifyDaemonHome(daemonPid); scope != daemonUnverifiable {
		t.Errorf("AGENT_FACTORY_HOME=/proc/%d/cwd/state classified %v; want daemonUnverifiable — "+
			"a /proc/<pid>/cwd home resolves against <pid>, not the caller, so the classifier must "+
			"not guess ours and signal a cross-cwd daemon", pid, scope)
	}
}

// TestHomeSymlinkEntersProcessRelativeProcfs_DotDotCollapsesProcfsSymlink
// pins the `..`-collapse bypass the cleaned chain walk missed: a home spelled
// A/link/.. where A/link -> /proc/self/cwd/state resolves in the DAEMON's frame
// to its own cwd (kernel follows the link to /proc/self/cwd, then applies
// `..`), but filepath.Clean collapses `link/..` to A before the chain walk, so
// the procfs target is never inspected and the guard returned false. Walking
// the uncleaned path follows `link` with the `..` rejoined onto its target
// (/proc/self/cwd/state/.. -> /proc/self/cwd), so the procfs indirection is
// caught (#4793 via a `..`-collapsed procfs symlink).
func TestHomeSymlinkEntersProcessRelativeProcfs_DotDotCollapsesProcfsSymlink(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	A := t.TempDir()
	if err := os.Symlink("/proc/self/cwd/state", filepath.Join(A, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Build the home WITHOUT filepath.Join, which would clean `link/..` to A
	// and lose the very procfs-symlink collapse this test exercises.
	home := filepath.Join(A, "link") + "/.."
	if !homeSymlinkEntersProcessRelativeProcfs(home) {
		t.Errorf("homeSymlinkEntersProcessRelativeProcfs(%q) = false; want true — `..` collapses a "+
			"procfs symlink the cleaned chain walk dropped, so the guard must follow `link` with the "+
			"`..` rejoined onto its /proc/self target", home)
	}
}

// TestClassifyDaemonHome_DotDotCollapsedProcfsSymlinkHomeIsUnverifiable pins the
// same `..`-collapse hazard end-to-end: a same-UID, same-namespace daemon
// launched from a different cwd B with AGENT_FACTORY_HOME=A/link/.. (where
// A/link -> /proc/self/cwd/state) resolves its home in the DAEMON's frame to B
// (via /proc/self/cwd/..), but the cleaned spelling the caller canonicalizes
// collapses `link/..` to A. With the caller's home set to A, the foreign
// daemon was misclassified daemonOurs on a stale PID file or lone pgrep result
// (#4793 via a `..`-collapsed procfs symlink). The uncleaned chain walk
// catches the procfs indirection and classifies it daemonUnverifiable.
func TestClassifyDaemonHome_DotDotCollapsedProcfsSymlinkHomeIsUnverifiable(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	// The caller's home is A; the daemon spells its home as A/link/.. where
	// A/link -> /proc/self/cwd/state. Pre-fix the cleaned spelling collapsed
	// `link/..` to A and the foreign daemon (launched from a different cwd)
	// compared equal to the caller's home.
	A := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", A)
	if err := os.Symlink("/proc/self/cwd/state", filepath.Join(A, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	B := t.TempDir()
	// Build the home WITHOUT filepath.Join, which would clean `link/..` to A
	// and lose the very procfs-symlink collapse this test exercises.
	daemonPid := spawnFakeDaemonIn(t, filepath.Join(A, "link")+"/..", fakeBinDir(t), B)
	if scope := classifyDaemonHome(daemonPid); scope != daemonUnverifiable {
		t.Errorf("AGENT_FACTORY_HOME=%s/link/.. (link -> /proc/self/cwd/state) classified %v; want "+
			"daemonUnverifiable — `..` collapses a procfs symlink the cleaned chain walk dropped, "+
			"so the classifier must not guess ours and signal a cross-cwd daemon", A, scope)
	}
}
