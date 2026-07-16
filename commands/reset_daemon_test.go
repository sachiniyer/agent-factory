package commands

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"

	"github.com/spf13/cobra"
)

// These tests cover the ordering contract of a factory reset — that the daemon
// is provably gone before a single byte of state is deleted, and that the
// autostart supervisor is disarmed before anything is signalled.
//
// EVERY destructive boundary is faked. Nothing here starts, signals, or scans
// for a real daemon, and nothing touches the real tmux server or the real AF
// home: the daemon scan is host-wide and the teardown is destructive, so a test
// that exercised the real thing would stop the developer's own daemon and bounce
// their live sessions. The seams in reset.go exist for this reason. If you add a
// case here, fake the boundary — never reach for the real one.

// daemonFlushDelay is how long the fake daemon takes to reach its final
// SaveInstances after acknowledging the stop. It models the real gap the wait
// exists to cover; the tests synchronise on the flush rather than sleeping for
// it, so the delay only has to be long enough that a wipe which does NOT wait
// would win the race and be caught.
const daemonFlushDelay = 150 * time.Millisecond

// fakeDaemonSeams neutralises every daemon/tmux boundary of the reset, so a
// test opts IN to the behaviour it wants to model rather than inheriting a live
// one. Restoration is automatic.
func fakeDaemonSeams(t *testing.T) {
	t.Helper()
	origInstalled := autostartInstalledFn
	origServes := autostartUnitServesHomeFn
	origPause := pauseAutostartUnitFn
	origResume := resumeAutostartUnitFn
	origStop := stopDaemonFn
	origWait := waitForShutdownCompletionFn
	origOrphans := stopOrphanDaemonsFn
	origAssert := assertNoLiveDaemonFn
	origSockets := removeRuntimeSocketFn
	origTmux := cleanupTmuxSessionsFn
	origForce := resetForceFlag
	t.Cleanup(func() {
		autostartInstalledFn = origInstalled
		autostartUnitServesHomeFn = origServes
		pauseAutostartUnitFn = origPause
		resumeAutostartUnitFn = origResume
		stopDaemonFn = origStop
		waitForShutdownCompletionFn = origWait
		stopOrphanDaemonsFn = origOrphans
		assertNoLiveDaemonFn = origAssert
		removeRuntimeSocketFn = origSockets
		cleanupTmuxSessionsFn = origTmux
		resetForceFlag = origForce
	})

	// Default to "no unit installed" so a test that forgets to say otherwise
	// can never reach the real systemctl/launchctl.
	autostartInstalledFn = func() bool { return false }
	autostartUnitServesHomeFn = func(string) (bool, bool, error) { return false, false, nil }
	pauseAutostartUnitFn = func() error { t.Fatal("pauseAutostartUnitFn called without a fake"); return nil }
	resumeAutostartUnitFn = func() error { t.Fatal("resumeAutostartUnitFn called without a fake"); return nil }
	stopDaemonFn = func() (bool, error) { return false, nil }
	waitForShutdownCompletionFn = func() error { return nil }
	stopOrphanDaemonsFn = func(string) ([]int, []int, error) { return nil, nil, nil }
	assertNoLiveDaemonFn = func(string) error { return nil }
	removeRuntimeSocketFn = func(string) ([]string, error) { return nil, nil }
	cleanupTmuxSessionsFn = func() error { return nil }
	// The typed-WIPE gate is covered by TestResetConfirmed; bypass it here so
	// these tests exercise the teardown order rather than the prompt.
	resetForceFlag = true
}

// runResetCapture drives the real runReset and returns its output AND error.
// reset_test.go's runResetForTest fatals on error; the abort paths here are the
// behaviour under test, so they need the error back.
func runResetCapture(t *testing.T) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	err := runReset(cmd, nil)
	return buf.String(), err
}

