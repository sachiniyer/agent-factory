package daemon

import (
	"errors"
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
func withProbeCommand(t *testing.T, fn func(string, ...string) probeResult) {
	t.Helper()
	prev := autostartProbeCommand
	t.Cleanup(func() { autostartProbeCommand = prev })
	autostartProbeCommand = fn
}

// answered builds the result of a service-manager command that RAN to
// completion and printed out, exiting code.
func answered(out string, code int) probeResult {
	return probeResult{completed: true, output: out, exitCode: code}
}

// neverAnswered builds the result of a command that did not complete — the
// state whose output is deliberately unreachable.
func neverAnswered(cause error) probeResult {
	return probeResult{cause: cause}
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
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		probed = true
		if args[1] == "is-enabled" {
			return answered("enabled\n", 0)
		}
		return answered("inactive\n", 3)
	})

	info := AutostartSupervision()
	require.True(t, probed, "an unreadable unit file must not skip the service-manager probe")
	require.True(t, info.UnitPresent)
	require.Error(t, info.Err, "the read failure rides along for the caller to report")
	requireAnswer(t, "no", info.Active)
}

func TestInspectAutostart_UnsupportedPlatform(t *testing.T) {
	withAutostartTestEnv(t, "plan9")
	info := InspectAutostart()
	require.False(t, info.Supported)
}

func TestAutostartSupervision_LinuxEnabledActive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))

	withProbeCommand(t, func(name string, args ...string) probeResult {
		require.Equal(t, "systemctl", name)
		switch args[1] {
		case "is-enabled":
			return answered("enabled\n", 0)
		case "is-active":
			return answered("active\n", 0)
		}
		return neverAnswered(errFake)
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	requireAnswer(t, "yes", info.Enabled)
	requireAnswer(t, "yes", info.Active)
}

func TestAutostartSupervision_LinuxInactive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))

	withProbeCommand(t, func(_ string, args ...string) probeResult {
		if args[1] == "is-enabled" {
			return answered("enabled\n", 0)
		}
		return answered("inactive\n", 3) // systemctl exits 3 for inactive
	})

	info := AutostartSupervision()
	requireAnswer(t, "yes", info.Enabled)
	requireAnswer(t, "no", info.Active)
	require.Contains(t, info.Detail, "inactive")
}

// The macOS domain mismatch: `launchctl print gui/<uid>/…` misses the agent,
// but the domain-agnostic `launchctl list` finds it — so it is loaded
// somewhere the restart path cannot reach.
func TestAutostartSupervision_DarwinLoadedOutsideGUIDomain(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))

	withProbeCommand(t, func(_ string, args ...string) probeResult {
		switch args[0] {
		case "print-disabled":
			return answered("disabled services = {\n}\n", 0)
		case "print":
			// launchd answered: not in gui/<uid>.
			return answered("Could not find service", launchdNotFoundExit)
		case "list":
			return answered("12345\t0\t"+autostartLaunchdLabel+"\n", 0)
		}
		return neverAnswered(errFake)
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.NoError(t, info.LoadedElsewhere.Cause(), "launchd answered both questions")
	requireAnswer(t, "yes", info.LoadedElsewhere)
	require.Equal(t, launchdDomainTarget(), info.Domain)
	// Whether that other domain is actually RUNNING it is not something
	// `launchctl list` establishes — it reports that the label is known. The
	// finding is the domain mismatch; claiming a liveness we never observed
	// would be the same overreach this file exists to remove.
	requireAnswer(t, "unknown", info.Active)
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
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		if args[0] == "print-disabled" {
			return answered("disabled services = {\n}\n", 0)
		}
		if args[0] == "print" {
			return answered("com.agent-factory.daemon = {\n"+
				"\tactive count = 0\n"+
				"\tstate = not running\n"+
				"\tlast exit code = 1\n"+
				"}\n", 0)
		}
		return neverAnswered(errFake)
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	requireAnswer(t, "yes", info.Loaded, "launchd knows the job")
	requireAnswer(t, "no", info.Active, "but no daemon process is running")
	requireAnswer(t, "no", info.LoadedElsewhere, "it is loaded in the right domain, just not running")
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

	withProbeCommand(t, func(_ string, args ...string) probeResult {
		if args[0] == "print-disabled" {
			return answered("disabled services = {\n}\n", 0)
		}
		if args[0] == "print" {
			require.Equal(t, launchdDomainTarget(), args[1],
				"doctor must probe the same domain the restart path kickstarts")
			return answered("\tstate = running\n\tpid = 4321\n", 0)
		}
		return neverAnswered(errFake)
	})

	info := AutostartSupervision()
	requireAnswer(t, "yes", info.Loaded)
	requireAnswer(t, "yes", info.Active)
	requireAnswer(t, "no", info.LoadedElsewhere)
}

func TestAutostartSupervision_DarwinNotLoaded(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))

	// launchd's real "no such service" answer, not a generic failure: a generic
	// failure means we could not ask, which is a different fact entirely.
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		if args[0] == "print-disabled" {
			return answered("disabled services = {\n}\n", 0)
		}
		return answered("Could not find service", launchdNotFoundExit)
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.NoError(t, info.Active.Cause())
	requireAnswer(t, "no", info.Active)
	requireAnswer(t, "no", info.LoadedElsewhere)
	require.Equal(t, "not loaded", info.Detail)
}

