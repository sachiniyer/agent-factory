package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errFake stands in for a non-zero exit from systemctl/launchctl.
var errFake = errors.New("fake unit command failure")

// withProbeCommand replaces the bounded read-only probe runner. Separate from
// withUnitCommand because the probes are what carry the deadline and the
// answered/could-not-ask distinction.
func withProbeCommand(t *testing.T, fn func(string, ...string) ([]byte, error)) {
	t.Helper()
	prev := autostartProbeCommand
	t.Cleanup(func() { autostartProbeCommand = prev })
	autostartProbeCommand = fn
}

// exitErr builds a real *exec.ExitError with the given code, which is what
// distinguishes "the service manager answered" from "it never ran".
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	require.ErrorAs(t, err, &ee)
	return ee
}

func writeUnitFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// These tests never install, load, or signal anything: they render a unit with
// the production writer, parse it back with the production parser, and assert
// the round trip. The autostart GOOS and unit directories are the package's own
// injection points, pointed at a temp dir.

func TestParseSystemdExecStart_RoundTripsWriter(t *testing.T) {
	// Round-tripping through the real writer is the point: a change to the
	// ExecStart quoting rules that forgot the parser would surface here rather
	// than as a phantom "your autostart runs a different binary" on every
	// Linux box.
	for _, path := range []string{
		"/usr/local/bin/af",
		"/home/dev/.local/bin/af",
		"/home/dev/my apps/af",        // spaces: the reason for the quoting
		`/home/dev/we"rd/af`,          // embedded quote
		`/home/dev/back\slash/af`,     // embedded backslash
		"/home/dev/100%/af",           // systemd specifier
		"/home/dev/$HOME-literal/af",  // systemd expansion char
		"/home/dev/mix of $ % \\ /af", // all of them at once
	} {
		unit := systemdAutostartUnit(path, "/usr/bin", "/bin/bash", "/home/dev/.agent-factory")
		require.Equal(t, path, parseSystemdExecStart(unit), "round trip for %q", path)
	}
}

func TestParseSystemdExecStart_NoExecStart(t *testing.T) {
	require.Empty(t, parseSystemdExecStart("[Unit]\nDescription=nothing\n"))
}

func TestParseLaunchdProgramPath_RoundTripsWriter(t *testing.T) {
	for _, path := range []string{
		"/usr/local/bin/af",
		"/Users/dev/.local/bin/af",
		"/Users/dev/my apps/af", // spaces
		`/Users/dev/we"rd/af`,   // XML-escaped on write
		"/Users/dev/a&b/af",     // ampersand
		"/Users/dev/<tag>/af",   // angle brackets
	} {
		plist := launchdAutostartPlist(path, "/usr/bin", "/bin/zsh", "/Users/dev/.agent-factory", "/tmp/af.log")
		require.Equal(t, path, parseLaunchdProgramPath(plist), "round trip for %q", path)
	}
}

func TestParseLaunchdProgramPath_Malformed(t *testing.T) {
	require.Empty(t, parseLaunchdProgramPath("<plist><dict></dict></plist>"))
}

// InspectAutostart must report the binary the installed unit launches — the
// fact the split-brain check is built on.
func TestInspectAutostart_ReadsExecPathFromInstalledUnit(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")

	unitPath := filepath.Join(dir, autostartUnitName)
	writeUnitFile(t, unitPath, systemdAutostartUnit("/usr/local/bin/af", "/usr/bin", "/bin/bash", ""))

	info := InspectAutostart()
	require.True(t, info.Supported)
	require.True(t, info.Exists)
	require.Equal(t, unitPath, info.Path)
	require.Equal(t, "/usr/local/bin/af", info.ExecPath)
	require.NoError(t, info.Err)
}

func TestInspectAutostart_NoUnitInstalled(t *testing.T) {
	withAutostartTestEnv(t, "linux")

	info := InspectAutostart()
	require.True(t, info.Supported)
	require.False(t, info.Exists)
	require.NoError(t, info.Err, "an absent unit is a normal state, not an error")
}

