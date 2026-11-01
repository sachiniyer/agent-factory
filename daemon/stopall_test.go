package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
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
	testguard.StartGroupProcess(t, cmd)
	pid := cmd.Process.Pid
	waitForFakeDaemon(t, cmd, argv0)
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

// TestAssertNoLiveDaemon_RefusesOrphanThroughSymlinkedParent exercises the
// destructive reset barrier, not just path normalization in isolation. A
// daemon keeps the spelling of AGENT_FACTORY_HOME from its startup environment;
// after that home is deleted, a plain EvalSymlinks fallback cannot tell that a
// symlinked spelling and the real spelling still name the same missing path.
// Calling that daemon foreign lets reset proceed while its single writer is
// alive over state reset is about to remove.
func TestAssertNoLiveDaemon_RefusesOrphanThroughSymlinkedParent(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}

	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(base, "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}

	realHome := filepath.Join(realRoot, "af-home")
	if err := os.Mkdir(realHome, 0o755); err != nil {
		t.Fatal(err)
	}
	linkHome := filepath.Join(linkRoot, "af-home")
	pid := spawnFakeDaemonWithHome(t, linkHome)

	// Reproduce the orphan state: the process remains live, but the home it
	// started against is gone before reset performs its final liveness proof.
	if err := os.Remove(realHome); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.EvalSymlinks(linkHome); err == nil {
		t.Fatal("sanity: EvalSymlinks unexpectedly resolved the deleted AF home")
	}

	origScan := scopedDaemonScanFn
	t.Cleanup(func() { scopedDaemonScanFn = origScan })
	scopedDaemonScanFn = func() ([]int, error) { return []int{pid}, nil }
	// AssertNoLiveDaemon probes the control socket before the injected scan.
	// Keep that probe inside this throwaway home so an ambient developer daemon
	// cannot satisfy the assertion for an unrelated reason.
	t.Setenv("AGENT_FACTORY_HOME", realHome)

	err := AssertNoLiveDaemon(realHome)
	if err == nil {
		t.Fatalf("AssertNoLiveDaemon allowed reset while orphaned daemon pid %d was still live", pid)
	}
	want := "af daemon(s) still running for this AF home: " + strconv.Itoa(pid)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("AssertNoLiveDaemon refusal = %q, want proven-home refusal containing %q", err, want)
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
	testguard.StartGroupProcess(t, cmd)
	pid := cmd.Process.Pid
	waitForFakeDaemon(t, cmd, argv0)

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

// TestStopOrphanDaemons_RecycledPIDNotSignaled pins the fix for the
// orphan-daemon SIGKILL-on-recycled-PID hazard: a PID verifyScopedDaemon proved
// serves this home exits near the end of the SIGTERM grace and its number is
// recycled onto an unrelated same-user process before the escalation fires. The
// pre-fix path (signalAndWait) held a bare os.Process for the numeric PID and
// polled it with pidLooksAlive (a PID-number liveness check with no instance
// role), so the recycled PID's new owner kept the grace poll reporting "alive"
// and the eventual SIGKILL landed on the unrelated process; reset then reported
// that PID as successfully "stopped". The fix captures the daemon's
// process-instance identity at the verification decision and signals through
// signalClassifiedDaemon -> signalProcessChecked, whose proctree.Signal
// revalidates the StartID immediately before SIGTERM and SIGKILL and refuses
// (ErrIdentityChanged) rather than signal a PID the kernel recycled.
//
// A real PID recycle cannot be arranged on demand (#5007), so the test models
// one the same way TestSignalClassifiedDaemon_UsesCapturedIdentity and
// TestSignalProcessChecked_RecycledPIDNotSignaled do: capture the live daemon's
// real identity and corrupt its StartID, so the captured snapshot no longer
// matches the live process — exactly the instance-check failure a recycle
// produces. StopOrphanDaemons must surface errSignalTargetChanged, leave the
// PID out of the stopped return, and NOT signal the live process.
func TestStopOrphanDaemons_RecycledPIDNotSignaled(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("proctree identity needs /proc")
	}
	ourHome := t.TempDir()
	ours := spawnFakeDaemonWithHome(t, ourHome)

	// Capture the live identity, then corrupt its StartID to model a PID the
	// kernel recycled between verifyScopedDaemon and the signal: the PID is
	// still alive (still this home's daemon) but the captured identity no
	// longer matches the live process — exactly the instance-check failure a
	// recycle produces.
	real, err := proctree.Lookup(ours)
	if err != nil {
		t.Fatalf("proctree.Lookup(ours=%d): %v", ours, err)
	}
	stale := real
	stale.StartID = real.StartID + 1

	origCapture := captureDaemonIdentity
	t.Cleanup(func() { captureDaemonIdentity = origCapture })
	captureDaemonIdentity = func(int) proctree.Process { return stale }

	origScan := scopedDaemonScanFn
	t.Cleanup(func() { scopedDaemonScanFn = origScan })
	scopedDaemonScanFn = func() ([]int, error) { return []int{ours}, nil }

	stopped, unverified, err := StopOrphanDaemons(ourHome)
	if err == nil {
		t.Fatalf("StopOrphanDaemons returned nil error; want errSignalTargetChanged because the " +
			"captured identity no longer matches the live process (recycled-PID simulation)")
	}
	if !errors.Is(err, errSignalTargetChanged) {
		t.Fatalf("StopOrphanDaemons err=%v; want errSignalTargetChanged — a PID whose captured "+
			"identity no longer matches the live process must not be signaled, and the refusal "+
			"must surface so reset does not report the recycled PID as stopped", err)
	}
	if len(stopped) != 0 {
		t.Errorf("stopped = %v; want none — a recycled PID must not be reported as successfully "+
			"stopped (the pre-fix bug reported the SIGKILL'd replacement as stopped)", stopped)
	}
	if len(unverified) != 0 {
		t.Errorf("unverified = %v; want none (the PID was proven ours at SIGTERM time; the "+
			"refusal is an identity change, not an unverified home)", unverified)
	}
	if !pidLooksAlive(ours) {
		t.Fatalf("daemon pid %d was killed despite a stale captured identity; the identity-checked "+
			"escalation must refuse (ErrIdentityChanged) rather than SIGTERM/SIGKILL the live process "+
			"the kernel recycled the PID onto", ours)
	}
}