// No unit file means nothing to supervise; the service manager must not even
// be consulted.
func TestAutostartSupervision_NoUnitSkipsProbe(t *testing.T) {
	withAutostartTestEnv(t, "linux")
	withProbeCommand(t, func(string, ...string) probeResult {
		t.Fatal("must not probe the service manager when no unit is installed")
		return probeResult{}
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
	res := runAutostartProbeCommand("sh", "-c", "echo hi; sleep 30")
	elapsed := time.Since(start)

	require.Error(t, res.Cause(), "a killed probe is not a successful answer")
	require.Contains(t, res.Cause().Error(), "timed out")
	out, ok := res.Output()
	require.False(t, ok, "a probe that never completed must expose no output to classify")
	require.Empty(t, out, "and nothing that could be mistaken for a state word")
	require.Less(t, elapsed, 10*time.Second,
		"the deadline must bound the call; without WaitDelay the held pipe defeats it")
}

// withBlockingServiceManager puts a FAKE systemctl on PATH that never returns,
// so the probe's deadline is the only thing that can end the call.
//
// A fake binary, not a fake function: the timeout path lives in the real runner
// (fork, deadline, kill, pipe), and a stub that returns an error exercises none
// of it.
func withBlockingServiceManager(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	// Prints first, THEN blocks — and blocks in a CHILD that inherits stdout, so
	// the output pipe stays open after the direct child is killed. That is
	// #1967's shape: without WaitDelay the deadline kills systemctl and the call
	// still waits on the pipe.
	script := "#!/bin/sh\necho starting\nsleep 300 &\nsleep 300\n"
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A probe that HANGS must come back UNKNOWN with its cause, promptly — never
// "inactive", and never not at all.
//
// This drives a real blocking service-manager binary through the real runner, so
// the timeout path is genuinely executed. An earlier version of this test called
// the real runner against a `systemctl` that does not exist in the container: it
// returned instantly via ErrNotFound and passed on the missing-binary path while
// claiming to test the timeout — green whether or not the bound worked, which is
// this PR's own disease wearing a test's clothes.
func TestAutostartSupervision_ProbeHangs_IsUnknownNotInactive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withBlockingServiceManager(t, "systemctl")
	withProbeTimeout(t, 300*time.Millisecond)
	withProbeCommand(t, runAutostartProbeCommand) // the real, bounded runner

	done := make(chan SupervisionInfo, 1)
	start := time.Now()
	go func() { done <- AutostartSupervision() }()

	var info SupervisionInfo
	select {
	case info = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("af doctor's autostart probe never returned: the deadline does not bound it")
	}
	elapsed := time.Since(start)

	require.Less(t, elapsed, 15*time.Second,
		"the probe must be bounded by its deadline, not by the child exiting")
	require.True(t, info.UnitPresent)
	require.Error(t, info.Active.Cause(), "a probe that never answered must say why")
	require.Contains(t, info.Active.Cause().Error(), "timed out", "and the cause must name the timeout")
	requireAnswer(t, "unknown", info.Active, "a hang is not an answer of 'inactive'")
	requireAnswer(t, "unknown", info.Enabled)
}

// The same, on darwin: a wedged launchctl is unknown, never "not loaded".
func TestAutostartSupervision_DarwinProbeHangs_IsUnknownNotNotLoaded(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))
	withBlockingServiceManager(t, "launchctl")
	withProbeTimeout(t, 300*time.Millisecond)
	withProbeCommand(t, runAutostartProbeCommand)

	done := make(chan SupervisionInfo, 1)
	go func() { done <- AutostartSupervision() }()

	var info SupervisionInfo
	select {
	case info = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the launchd probe never returned: the deadline does not bound it")
	}

	require.Error(t, info.Active.Cause())
	require.Contains(t, info.Loaded.Cause().Error(), "timed out")
	requireAnswer(t, "unknown", info.Loaded, "a hang is not an answer of 'not loaded'")
	requireAnswer(t, "unknown", info.Active)
}