// A unit that exists but cannot be read must still report Exists: callers gate
// their checks on it, so a false here silently drops both the error and every
// check behind it (#1044).
func TestInspectAutostart_PresentButUnreadable_StillExists(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads unreadable files, so the permission case cannot be staged")
	}
	dir := withAutostartTestEnv(t, "linux")
	unitPath := filepath.Join(dir, autostartUnitName)
	writeUnitFile(t, unitPath, systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	require.NoError(t, os.Chmod(unitPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unitPath, 0o644) })

	info := InspectAutostart()
	require.True(t, info.Exists, "the unit is installed; unreadable is not absent")
	require.Error(t, info.Err, "the read failure must be reported, not swallowed")
	require.Empty(t, info.ExecPath)
}

// The supervision probe must run anyway: systemctl answers by unit name, not by
// reading the file, and whether the unit is active is exactly what the user needs.
func TestAutostartSupervision_UnreadableUnitStillProbesServiceManager(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads unreadable files, so the permission case cannot be staged")
	}
	dir := withAutostartTestEnv(t, "linux")
	unitPath := filepath.Join(dir, autostartUnitName)
	writeUnitFile(t, unitPath, systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	require.NoError(t, os.Chmod(unitPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unitPath, 0o644) })

	probed := false
	withProbeCommand(t, func(_ string, args ...string) ([]byte, error) {
		probed = true
		if args[1] == "is-enabled" {
			return []byte("enabled\n"), nil
		}
		return []byte("inactive\n"), errFake
	})

	info := AutostartSupervision()
	require.True(t, probed, "an unreadable unit file must not skip the service-manager probe")
	require.True(t, info.UnitPresent)
	require.Error(t, info.Err, "the read failure rides along for the caller to report")
	require.Equal(t, ProbeNo, info.Active)
}

func TestInspectAutostart_UnsupportedPlatform(t *testing.T) {
	withAutostartTestEnv(t, "plan9")
	info := InspectAutostart()
	require.False(t, info.Supported)
}

func TestAutostartSupervision_LinuxEnabledActive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))

	withProbeCommand(t, func(name string, args ...string) ([]byte, error) {
		require.Equal(t, "systemctl", name)
		switch args[1] {
		case "is-enabled":
			return []byte("enabled\n"), nil
		case "is-active":
			return []byte("active\n"), nil
		}
		return nil, errFake
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.Equal(t, ProbeYes, info.Enabled)
	require.Equal(t, ProbeYes, info.Active)
}

func TestAutostartSupervision_LinuxInactive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))

	withProbeCommand(t, func(_ string, args ...string) ([]byte, error) {
		if args[1] == "is-enabled" {
			return []byte("enabled\n"), nil
		}
		return []byte("inactive\n"), errFake // systemctl exits non-zero for inactive
	})

	info := AutostartSupervision()
	require.Equal(t, ProbeYes, info.Enabled)
	require.Equal(t, ProbeNo, info.Active)
	require.Contains(t, info.Detail, "inactive")
}

// The macOS domain mismatch: `launchctl print gui/<uid>/…` misses the agent,
// but the domain-agnostic `launchctl list` finds it — so it is loaded
// somewhere the restart path cannot reach.
func TestAutostartSupervision_DarwinLoadedOutsideGUIDomain(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))

	withProbeCommand(t, func(_ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "print":
			// launchd answered: not in gui/<uid>.
			return []byte("Could not find service"), exitErr(t, launchdNotFoundExit)
		case "list":
			return []byte("12345\t0\t" + autostartLaunchdLabel + "\n"), nil
		}
		return nil, errFake
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.NoError(t, info.ProbeErr, "launchd answered both questions")
	require.Equal(t, ProbeYes, info.LoadedElsewhere)
	require.Equal(t, launchdDomainTarget(), info.Domain)
	// Whether that other domain is actually RUNNING it is not something
	// `launchctl list` establishes — it reports that the label is known. The
	// finding is the domain mismatch; claiming a liveness we never observed
	// would be the same overreach this file exists to remove.
	require.Equal(t, ProbeUnknown, info.Active)
}