// seedResetHome builds a throwaway AF home with a mock repo and AF records, and
// returns the home plus the path of the instances file the daemon would flush.
// resetHomeUnderTest is the AF home the most recent seedResetHome created, so a
// test can assert the reset scoped its daemon work to exactly that home.
var resetHomeUnderTest string

func seedResetHome(t *testing.T) (home, instancesPath string) {
	t.Helper()
	home = t.TempDir()
	resetHomeUnderTest = home
	t.Setenv("AGENT_FACTORY_HOME", home)
	// Keep the wipe scoped to the seeded mock repo, never the repo the test
	// binary runs from (mirrors TestFactoryReset_WipesEverythingKeepsRepoAndConfig).
	t.Chdir(t.TempDir())

	repo, liveWT, reusedWT := seedMockRepo(t, home)
	seedAFState(t, home, repo, liveWT, reusedWT)
	repoID := config.RepoIDFromRoot(repo)
	return home, filepath.Join(home, "instances", repoID, "instances.json")
}

// TestFactoryReset_WaitsForDaemonFlushBeforeWipe is the regression lock for the
// resurrection race: the daemon is the single writer (#960) and persists its
// whole in-memory session set on the way out (RunDaemon's final SaveInstances),
// so a reset that deletes instances.json while that flush is still pending gets
// the old sessions written straight back — a "factory reset" that hands the user
// their sessions back.
//
// The fake models the real shutdown shape: the stop is acknowledged
// immediately, the flush lands a moment later, and the control socket only
// stops answering afterwards (RunDaemon closes the listener on its deferred
// teardown, AFTER SaveInstances — which is exactly why waiting on the socket is
// a sufficient barrier for the flush).
//
// To watch it fail: delete the waitForShutdownCompletionFn call from
// stopDaemonsForReset. The wipe then runs before the flush, the flush recreates
// instances.json, and this test catches it.
func TestFactoryReset_WaitsForDaemonFlushBeforeWipe(t *testing.T) {
	_, instancesPath := seedResetHome(t)

	// What the daemon is holding in memory and will write back out.
	flushBytes, err := os.ReadFile(instancesPath)
	if err != nil {
		t.Fatalf("read seeded instances: %v", err)
	}

	fakeDaemonSeams(t)
	flushed := make(chan struct{})
	stopDaemonFn = func() (bool, error) {
		go func() {
			defer close(flushed)
			time.Sleep(daemonFlushDelay)
			if err := os.MkdirAll(filepath.Dir(instancesPath), 0o700); err != nil {
				return
			}
			_ = os.WriteFile(instancesPath, flushBytes, 0o600)
		}()
		return true, nil // signal delivered; the process has NOT exited yet
	}
	waitForShutdownCompletionFn = func() error {
		select {
		case <-flushed:
			return nil
		case <-time.After(10 * time.Second):
			return errors.New("fake daemon never finished shutting down")
		}
	}

	if _, err := runResetCapture(t); err != nil {
		t.Fatalf("runReset: %v", err)
	}

	// Sync on the flush so the assertion is about ordering, not timing: by here
	// the daemon has definitely written whatever it was going to write.
	select {
	case <-flushed:
	case <-time.After(10 * time.Second):
		t.Fatal("fake daemon flush never ran")
	}

	if _, err := os.Stat(instancesPath); !os.IsNotExist(err) {
		t.Fatalf("instances.json is back at %s after the reset (err=%v): the wipe ran before the "+
			"daemon's shutdown flush, so the daemon resurrected the sessions the reset deleted",
			instancesPath, err)
	}
}