// The service manager binary missing is an inability to ask, not an answer.
func TestAutostartSupervision_ProbeBinaryMissing_IsUnknownNotInactive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(string, ...string) probeResult {
		return neverAnswered(exec.ErrNotFound) // no systemctl on this box
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.Error(t, info.Active.Cause())
	require.Contains(t, info.Active.Cause().Error(), "could not query systemd")
	requireAnswer(t, "unknown", info.Active, "no systemctl is not 'inactive'")
	requireAnswer(t, "unknown", info.Enabled)
}

// A user bus that is down exits non-zero with a diagnostic, not a state word.
// The exit code alone cannot tell it from "inactive" — the output must.
func TestAutostartSupervision_BusUnavailable_IsUnknownNotInactive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(string, ...string) probeResult {
		return answered("Failed to connect to bus: No medium found", 1)
	})

	info := AutostartSupervision()
	require.Error(t, info.Active.Cause())
	requireAnswer(t, "unknown", info.Active,
		"a non-zero exit whose output is not a state word means the question never reached systemd")
	require.Contains(t, info.Detail, "unknown")
}

// The other half: a genuine "inactive" still reads as a real answer. Making the
// probe honest must not make it blind.
func TestAutostartSupervision_GenuineInactive_IsAnswered(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		if args[1] == "is-enabled" {
			return answered("enabled\n", 0)
		}
		return answered("inactive\n", 3) // systemctl's real inactive exit
	})

	info := AutostartSupervision()
	require.NoError(t, info.Active.Cause(), "systemd answered; there is nothing unknown here")
	requireAnswer(t, "yes", info.Enabled)
	requireAnswer(t, "no", info.Active)
}

// DARWIN gets the same treatment: launchctl missing or wedged is unknown, never
// "not loaded". macOS is where the skew bug actually bites (#1947), so a
// Linux-only honest probe would be exactly the wrong half.
func TestAutostartSupervision_DarwinProbeBinaryMissing_IsUnknownNotNotLoaded(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))
	withProbeCommand(t, func(string, ...string) probeResult {
		return neverAnswered(exec.ErrNotFound) // no launchctl
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.Error(t, info.Active.Cause())
	require.Contains(t, info.Loaded.Cause().Error(), "could not query launchd")
	requireAnswer(t, "unknown", info.Loaded, "no launchctl is not 'not loaded'")
	requireAnswer(t, "unknown", info.LoadedElsewhere)
	requireAnswer(t, "unknown", info.Active)
}

// A launchctl failure that is NOT a not-found answer (permission denied, bad
// request) is also unknown — only a recognized not-found counts as an answer.
func TestAutostartSupervision_DarwinProbeDenied_IsUnknownNotNotLoaded(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		if args[0] == "print-disabled" {
			return answered("disabled services = {\n}\n", 0)
		}
		return answered("Operation not permitted", 1)
	})

	info := AutostartSupervision()
	require.Error(t, info.Active.Cause())
	requireAnswer(t, "unknown", info.Loaded)
	requireAnswer(t, "unknown", info.Active)
}

