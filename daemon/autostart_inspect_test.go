package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// errFake stands in for a non-zero exit from systemctl/launchctl.
var errFake = errors.New("fake unit command failure")

// withUnitCommand replaces the external service-manager runner for one test, so
// nothing here can shell out to the real systemctl/launchctl.
func withUnitCommand(t *testing.T, fn func(string, ...string) ([]byte, error)) {
	t.Helper()
	prev := autostartUnitCommand
	t.Cleanup(func() { autostartUnitCommand = prev })
	autostartUnitCommand = fn
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

func TestInspectAutostart_UnsupportedPlatform(t *testing.T) {
	withAutostartTestEnv(t, "plan9")
	info := InspectAutostart()
	require.False(t, info.Supported)
}

func TestAutostartSupervision_LinuxEnabledActive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))

	withUnitCommand(t, func(name string, args ...string) ([]byte, error) {
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
	require.True(t, info.Enabled)
	require.True(t, info.Active)
}

func TestAutostartSupervision_LinuxInactive(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	writeUnitFile(t, filepath.Join(dir, autostartUnitName), systemdAutostartUnit("/usr/local/bin/af", "", "", ""))

	withUnitCommand(t, func(_ string, args ...string) ([]byte, error) {
		if args[1] == "is-enabled" {
			return []byte("enabled\n"), nil
		}
		return []byte("inactive\n"), errFake // systemctl exits non-zero for inactive
	})

	info := AutostartSupervision()
	require.True(t, info.Enabled)
	require.False(t, info.Active)
	require.Contains(t, info.Detail, "inactive")
}

// The macOS domain mismatch: `launchctl print gui/<uid>/…` misses the agent,
// but the domain-agnostic `launchctl list` finds it — so it is loaded
// somewhere the restart path cannot reach.
func TestAutostartSupervision_DarwinLoadedOutsideGUIDomain(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))

	withUnitCommand(t, func(_ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "print":
			return nil, errFake // not in gui/<uid>
		case "list":
			return []byte("12345\t0\t" + autostartLaunchdLabel + "\n"), nil
		}
		return nil, errFake
	})

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.True(t, info.LoadedElsewhere)
	require.False(t, info.Active)
	require.Equal(t, launchdDomainTarget(), info.Domain)
}

func TestAutostartSupervision_DarwinLoadedInGUIDomain(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))

	withUnitCommand(t, func(_ string, args ...string) ([]byte, error) {
		if args[0] == "print" {
			require.Equal(t, launchdDomainTarget(), args[1],
				"doctor must probe the same domain the restart path kickstarts")
			return []byte("state = running\n"), nil
		}
		return nil, errFake
	})

	info := AutostartSupervision()
	require.True(t, info.Active)
	require.False(t, info.LoadedElsewhere)
}

func TestAutostartSupervision_DarwinNotLoaded(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	writeUnitFile(t, filepath.Join(dir, autostartLaunchdLabel+".plist"),
		launchdAutostartPlist("/usr/local/bin/af", "", "", "", "/tmp/af.log"))

	withUnitCommand(t, func(string, ...string) ([]byte, error) { return nil, errFake })

	info := AutostartSupervision()
	require.True(t, info.UnitPresent)
	require.False(t, info.Active)
	require.False(t, info.LoadedElsewhere)
	require.Equal(t, "not loaded", info.Detail)
}

// No unit file means nothing to supervise; the service manager must not even
// be consulted.
func TestAutostartSupervision_NoUnitSkipsProbe(t *testing.T) {
	withAutostartTestEnv(t, "linux")
	withUnitCommand(t, func(string, ...string) ([]byte, error) {
		t.Fatal("must not probe the service manager when no unit is installed")
		return nil, nil
	})

	info := AutostartSupervision()
	require.False(t, info.UnitPresent)
}