// TestFactoryReset_DisarmsSupervisorBeforeStoppingDaemons locks vector B: the
// autostart supervisor respawns a daemon that dies badly (systemd
// Restart=on-failure on the SIGKILL escalation; launchd KeepAlive on any death
// by signal). If reset signals daemons before stopping the unit, the supervisor
// can start a fresh daemon into the middle of the wipe, which then re-persists
// the state being deleted. Stopping the unit FIRST is what closes that window,
// and supervision must be handed back afterwards.
func TestFactoryReset_DisarmsSupervisorBeforeStoppingDaemons(t *testing.T) {
	seedResetHome(t)
	fakeDaemonSeams(t)

	var order []string
	autostartInstalledFn = func() bool { return true }
	pauseAutostartUnitFn = func() error { order = append(order, "pause-unit"); return nil }
	resumeAutostartUnitFn = func() error { order = append(order, "resume-unit"); return nil }
	stopDaemonFn = func() (bool, error) { order = append(order, "stop-daemon"); return true, nil }
	autostartUnitServesHomeFn = func(string) (bool, bool, error) { return true, true, nil }
	stopOrphanDaemonsFn = func(string) ([]int, []int, error) {
		order = append(order, "stop-orphans")
		return []int{4242}, nil, nil
	}
	assertNoLiveDaemonFn = func(string) error { order = append(order, "assert-clear"); return nil }
	removeRuntimeSocketFn = func(string) ([]string, error) { order = append(order, "sockets"); return nil, nil }

	out, err := runResetCapture(t)
	if err != nil {
		t.Fatalf("runReset: %v", err)
	}

	want := []string{"pause-unit", "stop-daemon", "stop-orphans", "assert-clear", "sockets", "resume-unit"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("teardown order = %v, want %v", order, want)
	}
	// The PIDs reset signalled are the only record the user has of what an
	// irreversible command did to their process table.
	if !strings.Contains(out, "4242") {
		t.Errorf("reset output does not name the stopped daemon PID 4242:\n%s", out)
	}
}

// TestFactoryReset_AbortsWhenShutdownDoesNotComplete covers the daemon that
// refuses to exit within the grace. Wiping under a live single-writer daemon
// does not half-work — the daemon wins and re-persists — so an unprovable
// shutdown must abort rather than delete. Aborting costs the user one command;
// guessing costs them a half-wiped home.
func TestFactoryReset_AbortsWhenShutdownDoesNotComplete(t *testing.T) {
	_, instancesPath := seedResetHome(t)
	fakeDaemonSeams(t)

	stopDaemonFn = func() (bool, error) { return true, nil }
	waitForShutdownCompletionFn = func() error {
		return errors.New("daemon control socket still answering 5s after shutdown was acknowledged")
	}
	wiped := false
	removeRuntimeSocketFn = func(string) ([]string, error) { wiped = true; return nil, nil }

	out, err := runResetCapture(t)
	if err == nil {
		t.Fatal("runReset returned nil; want an abort when the daemon has not finished shutting down")
	}
	if !strings.Contains(err.Error(), "did not finish shutting down") {
		t.Errorf("abort error = %v, want it to name the incomplete shutdown", err)
	}
	if wiped {
		t.Error("reset removed sockets after failing to confirm the daemon was gone")
	}
	if _, statErr := os.Stat(instancesPath); statErr != nil {
		t.Errorf("instances.json was touched by an aborted reset: %v", statErr)
	}
	if !strings.Contains(out, "Nothing was removed") {
		t.Errorf("aborted reset did not tell the user nothing was removed:\n%s", out)
	}
}