// And the darwin answer path still works: a real not-found is a real answer.
func TestAutostartSupervision_DarwinGenuineNotLoaded_IsAnswered(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		return answered("Could not find service \""+autostartLaunchdLabel+"\" in domain", launchdNotFoundExit)
	})

	info := AutostartSupervision()
	require.NoError(t, info.Loaded.Cause(), "launchd answered; nothing is unknown")
	requireAnswer(t, "no", info.Loaded)
	requireAnswer(t, "no", info.LoadedElsewhere)
	requireAnswer(t, "no", info.Active)
	require.Equal(t, "not loaded", info.Detail)
}

// A probe whose command finished but whose output pipe was held open by a
// straggler must still be an ANSWER, not a failure.
//
// This is the bug's mirror image, and it bit my own bounded runner: the shell
// exits 0 having printed "active", a backgrounded child keeps stdout open past
// WaitDelay, and Go reports exec.ErrWaitDelay. The callers read "error + a state
// word" as "the manager answered no" — so a perfectly healthy unit would come
// back inactive. The command's work is done; only a straggler lingered, and the
// reap kills it (#676/#914 normalizeWaitDelay precedent).
func TestAutostartProbe_WaitDelayStragglerIsStillAnAnswer(t *testing.T) {
	// The deadline must NOT fire here: this is about the pipe, not the timeout.
	withProbeTimeout(t, 30*time.Second)

	// sh exits immediately; the backgrounded sleep inherits stdout and holds it.
	res := runAutostartProbeCommand("sh", "-c", "echo active; sleep 30 &")

	require.NoError(t, res.Cause(), "the command answered; a lingering pipe-holder is not a failure")
	out, ok := res.Output()
	require.True(t, ok, "a completed command has output to classify")
	require.Contains(t, out, "active", "and its answer must survive")
}

// The same shape, all the way through the classifier: a straggler must not turn
// "active" into "inactive".
func TestAutostartSupervision_WaitDelayStragglerDoesNotInvertTheAnswer(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		// What CombinedOutput returns when the answer arrived but a child held
		// the pipe past WaitDelay — after runAutostartProbeCommand normalizes it.
		if args[1] == "is-enabled" {
			return answered("enabled\n", 0)
		}
		return answered("active\n", 0)
	})

	info := AutostartSupervision()
	require.NoError(t, info.Active.Cause())
	requireAnswer(t, "yes", info.Active, "a straggler must never invert a healthy answer")
}

// requireAnswer asserts an answer's outcome by name, which keeps the tests
// readable while ProbeAnswer stays unconstructible from outside.
func requireAnswer(t *testing.T, want string, got ProbeAnswer, msg ...any) {
	t.Helper()
	require.Equal(t, want, got.String(), msg...)
}

// systemd's not-found is a DEFINITE answer and must survive as its own outcome.
//
// It is the most actionable state of the four: the unit FILE is installed (we
// only probe when it is), so systemd not knowing it means it was never loaded —
// `systemctl --user daemon-reload` fixes it. A two-valued probe threw this away,
// calling it "inactive" (sending users to reinstall something already there) or
// "unknown" (telling them nothing).
func TestAutostartSupervision_SystemdNotFound_IsItsOwnAnswer(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		// systemd's real shape for a unit it has no record of.
		if args[1] == "is-enabled" {
			return answered("not-found\n", systemdNoSuchUnitExit)
		}
		return answered("unknown\n", systemdNoSuchUnitExit)
	})

	info := AutostartSupervision()
	require.NoError(t, info.Enabled.Cause(), "systemd answered; nothing is unknown")
	requireAnswer(t, "not-found", info.Enabled, "not-found is not 'disabled'")
	requireAnswer(t, "not-found", info.Active, "and not 'inactive'")
}

