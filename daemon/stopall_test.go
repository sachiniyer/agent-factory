package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These tests cover the scoping that makes `af reset` safe to point at a
// process table: reset SIGTERMs (and escalates to SIGKILL on) daemons it
// discovers by scanning, so a filter that is too loose kills someone else's
// daemon.
//
// The classification tests never signal anything — they spawn a fake and ask
// verifyScopedDaemon what it thinks. The one test that DOES signal injects
// scopedDaemonScanFn so only its own fake's PID is ever a candidate: the real
// scan is host-wide, and a developer running these must never have their own
// daemon stopped (#793).

// spawnFakeDaemonWithHome starts a fake `af --daemon` whose environ carries the
// given AGENT_FACTORY_HOME. Passing env=nil starts it with NO AGENT_FACTORY_HOME
// at all, which is the common real-world shape — almost nobody sets it — and the
// case a naive "read the env var and compare" filter gets wrong.
func spawnFakeDaemonWithHome(t *testing.T, home string) int {
	t.Helper()
	argv0 := filepath.Join(fakeBinDir(t), "af")

	script := "sleep 300; :" // compound so bash does not exec-optimize the argv away
	cmd := fakeDaemonCmd(t, argv0, script, "--daemon")
	env := []string{"PATH=" + os.Getenv("PATH")}
	if home != "" {
		env = append(env, "AGENT_FACTORY_HOME="+home)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	waitForArgv(t, pid, argv0)
	return pid
}

// fakeBinDir returns a temp dir for a fake daemon binary that is NOT under
// /tmp/Test..., which t.TempDir() would give us.
//
// That distinction is the difference between testing the filter and fooling it:
// isTestBinaryArgs deliberately rejects any argv living under /tmp/Test* or
// /tmp/go-build*, so a fake whose argv0 came from t.TempDir() is classified
// foreign because it looks like a Go test binary — never reaching the uid or
// AF-home checks these tests exist to exercise. It makes a "must be rejected"
// assertion pass for entirely the wrong reason.
func fakeBinDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "af-fake-daemon")
	if err != nil {
		t.Fatalf("make fake bin dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// waitForArgv blocks until pid's argv is the one the fake was built to expose,
// i.e. argv[0] == argv0.
//
// Waiting for a merely NON-EMPTY argv is a race that silently inverts these
// tests. spawnFakeDaemonProc runs `bash -c "exec -a <argv0> bash ..."`, so the
// outer bash has a perfectly readable argv — ["bash", "-c", ...] — for a
// moment BEFORE it execs into the crafted one. Classify it in that window and
// argsAreDaemonBinary sees "bash", returns false, and the daemon is written off
// as foreign: an assertion that should fail passes, and one that should pass
// fails, both for a reason that has nothing to do with the code under test.
func waitForArgv(t *testing.T, pid int, argv0 string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if args := daemonArgs(pid); len(args) > 0 && args[0] == argv0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake daemon pid %d never exec'd into argv0 %q (argv=%q)", pid, argv0, daemonArgs(pid))
}

func TestVerifyScopedDaemon_MatchesOnlyOurHome(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	ourHome := t.TempDir()
	otherHome := t.TempDir()
	uid := os.Getuid()

	want, err := canonicalDir(ourHome)
	if err != nil {
		t.Fatal(err)
	}

	ours := spawnFakeDaemonWithHome(t, ourHome)
	if got := verifyScopedDaemon(ours, uid, want); got != daemonOurs {
		t.Errorf("daemon for our home classified %v, want daemonOurs", got)
	}

	// The invariant that matters most: a daemon serving a DIFFERENT AF home is
	// never ours to stop. Running several homes side by side is supported, and a
	// reset that killed all of them would be worse than the stale daemon it came
	// to clean up.
	foreign := spawnFakeDaemonWithHome(t, otherHome)
	if got := verifyScopedDaemon(foreign, uid, want); got != daemonForeign {
		t.Errorf("daemon for a different AF home classified %v, want daemonForeign", got)
	}
}

// TestVerifyScopedDaemon_UnsetHomeMeansDefaultHome pins the trap in scoping by
// environ: a daemon started WITHOUT AGENT_FACTORY_HOME is not "unknown", it
// resolved the DEFAULT home. That is the overwhelmingly common case, so reading
// the variable and skipping when it is absent would skip exactly the everyday
// stale daemon reset exists to kill — while still, correctly, not matching when
// we ourselves are pointed at some other home.
func TestVerifyScopedDaemon_UnsetHomeMeansDefaultHome(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	uid := os.Getuid()
	bare := spawnFakeDaemonWithHome(t, "") // no AGENT_FACTORY_HOME in its environ

	// We are pointed at a temp home; the bare daemon serves the default home.
	tempHome, err := canonicalDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := verifyScopedDaemon(bare, uid, tempHome); got != daemonForeign {
		t.Errorf("bare daemon vs a temp home classified %v, want daemonForeign", got)
	}

	// Now resolve the default home the same way the daemon did, and it matches.
	defHome, err := config.ConfigDirFor("")
	if err != nil {
		t.Skipf("cannot resolve the default af home here: %v", err)
	}
	wantDefault, err := canonicalDir(defHome)
	if err != nil {
		t.Fatal(err)
	}
	if got := verifyScopedDaemon(bare, uid, wantDefault); got != daemonOurs {
		t.Errorf("bare daemon vs the default home classified %v, want daemonOurs "+
			"(a daemon with no AGENT_FACTORY_HOME resolved the default home)", got)
	}
}

// TestVerifyScopedDaemon_NonDaemonNeverMatches guards the other direction: the
// scan matches on a `--daemon` token, so anything that is not an af binary — or
// is a Go test binary — must be rejected before it is ever signalled.
func TestVerifyScopedDaemon_NonDaemonNeverMatches(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("needs /proc")
	}
	uid := os.Getuid()
	home := testguard.SocketTempDir(t)
	want, err := canonicalDir(home)
	if err != nil {
		t.Fatal(err)
	}

	// An unrelated program carrying a --daemon token. Its path must not look
	// like a Go test binary, or it would be rejected for that reason instead of
	// the one under test (see fakeBinDir).
	argv0 := filepath.Join(fakeBinDir(t), "some-other-tool")
	cmd := fakeDaemonCmd(t, argv0, "sleep 300; :", "--daemon")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "AGENT_FACTORY_HOME=" + home}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	waitForArgv(t, pid, argv0)

	if got := verifyScopedDaemon(pid, uid, want); got != daemonForeign {
		t.Errorf("non-af binary carrying --daemon classified %v, want daemonForeign", got)
	}
}