// TestFactoryReset_AbortsWhenDaemonStillRunning covers the final gate: a daemon
// that survived every stop above (a supervisor we could not disarm respawned
// one, or an orphan refused to die). The reset must abort with the PIDs named —
// and must hand supervision back, because an aborted reset has to leave the
// machine exactly as it found it.
func TestFactoryReset_AbortsWhenDaemonStillRunning(t *testing.T) {
	_, instancesPath := seedResetHome(t)
	fakeDaemonSeams(t)

	autostartInstalledFn = func() bool { return true }
	autostartUnitServesHomeFn = func(string) (bool, bool, error) { return true, true, nil }
	pauseAutostartUnitFn = func() error { return nil }
	resumed := false
	resumeAutostartUnitFn = func() error { resumed = true; return nil }
	assertNoLiveDaemonFn = func(string) error {
		return errors.New("af daemon(s) still running for this AF home: 7777")
	}
	wiped := false
	removeRuntimeSocketFn = func(string) ([]string, error) { wiped = true; return nil, nil }

	_, err := runResetCapture(t)
	if err == nil {
		t.Fatal("runReset returned nil; want an abort while a daemon is still running")
	}
	if !strings.Contains(err.Error(), "refusing to wipe") || !strings.Contains(err.Error(), "7777") {
		t.Errorf("abort error = %v, want it to refuse the wipe and name PID 7777", err)
	}
	if wiped {
		t.Error("reset proceeded to the wipe with a daemon still running")
	}
	if _, statErr := os.Stat(instancesPath); statErr != nil {
		t.Errorf("instances.json was touched by an aborted reset: %v", statErr)
	}
	if !resumed {
		t.Error("an aborted reset left the autostart unit paused; supervision must be handed back")
	}
}

// TestFactoryReset_StopsUnverifiableDaemonsNever guards the scoping promise the
// help text makes: a daemon whose AF home cannot be established is REPORTED,
// never signalled. Killing another home's daemon would be a far worse bug than
// the stale daemon reset is cleaning up, so "I could not tell" must resolve to
// "leave it alone".
func TestFactoryReset_StopsUnverifiableDaemonsNever(t *testing.T) {
	seedResetHome(t)
	fakeDaemonSeams(t)

	stopOrphanDaemonsFn = func(string) ([]int, []int, error) {
		return nil, []int{9191}, nil // one unverifiable daemon, none stopped
	}

	out, err := runResetCapture(t)
	if err != nil {
		t.Fatalf("runReset: %v", err)
	}
	if !strings.Contains(out, "9191") || !strings.Contains(out, "could not be verified") {
		t.Errorf("reset did not report the unverifiable daemon it left running:\n%s", out)
	}
}

// TestRemoveRuntimeSockets_NamesEveryDaemonSocket pins the socket set a reset
// clears. A socket that outlives its daemon points the next client at a dead
// endpoint, which is the failure this change exists to remove.
func TestRemoveRuntimeSockets_NamesEveryDaemonSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	// The two daemon sockets, plus a VS Code editor socket matching the name
	// shape the daemon mints, plus a file that must survive.
	mustWrite := func(rel string) string {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sock := mustWrite("daemon.sock")
	httpSock := mustWrite("daemon-http.sock")
	keep := mustWrite("config.toml")

	removed, err := daemon.RemoveRuntimeSockets(home)
	if err != nil {
		t.Fatalf("RemoveRuntimeSockets: %v", err)
	}
	for _, p := range []string{sock, httpSock} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("%s survived the socket sweep", p)
		}
	}
	if len(removed) != 2 {
		t.Errorf("removed = %v, want the two daemon sockets", removed)
	}
	if _, statErr := os.Stat(keep); statErr != nil {
		t.Errorf("the socket sweep removed config.toml: %v", statErr)
	}

	// Idempotent: a second sweep is a clean no-op, matching the reset's
	// re-runnable contract.
	again, err := daemon.RemoveRuntimeSockets(home)
	if err != nil {
		t.Fatalf("second RemoveRuntimeSockets: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second sweep removed %v, want nothing", again)
	}
}

