//go:build linux

package systemdunit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startStubHookLauncher starts a live process that looks EXACTLY like a
// systemd-run which has not registered its scope yet: argv[0] is systemd-run and
// its argv carries --unit=<unit>.
//
// It is a stub and not a real systemd-run on purpose. The window under test is
// the sub-second interval between systemd-run's execve and its
// StartTransientUnit reply, and a test that tried to catch a real launcher
// inside it would be a coin flip about whether the defect was even reachable on
// that run. Holding a stub open makes the window a fact of the test.
//
// The argv[0] spoof is not a trick either: exec.Command sets Args[0] to the NAME
// it was given, so production's launcher reports argv[0]="systemd-run" in
// /proc/<pid>/cmdline for exactly the reason this one does. Everything after
// sh's command string is $0/$1/… to the shell, which ignores it — so the process
// blocks while carrying the argv the sweep has to recognise.
func startStubHookLauncher(t *testing.T, program, unit, body string) int {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no sh to build a stub launcher from: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "launcher.pid")
	cmd := &exec.Cmd{
		Path: shell,
		Args: []string{
			program, "-c", fmt.Sprintf("printf '%%s' \"$$\" > %q; %s", pidFile, body),
			// Everything from here on is positional to sh and reproduces the
			// launcher's real argv: the scope flags it is about to register with.
			"--user", "--scope", "--quiet", "--collect", "--unit=" + unit,
			"--property=TimeoutStopSec=" + HookScopeStopTimeout, "--", "sh", "-c", "make dev_install",
		},
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub launcher: %v", err)
	}
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-reaped
	})
	// Wait for the pid file: until execve completes the child's /proc entry
	// still carries the TEST binary's argv, so starting the sweep before then
	// would test a process that has not become the launcher yet.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stub launcher never published its pid to %s", pidFile)
	return 0
}

// #3667. Between systemd-run's execve and its StartTransientUnit reply the unit
// DOES NOT EXIST. A successor daemon that sweeps by unit prefix finds nothing
// there, and before this change read that silence as proof no hook survives — so
// it went on to rebuild or relocate the tree while the launcher was still about
// to exec the operator's command with the pre-move cwd (#2770, across the
// restart boundary).
//
// The launcher here exits on its own, so the assertion is that the sweep WAITED
// for it rather than that it refused: a launcher is a hook that is about to
// start, and once it is gone the sweep may legitimately report success.
func TestStopHookScopesWaitsForALauncherThatHasNotRegisteredItsScope(t *testing.T) {
	installSystemctlShim(t, "exit 0\n") // the manager lists nothing: this IS the window
	prefix := HookScopeUnitPrefix("s3667")
	release := holdStubHookLauncher(t, "systemd-run", HookScopeUnit(prefix, "g0", 0))

	done := make(chan error, 1)
	go func() { done <- StopHookScopes(prefix) }()

	// The claim, stated as an observation rather than as arithmetic: at a moment
	// when the launcher is definitely still alive, the sweep has not returned.
	select {
	case err := <-done:
		t.Fatalf("StopHookScopes returned (%v) while a systemd-run for %s-* was still live and unregistered; "+
			"the caller then rebuilds or moves the tree the hook is about to be exec'd into (#3667)", err, prefix)
	case <-time.After(stillBlockedWindow):
	}

	release()
	if err := waitForSweep(t, done); err != nil {
		t.Fatalf("StopHookScopes refused after the launcher was gone: %v", err)
	}
}