// The exit code alone is enough: systemctl exits 4 for "no such unit".
func TestAutostartSupervision_SystemdNotFoundByExitCode(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(string, ...string) probeResult {
		return answered("Failed to get unit file state: No such file or directory\n", systemdNoSuchUnitExit)
	})

	info := AutostartSupervision()
	requireAnswer(t, "not-found", info.Enabled)
	require.NoError(t, info.Enabled.Cause())
}

// THE ROUND-3 BUG, pinned: a probe that TIMED OUT must not become a definite
// negative, even when what it managed to print before dying is a real state word.
//
// The old classifier took (output, error) and asked "is the output a state
// word?" — so a wedged systemctl that printed "inactive" and then hung produced
// a confident No. It cannot happen now by construction: an incomplete probe
// exposes no output at all, so there is nothing to match against.
func TestAutostartSupervision_TimeoutWithStateWordOutput_IsStillUnknown(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(string, ...string) probeResult {
		// The nastiest shape: it printed a perfectly good state word, and then
		// never completed.
		return probeResult{cause: errors.New("systemctl is-active timed out after 5s: context deadline exceeded")}
	})

	info := AutostartSupervision()
	requireAnswer(t, "unknown", info.Enabled, "a timed-out probe is not an answer, whatever it printed")
	require.Error(t, info.Enabled.Cause())
	require.Contains(t, info.Enabled.Cause().Error(), "timed out")
}

// The structural guarantee, asserted directly: a probe that did not complete
// carries no output, so no classifier anywhere can mint a state from it.
func TestProbeResult_IncompleteCarriesNoOutput(t *testing.T) {
	res := probeResult{cause: errors.New("timed out")}
	out, ok := res.Output()
	require.False(t, ok, "an incomplete probe must not present output as classifiable")
	require.Empty(t, out, "and must not leak the partial text a classifier could match")
	require.False(t, res.Succeeded())
	require.Error(t, res.Cause())
}

// The zero value is the honest one: a field nobody probed reads as unknown.
func TestProbeAnswer_ZeroValueIsUndetermined(t *testing.T) {
	var a ProbeAnswer
	require.Equal(t, "unknown", a.String(), "an unset answer must never read as a negative")

	matched := ""
	a.Match(
		func() { matched = "yes" },
		func() { matched = "no" },
		func() { matched = "not-found" },
		func(error) { matched = "undetermined" },
	)
	require.Equal(t, "undetermined", matched, "Match must route the zero value to the undetermined branch")
}

// Undetermined always carries a cause: "unknown" with no reason is not
// reportable, so a nil cause is backfilled rather than silently dropped.
func TestProbeAnswer_UndeterminedAlwaysHasACause(t *testing.T) {
	require.Error(t, Undetermined(nil).Cause())
	require.Equal(t, "unknown", Undetermined(nil).String())
}

// EXIT 0 IS NOT AN ANSWER. `systemctl is-enabled` exits 0 for six words that do
// NOT mean "this starts at login", and reading the exit code instead of the word
// reported broken autostart as healthy.
//
// This is the opposite polarity to the timeout bugs, and worse for this feature:
// a false negative sends someone to fix a working system, but a false POSITIVE
// leaves their daemon dead after the next reboot with doctor having said
// everything was fine — the exact failure #1920 exists to prevent.
func TestSystemdIsEnabled_ExitZeroWordsThatAreNotEnabled(t *testing.T) {
	// Every word systemctl(1) documents as exiting 0 for is-enabled, and what it
	// means for AF's question.
	for _, tc := range []struct {
		word string
		want string
	}{
		{"enabled", "yes"},        // the only one that means what we ask
		{"enabled-runtime", "no"}, // enabled in /run only — gone after a reboot
		{"static", "no"},          // no [Install]; cannot be enabled
		{"indirect", "no"},        // only triggered by another unit
		{"generated", "no"},       // generator-made, not a persistent install
		{"transient", "no"},       // runtime unit; will not survive a reboot
		{"alias", "unknown"},      // describes the NAME, not our question
	} {
		t.Run(tc.word, func(t *testing.T) {
			dir := withAutostartTestEnv(t, "linux")
			writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
			withProbeCommand(t, func(_ string, args ...string) probeResult {
				if args[1] == systemdIsEnabled {
					return answered(tc.word+"\n", 0) // EXIT 0, every time
				}
				return answered("active\n", 0)
			})

			info := AutostartSupervision()
			requireAnswer(t, tc.want, info.Enabled,
				"is-enabled=%s exits 0; only the WORD says what it means", tc.word)
			require.Contains(t, info.Detail, tc.word,
				"the manager's own word must reach the report")
		})
	}
}