// TestFactoryReset_LeavesOtherHomesAutostartUnitAlone is the regression lock for
// the #1916 P2: the autostart pause was gated on the unit FILE existing, not on
// the unit serving the home being reset.
//
// The unit bakes its own AGENT_FACTORY_HOME at install time, so it has no
// relationship to the AGENT_FACTORY_HOME the resetting process carries. Gated on
// file existence alone, `AGENT_FACTORY_HOME=/tmp/sandbox af reset` reaches out
// and stops the developer's REAL daemon — and if the deferred resume then fails,
// leaves it stopped. On a shared box that is an outage caused by a sandbox.
//
// To watch it fail: this reproduces against master (ddb72aa), where runReset
// pauses on `if autostartInstalledFn()` with no home check.
func TestFactoryReset_LeavesOtherHomesAutostartUnitAlone(t *testing.T) {
	seedResetHome(t) // we are resetting a throwaway sandbox home
	fakeDaemonSeams(t)

	// A unit file EXISTS on this machine — it just belongs to a different home.
	autostartInstalledFn = func() bool { return true }
	autostartUnitServesHomeFn = func(string) (bool, bool, error) { return false, true, nil }
	pauseAutostartUnitFn = func() error {
		t.Fatal("reset paused an autostart unit that serves a DIFFERENT AF home — " +
			"this stops the real daemon on a shared box (#1916 P2)")
		return nil
	}
	resumeAutostartUnitFn = func() error {
		t.Fatal("reset resumed an autostart unit it should never have touched")
		return nil
	}
	// Nor may it stop that home's daemons: same bug class, same gate.
	stopOrphanDaemonsFn = func(configDir string) ([]int, []int, error) {
		if configDir != resetHomeUnderTest {
			t.Errorf("orphan scan scoped to %q, want the home being reset %q", configDir, resetHomeUnderTest)
		}
		return nil, nil, nil
	}

	out, err := runResetCapture(t)
	if err != nil {
		t.Fatalf("runReset: %v\n%s", err, out)
	}
	if !strings.Contains(out, "different AF home") {
		t.Errorf("reset did not report leaving the other home's unit alone:\n%s", out)
	}
}

// TestFactoryReset_ResumeFailureIsLoudAndFails covers the other half of the P2's
// blast radius: we stopped a REAL supervised daemon, so failing to start it back
// must never be a quiet warning. The unit stays down until the next login, and a
// scripted caller must see a non-zero exit.
func TestFactoryReset_ResumeFailureIsLoudAndFails(t *testing.T) {
	seedResetHome(t)
	fakeDaemonSeams(t)

	autostartInstalledFn = func() bool { return true }
	autostartUnitServesHomeFn = func(string) (bool, bool, error) { return true, true, nil }
	pauseAutostartUnitFn = func() error { return nil }
	attempts := 0
	resumeAutostartUnitFn = func() error {
		attempts++
		return errors.New("systemctl --user start failed: unit not found")
	}

	out, err := runResetCapture(t)
	if attempts != autostartResumeAttempts {
		t.Errorf("resume attempted %d times, want %d retries before giving up", attempts, autostartResumeAttempts)
	}
	if !strings.Contains(out, "ACTION REQUIRED") || !strings.Contains(out, "STOPPED") {
		t.Errorf("a failed resume did not shout about the stopped daemon:\n%s", out)
	}
	if err == nil {
		t.Error("runReset returned nil after leaving the daemon stopped; a scripted caller would never notice")
	}
}

// TestFactoryReset_UnknownUnitHomeIsLeftAlone: if we cannot establish which home
// the unit serves, we must not touch it. "I could not tell" never resolves to
// "stop it" — the same rule the daemon scan follows.
func TestFactoryReset_UnknownUnitHomeIsLeftAlone(t *testing.T) {
	seedResetHome(t)
	fakeDaemonSeams(t)

	autostartInstalledFn = func() bool { return true }
	autostartUnitServesHomeFn = func(string) (bool, bool, error) {
		return false, true, errors.New("unit file is unreadable")
	}
	pauseAutostartUnitFn = func() error {
		t.Fatal("reset paused a unit whose AF home it could not establish")
		return nil
	}

	out, err := runResetCapture(t)
	if err != nil {
		t.Fatalf("runReset: %v", err)
	}
	if !strings.Contains(out, "could not determine which AF home") {
		t.Errorf("reset did not report the unverifiable unit:\n%s", out)
	}
}