// TestStopOrphanDaemons_StopsOnlyScopedDaemon is the only test here that
// signals. scopedDaemonScanFn is injected so the candidate list is EXACTLY our
// two fakes — the real scan is host-wide and must never be run from a test.
func TestStopOrphanDaemons_StopsOnlyScopedDaemon(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	ourHome := t.TempDir()
	otherHome := t.TempDir()

	ours := spawnFakeDaemonWithHome(t, ourHome)
	foreign := spawnFakeDaemonWithHome(t, otherHome)

	origScan := scopedDaemonScanFn
	t.Cleanup(func() { scopedDaemonScanFn = origScan })
	scopedDaemonScanFn = func() ([]int, error) { return []int{ours, foreign}, nil }

	stopped, unverified, err := StopOrphanDaemons(ourHome)
	if err != nil {
		t.Fatalf("StopOrphanDaemons: %v", err)
	}
	if len(unverified) != 0 {
		t.Errorf("unverified = %v, want none", unverified)
	}
	if len(stopped) != 1 || stopped[0] != ours {
		t.Fatalf("stopped = %v, want exactly [%d] (our home's daemon)", stopped, ours)
	}
	if pidLooksAlive(ours) {
		t.Errorf("our daemon pid %d is still alive after StopOrphanDaemons", ours)
	}
	if !pidLooksAlive(foreign) {
		t.Fatalf("StopOrphanDaemons killed pid %d, a daemon for a DIFFERENT AF home", foreign)
	}
}