// launchctl print succeeds for a job launchd KNOWS, including one whose process
// has stopped — it reports the service's properties, not an is-running answer.
// Equating the two reports a dead daemon as healthy supervision, on the platform
// where that failure was actually hit.
func TestAutostartSupervision_DarwinLoadedButNotRunning(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))

	// A real loaded-but-stopped print block: no pid, and a state that merely
	// contains the word "running".
	withProbeCommand(t, func(_ string, args ...string) ([]byte, error) {
		if args[0] == "print" {
			return []byte("com.agent-factory.daemon = {\n" +
				"\tactive count = 0\n" +
				"\tstate = not running\n" +
				"\tlast exit code = 1\n" +
				"}\n"), nil
		}
		return nil, errFake
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.Equal(t, ProbeYes, info.Loaded, "launchd knows the job")
	require.Equal(t, ProbeNo, info.Active, "but no daemon process is running")
	require.Equal(t, ProbeNo, info.LoadedElsewhere, "it is loaded in the right domain, just not running")
	require.Contains(t, info.Detail, "no daemon process is running")
}

func TestLaunchdJobRunning_ReadsState(t *testing.T) {
	// A live job: launchd prints a pid only for a process it is running.
	require.True(t, launchdJobRunning("\tstate = running\n\tpid = 4321\n"))
	require.True(t, launchdJobRunning("\tpid = 4321\n"))
	require.True(t, launchdJobRunning("\tstate = running\n"))

	// Loaded but stopped. "not running" contains "running", which is why this
	// compares the whole value instead of searching for a substring.
	require.False(t, launchdJobRunning("\tstate = not running\n\tlast exit code = 1\n"))
	require.False(t, launchdJobRunning("\tactive count = 0\n"))
	require.False(t, launchdJobRunning(""))
	require.False(t, launchdJobRunning("\tpid = \n"), "an empty pid is not a pid")
}

func TestAutostartSupervision_DarwinLoadedInGUIDomain(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))

	withProbeCommand(t, func(_ string, args ...string) ([]byte, error) {
		if args[0] == "print" {
			require.Equal(t, launchdDomainTarget(), args[1],
				"doctor must probe the same domain the restart path kickstarts")
			return []byte("\tstate = running\n\tpid = 4321\n"), nil
		}
		return nil, errFake
	})

	info := AutostartSupervision()
	require.Equal(t, ProbeYes, info.Loaded)
	require.Equal(t, ProbeYes, info.Active)
	require.Equal(t, ProbeNo, info.LoadedElsewhere)
}

func TestAutostartSupervision_DarwinNotLoaded(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))

	// launchd's real "no such service" answer, not a generic failure: a generic
	// failure means we could not ask, which is a different fact entirely.
	withProbeCommand(t, func(string, ...string) ([]byte, error) {
		return []byte("Could not find service"), exitErr(t, launchdNotFoundExit)
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.NoError(t, info.ProbeErr)
	require.Equal(t, ProbeNo, info.Active)
	require.Equal(t, ProbeNo, info.LoadedElsewhere)
	require.Equal(t, "not loaded", info.Detail)
}

// No unit file means nothing to supervise; the service manager must not even
// be consulted.
func TestAutostartSupervision_NoUnitSkipsProbe(t *testing.T) {
	withAutostartTestEnv(t, "linux")
	withProbeCommand(t, func(string, ...string) ([]byte, error) {
		t.Fatal("must not probe the service manager when no unit is installed")
		return nil, nil
	})

	info := AutostartSupervision()
	require.False(t, info.UnitPresent)
}

// withProbeTimeout shortens the probe deadline for one test.
func withProbeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := autostartProbeTimeout
	t.Cleanup(func() { autostartProbeTimeout = prev })
	autostartProbeTimeout = d
}

// The probe must be really bounded, not decoratively. This drives the PRODUCTION
// runner against #1967's exact shape: a command that writes output and then
// leaves a child holding the pipe open. CommandContext alone kills the direct
// child and then CombinedOutput blocks on pipe EOF anyway — measured at "did not
// return at all" in #1967 — so only WaitDelay makes the deadline real.
//
// `af doctor` is the command a user runs BECAUSE things are wedged; it must not
// be the thing that hangs.
func TestAutostartProbe_HangIsBoundedAndReportsTimeout(t *testing.T) {
	withProbeTimeout(t, 300*time.Millisecond)

	start := time.Now()
	_, err := runAutostartProbeCommand("sh", "-c", "echo hi; sleep 30")
	elapsed := time.Since(start)

	require.Error(t, err, "a killed probe is not a successful answer")
	require.Contains(t, err.Error(), "timed out")
	require.Less(t, elapsed, 10*time.Second,
		"the deadline must bound the call; without WaitDelay the held pipe defeats it")
}