// The non-zero-exit is-enabled words are definite answers too, not errors.
func TestSystemdIsEnabled_NonZeroExitWordsAreDefinite(t *testing.T) {
	for _, word := range []string{"disabled", "masked", "masked-runtime", "linked", "linked-runtime", "bad"} {
		t.Run(word, func(t *testing.T) {
			dir := withAutostartTestEnv(t, "linux")
			writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
			withProbeCommand(t, func(_ string, args ...string) probeResult {
				if args[1] == systemdIsEnabled {
					return answered(word+"\n", 1)
				}
				return answered("active\n", 0)
			})

			info := AutostartSupervision()
			requireAnswer(t, "no", info.Enabled, "%s is a definite no, not an error", word)
			require.NoError(t, info.Enabled.Cause())
		})
	}
}

// is-active's vocabulary is disjoint from is-enabled's, and "reloading" means
// RUNNING — it was in the old flat negative table, which only worked by accident
// because exit 0 short-circuited to Yes.
func TestSystemdIsActive_WordsPerVerb(t *testing.T) {
	for _, tc := range []struct {
		word string
		exit int
		want string
	}{
		{"active", 0, "yes"},
		{"reloading", 0, "yes"}, // running, re-reading config
		{"inactive", 3, "no"},
		{"failed", 3, "no"},
		{"activating", 3, "no"}, // not up YET is not up
		{"deactivating", 3, "no"},
		{"maintenance", 3, "no"},
		{"unknown", 4, "not-found"},
	} {
		t.Run(tc.word, func(t *testing.T) {
			dir := withAutostartTestEnv(t, "linux")
			writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
			withProbeCommand(t, func(_ string, args ...string) probeResult {
				if args[1] == systemdIsEnabled {
					return answered("enabled\n", 0)
				}
				return answered(tc.word+"\n", tc.exit)
			})

			info := AutostartSupervision()
			requireAnswer(t, tc.want, info.Active, "is-active=%s", tc.word)
		})
	}
}

// A word systemd never documents for this verb is not an answer in EITHER
// direction — guessing yes is how the false positive happened.
func TestSystemdAsk_UnrecognizedWordOnExitZeroIsUndetermined(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	withProbeCommand(t, func(string, ...string) probeResult {
		return answered("something-systemd-invented-in-2027\n", 0) // exit 0!
	})

	info := AutostartSupervision()
	requireAnswer(t, "unknown", info.Enabled, "exit 0 with a word we do not know is not a yes")
	require.Error(t, info.Enabled.Cause())
}

// An "alias" answer must NOT suppress the is-active probe: systemd is perfectly
// reachable, only our enablement question is unanswerable. Only an UNREACHABLE
// manager justifies skipping the second query.
func TestSystemdAsk_AliasStillProbesIsActive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))
	asked := map[string]bool{}
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		asked[args[1]] = true
		if args[1] == systemdIsEnabled {
			return answered("alias\n", 0)
		}
		return answered("active\n", 0)
	})

	info := AutostartSupervision()
	require.True(t, asked[systemdIsActive],
		"systemd answered; an unmappable answer is not a reason to stop asking it things")
	requireAnswer(t, "unknown", info.Enabled)
	requireAnswer(t, "yes", info.Active, "and the answer we CAN have must be kept")
}