// The argv match is a STRONG identity, and this is the claim that makes it one.
// The prefix carries the session id and the generation that minted it, so a
// launcher naming a different prefix belongs to another session — adopting it
// would make one session's rebuild wait on another session's hook, and on a box
// running a dozen sessions that is a permanent wedge rather than a rare one.
func TestAForeignLauncherIsNotAdopted(t *testing.T) {
	installSystemctlShim(t, "exit 0\n")
	ours := HookScopeUnitPrefix("s1")
	theirs := HookScopeUnitPrefix("s2")
	startStubHookLauncher(t, "systemd-run", HookScopeUnit(theirs, "g0", 0), "while :; do sleep 1; done")

	launchers, err := RunningHookLaunchers(ours)
	if err != nil {
		t.Fatalf("RunningHookLaunchers: %v", err)
	}
	if len(launchers) != 0 {
		t.Fatalf("a systemd-run for %s was adopted by the sweep for %s: %+v", theirs, ours, launchers)
	}
	// And the sweep it feeds must not wait on it either.
	start := time.Now()
	if err := StopHookScopes(ours); err != nil {
		t.Fatalf("StopHookScopes refused because of another session's launcher: %v", err)
	}
	// Well under the settle budget, and well over the cost of the sweep itself:
	// the /proc scan measures ~100ms on a box at load 55, and what this rules out
	// is a wait, not a slow scan.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("StopHookScopes waited %s on a launcher that was never ours", elapsed)
	}

	// The same process IS found by the sweep that owns it, so the negative above
	// is about the prefix and not about the stub failing to look like a launcher.
	theirLaunchers, err := RunningHookLaunchers(theirs)
	if err != nil {
		t.Fatalf("RunningHookLaunchers: %v", err)
	}
	if len(theirLaunchers) != 1 || theirLaunchers[0].Unit != HookScopeUnit(theirs, "g0", 0) {
		t.Fatalf("the stub launcher was not recognised by its own prefix: %+v", theirLaunchers)
	}
}

// Identity is the PROGRAM plus the flag, never the flag alone. A process that
// merely mentions a scope name — a grep, an editor, the hook script itself —
// will never register anything, so waiting on one would wedge the rebuild until
// the settle timeout and then refuse, forever.
func TestAProcessThatMerelyNamesAScopeIsNotALauncher(t *testing.T) {
	prefix := HookScopeUnitPrefix("s1")
	unit := HookScopeUnit(prefix, "g0", 0)
	startStubHookLauncher(t, "grep", unit, "while :; do sleep 1; done")

	launchers, err := RunningHookLaunchers(prefix)
	if err != nil {
		t.Fatalf("RunningHookLaunchers: %v", err)
	}
	if len(launchers) != 0 {
		t.Fatalf("a process that is not systemd-run was adopted as a launcher: %+v", launchers)
	}
}

func TestHookLauncherUnitReadsSystemdRunsOwnOptions(t *testing.T) {
	for name, testCase := range map[string]struct {
		argv []string
		unit string
		ok   bool
	}{
		"joined form": {
			argv: []string{"systemd-run", "--user", "--scope", "--unit=af-hook-s1-g-0.scope", "--", "sh", "-c", "true"},
			unit: "af-hook-s1-g-0.scope", ok: true,
		},
		"separate form": {
			argv: []string{"systemd-run", "--user", "--unit", "af-hook-s1-g-0.scope", "--", "sh"},
			unit: "af-hook-s1-g-0.scope", ok: true,
		},
		"absolute path": {
			argv: []string{"/usr/bin/systemd-run", "--unit=af-hook-s1-g-0.scope"},
			unit: "af-hook-s1-g-0.scope", ok: true,
		},
		// Past "--" the words are the COMMAND being scoped. A hook script that
		// contains the text is not a launcher for it, and reading it as one
		// would hold up the rebuild behind a process that never registers.
		"only inside the scoped command": {
			argv: []string{"systemd-run", "--user", "--scope", "--", "sh", "-c", "af x --unit=af-hook-s1-g-0.scope"},
		},
		"another program entirely": {
			argv: []string{"pgrep", "--unit=af-hook-s1-g-0.scope"},
		},
		"no argv at all": {argv: nil},
		"a dangling --unit": {
			argv: []string{"systemd-run", "--user", "--unit"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			unit, ok := hookLauncherUnit(testCase.argv)
			if ok != testCase.ok || unit != testCase.unit {
				t.Fatalf("hookLauncherUnit(%q) = %q, %v; want %q, %v", testCase.argv, unit, ok, testCase.unit, testCase.ok)
			}
		})
	}
}

