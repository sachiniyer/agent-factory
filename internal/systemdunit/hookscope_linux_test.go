//go:build linux

package systemdunit

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func claimDaemon(t *testing.T) {
	t.Helper()
	t.Setenv(DaemonMarkerEnv, DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	stubSelfCgroup(t, "", errors.New("proc unavailable"))
}

// installSystemctlShim puts a scripted systemctl on PATH. It records every
// invocation so a test can prove the stop actually happened rather than
// inferring it from a nil error.
func installSystemctlShim(t *testing.T, body string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "systemctl.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n" + body
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write systemctl shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestTheTwoScopeShapesStayDistinct is the invariant the design turns on: the
// bound shape is right for watchers and editors, whose lifetime IS the daemon's,
// and wrong for a one-shot operator build, which must outlive a restart. A
// refactor that collapses them reintroduces #3650's rejected fix.
func TestTheTwoScopeShapesStayDistinct(t *testing.T) {
	claimDaemon(t)

	bound := newBoundChildCommandForUnit("stand-in.service", "sh", "-c", "true")
	boundArgs := strings.Join(bound.Args, " ")
	for _, want := range []string{
		"--property=BindsTo=stand-in.service",
		"--property=After=stand-in.service",
		"--property=KillMode=control-group",
		"--property=TimeoutStopSec=" + BoundChildStopTimeout,
	} {
		if !strings.Contains(boundArgs, want) {
			t.Fatalf("bound child scope lost %q: %s", want, boundArgs)
		}
	}

	unbound := NewUnboundScopeCommand("af-hook-abc-1-0.scope", "sh", "-c", "true")
	unboundArgs := strings.Join(unbound.Args, " ")
	if !strings.Contains(unboundArgs, "--unit=af-hook-abc-1-0.scope") {
		t.Fatalf("unbound scope was not named: %s", unboundArgs)
	}
	// Bounded on purpose: the survivor sweep fails closed on a stop it cannot
	// complete, and systemd's default here is 90s.
	if !strings.Contains(unboundArgs, "--property=TimeoutStopSec="+HookScopeStopTimeout) {
		t.Fatalf("unbound hook scope left systemd's 90s default stop timeout in place: %s", unboundArgs)
	}
	for _, forbidden := range []string{"BindsTo", "After=", "PartOf", "Requisite", "BoundBy"} {
		if strings.Contains(unboundArgs, forbidden) {
			t.Fatalf("unbound hook scope carries %q, which stops it when the daemon stops: %s", forbidden, unboundArgs)
		}
	}
	if got := unbound.Args[len(unbound.Args)-3:]; strings.Join(got, " ") != "sh -c true" {
		t.Fatalf("scope rewrote the hook argv: %v", unbound.Args)
	}
}

func TestScopeHelpersNoOpOutsideTheDaemon(t *testing.T) {
	t.Setenv(DaemonMarkerEnv, "")
	t.Setenv("SYSTEMD_EXEC_PID", "")
	stubSelfCgroup(t, "0::/user.slice/user-1000.slice/session-3.scope\n", nil)

	for name, cmd := range map[string]string{
		"bound":   strings.Join(NewBoundChildCommand("sh", "-c", "true").Args, " "),
		"unbound": strings.Join(NewUnboundScopeCommand("af-hook-x-1-0.scope", "sh", "-c", "true").Args, " "),
	} {
		if cmd != "sh -c true" {
			t.Fatalf("%s helper relocated a non-daemon spawn: %q", name, cmd)
		}
	}
}

func TestHookScopeNamesAreDerivedAndUnitSafe(t *testing.T) {
	if got := HookScopeUnitPrefix(""); got != "" {
		t.Fatalf("empty session id derived prefix %q; absence must mean no scope", got)
	}
	if got := HookScopeUnit("", "gen", 0); got != "" {
		t.Fatalf("empty prefix derived unit %q", got)
	}
	prefix := HookScopeUnitPrefix("a1b2c3d4-0000-4000-8000-000000000000")
	if prefix != "af-hook-a1b2c3d4-0000-4000-8000-000000000000" {
		t.Fatalf("unexpected prefix %q", prefix)
	}
	unit := HookScopeUnit(prefix, "abc123", 7)
	if unit != prefix+"-abc123-7.scope" {
		t.Fatalf("unexpected unit %q", unit)
	}
	// A unit name systemd would reject makes the hook fail to start at all, so
	// anything outside its unescaped character set has to be mapped, not passed.
	dirty := HookScopeUnitPrefix("id with/slash+and*star")
	for _, r := range strings.TrimPrefix(dirty, "af-hook-") {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			t.Fatalf("prefix %q contains %q, which systemd rejects in a unit name", dirty, r)
		}
	}
	if a, b := NewHookScopeGeneration(), NewHookScopeGeneration(); a == "" || a == b {
		t.Fatalf("generations must be distinct and non-empty, got %q and %q", a, b)
	}
}

func TestRunningHookScopesReadsTheManagersAnswer(t *testing.T) {
	installSystemctlShim(t, `cat <<'EOF'
af-hook-s1-g-0.scope       loaded active running Hook
af-hook-s1-g-1.scope       loaded inactive dead  Hook
af-hook-other-g-0.scope    loaded active running Hook
EOF
exit 0
`)
	units, err := RunningHookScopes("af-hook-s1")
	if err != nil {
		t.Fatalf("RunningHookScopes: %v", err)
	}
	if len(units) != 1 || units[0] != "af-hook-s1-g-0.scope" {
		t.Fatalf("RunningHookScopes = %v, want only the active scope under the prefix", units)
	}
	if units, err := RunningHookScopes(""); err != nil || units != nil {
		t.Fatalf("an empty prefix must not touch the manager: %v %v", units, err)
	}
}

// A failed read is not an empty result. Every caller of this treats "no
// survivors" as permission to rebuild the tree, so a manager that could not be
// asked must be distinguishable from a manager that answered "none".
func TestRunningHookScopesDistinguishesAnUnreachableManager(t *testing.T) {
	installSystemctlShim(t, "echo 'Failed to connect to bus: No such file or directory' >&2\nexit 1\n")
	if _, err := RunningHookScopes("af-hook-s1"); !errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("unreachable manager reported as %v, want ErrManagerUnavailable", err)
	}

	installSystemctlShim(t, "echo 'Unknown operation' >&2\nexit 1\n")
	_, err := RunningHookScopes("af-hook-s1")
	if errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("a manager that answered with a real failure was reported as unavailable: %v", err)
	}
	if err == nil {
		t.Fatal("a failing systemctl was reported as an empty result")
	}
}