// A PLIST ON DISK IS NOT "ENABLED". launchd's own state is the answer; the file
// only says what WOULD run.
//
// This is the darwin half of "exit 0 is transport, the word is the answer" — and
// it is the half that matters most, because #1947 was reported on macOS. A mac
// user with a `launchctl disable`d agent would have been told autostart was
// healthy and found their daemon gone after the next reboot.
func TestLaunchdEnabled_DisabledAgentIsNotEnabled(t *testing.T) {
	withAutostartTestEnv(t, "darwin")
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		require.Equal(t, "print-disabled", args[0])
		require.Equal(t, launchdUserDomain(), args[1],
			"must ask the SAME domain af's restarts target, or the answer is about nothing")
		return answered("disabled services = {\n\t\""+autostartLaunchdLabel+"\" => true\n}\n", 0)
	})

	requireAnswer(t, "no", launchdEnabled(), "launchd holds a disable override; the plist does not overrule it")
}

func TestLaunchdEnabled_ExplicitlyEnabled(t *testing.T) {
	withAutostartTestEnv(t, "darwin")
	withProbeCommand(t, func(string, ...string) probeResult {
		return answered("disabled services = {\n\t\""+autostartLaunchdLabel+"\" => false\n}\n", 0)
	})
	requireAnswer(t, "yes", launchdEnabled())
}

// Absent from the disabled store means no override, which is launchd's
// documented default — the plist's RunAtLoad governs.
func TestLaunchdEnabled_NotOverriddenIsEnabled(t *testing.T) {
	withAutostartTestEnv(t, "darwin")
	withProbeCommand(t, func(string, ...string) probeResult {
		return answered("disabled services = {\n\t\"com.someone.else\" => true\n}\n", 0)
	})
	requireAnswer(t, "yes", launchdEnabled())
}

// launchd that cannot be asked is Undetermined — never "enabled".
func TestLaunchdEnabled_ProbeFailsIsUndeterminedNotEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  probeResult
	}{
		{"binary missing", neverAnswered(exec.ErrNotFound)},
		{"timed out", neverAnswered(errors.New("launchctl print-disabled timed out after 5s"))},
		{"permission denied", answered("Operation not permitted", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withAutostartTestEnv(t, "darwin")
			withProbeCommand(t, func(string, ...string) probeResult { return tc.res })

			got := launchdEnabled()
			requireAnswer(t, "unknown", got, "an unaskable launchd is not an enabled one")
			require.Error(t, got.Cause(), "and it must say why")
		})
	}
}

// End to end: a disabled agent must not report supervision as enabled.
func TestAutostartSupervision_DarwinDisabledAgent_IsNotEnabled(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))
	withProbeCommand(t, func(_ string, args ...string) probeResult {
		switch args[0] {
		case "print-disabled":
			return answered("disabled services = {\n\t\""+autostartLaunchdLabel+"\" => true\n}\n", 0)
		case "print":
			return answered("\tstate = running\n\tpid = 4321\n", 0)
		}
		return neverAnswered(errFake)
	})

	info := AutostartSupervision()
	requireAnswer(t, "no", info.Enabled, "the plist exists and it is running — but it is DISABLED")
	requireAnswer(t, "yes", info.Active, "running now says nothing about starting at login")
}

// `--daemon=false` is a client saying it is NOT a daemon. Counting it as one
// makes every caller's answer about the wrong population — doctor's duplicate
// scan, the host-wide kill scan, and the #1004 pid guard alike.
func TestLooksLikeDaemonArgv_DaemonFalseIsNotADaemon(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"af", "--daemon"}, true},
		{[]string{"af", "--daemon=true"}, true},
		{[]string{"af", "--daemon=1"}, true},
		{[]string{"af", "--daemon=t"}, true},
		{[]string{"af", "--daemon=false"}, false},
		{[]string{"af", "--daemon=0"}, false},
		{[]string{"af", "--daemon=f"}, false},
		{[]string{"af", "--daemon=FALSE"}, false},
		// An unparseable value keeps the pre-existing "the form is present"
		// reading (TestArgsHaveDaemonFlag pins it since #342); cobra rejects a
		// non-boolean, so no such process is ever live to classify.
		{[]string{"af", "--daemon=maybe"}, true},
		{[]string{"af", "sessions", "list"}, false},
	} {
		require.Equal(t, tc.want, LooksLikeDaemonArgv(tc.args), "argv %v", tc.args)
	}
}