// A timeout is an inability to ask. It must surface as UNKNOWN with its cause —
// never as "inactive", which would tell the user their autostart is off when we
// have no idea.
func TestAutostartSupervision_ProbeTimeout_IsUnknownNotInactive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeTimeout(t, 200*time.Millisecond)
	// The real runner against a real hang, so the whole path is exercised.
	withProbeCommand(t, runAutostartProbeCommand)

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.Error(t, info.ProbeErr, "a probe that never answered must report why")
	require.Equal(t, ProbeUnknown, info.Active, "a timeout is not an answer of 'inactive'")
	require.Equal(t, ProbeUnknown, info.Enabled)
}

// The service manager binary missing is an inability to ask, not an answer.
func TestAutostartSupervision_ProbeBinaryMissing_IsUnknownNotInactive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(string, ...string) ([]byte, error) {
		return nil, exec.ErrNotFound // no systemctl on this box
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.Error(t, info.ProbeErr)
	require.Contains(t, info.ProbeErr.Error(), "could not query systemd")
	require.Equal(t, ProbeUnknown, info.Active, "no systemctl is not 'inactive'")
	require.Equal(t, ProbeUnknown, info.Enabled)
}

// A user bus that is down exits non-zero with a diagnostic, not a state word.
// The exit code alone cannot tell it from "inactive" — the output must.
func TestAutostartSupervision_BusUnavailable_IsUnknownNotInactive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(string, ...string) ([]byte, error) {
		return []byte("Failed to connect to bus: No medium found"), exitErr(t, 1)
	})

	info := AutostartSupervision()
	require.Error(t, info.ProbeErr)
	require.Equal(t, ProbeUnknown, info.Active,
		"a non-zero exit whose output is not a state word means the question never reached systemd")
	require.Contains(t, info.Detail, "unknown")
}

// The other half: a genuine "inactive" still reads as a real answer. Making the
// probe honest must not make it blind.
func TestAutostartSupervision_GenuineInactive_IsAnswered(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(_ string, args ...string) ([]byte, error) {
		if args[1] == "is-enabled" {
			return []byte("enabled\n"), nil
		}
		return []byte("inactive\n"), exitErr(t, 3) // systemctl's real inactive exit
	})

	info := AutostartSupervision()
	require.NoError(t, info.ProbeErr, "systemd answered; there is nothing unknown here")
	require.Equal(t, ProbeYes, info.Enabled)
	require.Equal(t, ProbeNo, info.Active)
}

// DARWIN gets the same treatment: launchctl missing or wedged is unknown, never
// "not loaded". macOS is where the skew bug actually bites (#1947), so a
// Linux-only honest probe would be exactly the wrong half.
func TestAutostartSupervision_DarwinProbeBinaryMissing_IsUnknownNotNotLoaded(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))
	withProbeCommand(t, func(string, ...string) ([]byte, error) {
		return nil, exec.ErrNotFound // no launchctl
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.Error(t, info.ProbeErr)
	require.Contains(t, info.ProbeErr.Error(), "could not query launchd")
	require.Equal(t, ProbeUnknown, info.Loaded, "no launchctl is not 'not loaded'")
	require.Equal(t, ProbeUnknown, info.LoadedElsewhere)
	require.Equal(t, ProbeUnknown, info.Active)
}

// A launchctl failure that is NOT a not-found answer (permission denied, bad
// request) is also unknown — only a recognized not-found counts as an answer.
func TestAutostartSupervision_DarwinProbeDenied_IsUnknownNotNotLoaded(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))
	withProbeCommand(t, func(string, ...string) ([]byte, error) {
		return []byte("Operation not permitted"), exitErr(t, 1)
	})

	info := AutostartSupervision()
	require.Error(t, info.ProbeErr)
	require.Equal(t, ProbeUnknown, info.Loaded)
	require.Equal(t, ProbeUnknown, info.Active)
}

// And the darwin answer path still works: a real not-found is a real answer.
func TestAutostartSupervision_DarwinGenuineNotLoaded_IsAnswered(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))
	withProbeCommand(t, func(_ string, args ...string) ([]byte, error) {
		return []byte("Could not find service \"" + autostartLaunchdLabel + "\" in domain"), exitErr(t, launchdNotFoundExit)
	})

	info := AutostartSupervision()
	require.NoError(t, info.ProbeErr, "launchd answered; nothing is unknown")
	require.Equal(t, ProbeNo, info.Loaded)
	require.Equal(t, ProbeNo, info.LoadedElsewhere)
	require.Equal(t, ProbeNo, info.Active)
	require.Equal(t, "not loaded", info.Detail)
}