// StopHookScopes must PROVE the stop. Reporting success off systemctl's exit
// code alone would hand the rebuild path a tree a hook is still writing into.
func TestStopHookScopesProvesTheScopesAreGone(t *testing.T) {
	stateDir := t.TempDir()
	stopped := filepath.Join(stateDir, "stopped")
	installSystemctlShim(t, `case "$*" in
  *stop*) : > '`+stopped+`'; exit 0 ;;
esac
if [ -f '`+stopped+`' ]; then exit 0; fi
printf '%s\n' 'af-hook-s1-g-0.scope loaded active running Hook'
exit 0
`)
	if err := StopHookScopes("af-hook-s1"); err != nil {
		t.Fatalf("StopHookScopes: %v", err)
	}

	installSystemctlShim(t, `case "$*" in
  *stop*) exit 0 ;;
esac
printf '%s\n' 'af-hook-s1-g-0.scope loaded active running Hook'
exit 0
`)
	err := StopHookScopes("af-hook-s1")
	if err == nil || !strings.Contains(err.Error(), "still active") {
		t.Fatalf("a scope that outlived its stop was reported as gone: %v", err)
	}
}

func TestStopScopeUnitsTreatsAnUnloadedUnitAsDone(t *testing.T) {
	installSystemctlShim(t, "echo 'Failed to stop af-hook-x.scope: Unit af-hook-x.scope not loaded.' >&2\nexit 5\n")
	if err := StopScopeUnits("af-hook-x.scope"); err != nil {
		t.Fatalf("an already-collected scope is the success case, got %v", err)
	}
	if err := StopScopeUnits(); err != nil {
		t.Fatalf("stopping nothing must not touch the manager: %v", err)
	}
}