// A launcher that never becomes a scope must FAIL the sweep, not time out into
// success. The caller is about to rebuild or move the tree, and a hook that is
// still on its way into it is the one thing this ordering exists to prevent.
func TestStopHookScopesRefusesWhenALauncherNeverRegisters(t *testing.T) {
	installSystemctlShim(t, "exit 0\n")
	shortenLauncherSettle(t)
	prefix := HookScopeUnitPrefix("s1")
	unit := HookScopeUnit(prefix, "g0", 0)
	pid := startStubHookLauncher(t, "systemd-run", unit, "while :; do sleep 1; done")

	err := StopHookScopes(prefix)
	if err == nil {
		t.Fatal("a launcher that never registered its scope was reported as no survivor at all")
	}
	// The operator has to be able to find the process the refusal is about.
	for _, want := range []string{strconv.Itoa(pid), unit} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %v", want, err)
		}
	}
}

// The handoff. A launcher that DOES register becomes a scope the manager can
// name, and the sweep must stop that scope — while still not returning until the
// launcher itself is gone.
func TestStopHookScopesStopsTheScopeAndStillWaitsForTheLauncher(t *testing.T) {
	prefix := HookScopeUnitPrefix("s1")
	unit := HookScopeUnit(prefix, "g0", 0)
	stopped := filepath.Join(t.TempDir(), "stopped")
	logPath := installSystemctlShim(t, `case "$*" in
  *" stop "*) : > '`+stopped+`'; exit 0 ;;
esac
if [ -f '`+stopped+`' ]; then exit 0; fi
printf '%s\n' '`+unit+` loaded active running Hook'
exit 0
`)
	release := holdStubHookLauncher(t, "systemd-run", HookScopeUnit(prefix, "g1", 0))

	done := make(chan error, 1)
	go func() { done <- StopHookScopes(prefix) }()
	select {
	case err := <-done:
		t.Fatalf("StopHookScopes returned (%v) before the unregistered launcher was gone", err)
	case <-time.After(stillBlockedWindow):
	}

	release()
	if err := waitForSweep(t, done); err != nil {
		t.Fatalf("StopHookScopes: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("systemctl was never consulted: %v", err)
	}
	if !strings.Contains(string(log), "stop -- "+unit) {
		t.Fatalf("the registered scope was never stopped:\n%s", log)
	}
}

// stillBlockedWindow is how long a test watches a sweep that must NOT have
// returned. Before the fix these returned in single-digit milliseconds, so this
// is ~100x the margin — and it can never be too SHORT for a correct
// implementation, which cannot finish at all while its launcher is held.
const stillBlockedWindow = 500 * time.Millisecond

// holdStubHookLauncher starts a stub launcher that holds until the returned
// function releases it, and is the reason none of these tests race a timer.
//
// An earlier draft had the stub `sleep 2` and asserted the sweep took at least
// 2s. That is unsound: the stub starts its own clock BEFORE the test can observe
// it, so the sweep legitimately finishes a few milliseconds short of the wall
// time the test measured — 1.987s against a 2s floor, which is exactly how it
// failed in CI. A release the test performs itself has no such gap.
func holdStubHookLauncher(t *testing.T, program, unit string) (release func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "release")
	// `sleep 1` rather than a fractional or busy loop: portable to any POSIX sh,
	// and the wait for the release below is generous enough to absorb it.
	startStubHookLauncher(t, program, unit, fmt.Sprintf("while [ ! -f %q ]; do sleep 1; done", gate))
	return func() {
		t.Helper()
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatalf("release the stub launcher: %v", err)
		}
	}
}

// waitForSweep collects the sweep's result once the launcher has been released.
// The bound is generous on purpose: it is not the property under test, only a
// guard against hanging the suite.
func waitForSweep(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(60 * time.Second):
		t.Fatal("the sweep never returned after its launcher exited")
		return nil
	}
}

// shortenLauncherSettle collapses the wait for a launcher that will never
// register, so the refusal is observable without a 15-second test.
func shortenLauncherSettle(t *testing.T) {
	t.Helper()
	previousTimeout, previousInterval := hookLauncherSettleTimeout, hookLauncherPollInterval
	t.Cleanup(func() {
		hookLauncherSettleTimeout, hookLauncherPollInterval = previousTimeout, previousInterval
	})
	hookLauncherSettleTimeout = 300 * time.Millisecond
	hookLauncherPollInterval = 20 * time.Millisecond
}