// TestStopOrphanDaemons_ZeroSnapshotFailsClosed pins the fail-closed half of
// the fix: a PID verifyScopedDaemon proved ours whose identity captureDaemonIdentity
// could not bind (the daemon exited between verification and the snapshot, or the
// backend could not read its identity) returns a zero Process. signalClassifiedDaemon
// must NOT fall back to a PID-only signal that could hit a recycled PID — the
// captured identity is the whole of the signal's safety (#4793). When the PID is
// still alive (recycled onto another process before the snapshot landed) it
// surfaces errSignalTargetChanged and leaves the PID untouched, the same refusal
// TestStopOrphanDaemons_RecycledPIDNotSignaled asserts for a stale snapshot.
func TestStopOrphanDaemons_ZeroSnapshotFailsClosed(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("proctree identity needs /proc")
	}
	ourHome := t.TempDir()
	ours := spawnFakeDaemonWithHome(t, ourHome)

	// A zero snapshot models a daemon whose identity could not be bound at the
	// classification decision (it exited before the snapshot, or the backend
	// could not read it). The PID is still alive, so this is the recycled
	// shape; signalClassifiedDaemon must fail closed rather than signal it.
	origCapture := captureDaemonIdentity
	t.Cleanup(func() { captureDaemonIdentity = origCapture })
	captureDaemonIdentity = func(int) proctree.Process { return proctree.Process{} }

	origScan := scopedDaemonScanFn
	t.Cleanup(func() { scopedDaemonScanFn = origScan })
	scopedDaemonScanFn = func() ([]int, error) { return []int{ours}, nil }

	stopped, unverified, err := StopOrphanDaemons(ourHome)
	if err == nil {
		t.Fatalf("StopOrphanDaemons returned nil error; want errSignalTargetChanged because a " +
			"live PID with a zero identity snapshot must fail closed rather than be signaled by PID")
	}
	if !errors.Is(err, errSignalTargetChanged) {
		t.Fatalf("StopOrphanDaemons err=%v; want errSignalTargetChanged — a live PID whose "+
			"identity could not be captured must fail closed (errSignalTargetChanged), not fall "+
			"back to a PID-only signal that could hit a recycled PID", err)
	}
	if len(stopped) != 0 {
		t.Errorf("stopped = %v; want none — a PID with no capturable identity must not be "+
			"reported as stopped", stopped)
	}
	if len(unverified) != 0 {
		t.Errorf("unverified = %v; want none (the PID was proven ours at SIGTERM time; the "+
			"refusal is an unbindable identity, not an unverified home)", unverified)
	}
	if !pidLooksAlive(ours) {
		t.Fatalf("daemon pid %d was killed despite a zero identity snapshot; the identity-checked "+
			"escalation must fail closed rather than fall back to signalAndWait's PID-only signal", ours)
	}
}
