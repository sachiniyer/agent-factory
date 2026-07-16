package doctor

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

// The skew suite (#1044) never touches a real daemon, unit, or install: the
// daemon that "answers" is a struct literal, the duplicate daemons are the
// test's own shells wearing an af cmdline (snapshot-filtered to them), the
// autostart unit is a fake reader, and the af binaries being compared are a
// map. The one real file it creates is a Unix socket in a temp home, which it
// listens on itself.

// findCheck returns the check row with the given name.
func findCheck(t *testing.T, r *Report, name string) CheckResult {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in report (have: %s)", name, strings.Join(checkNames(r), ", "))
	return CheckResult{}
}

func checkNames(r *Report) []string {
	var out []string
	for _, c := range r.Checks {
		out = append(out, c.Name)
	}
	return out
}

func hasCheck(r *Report, name string) bool {
	for _, c := range r.Checks {
		if c.Name == name {
			return true
		}
	}
	return false
}

// abandonedSocket leaves a real Unix socket at path with nothing listening —
// the state a killed daemon leaves behind. SetUnlinkOnClose(false) is the whole
// point: Go unlinks the socket on Close by default, which would clean up
// exactly the debris the check exists to find.
func abandonedSocket(t *testing.T, path string) string {
	t.Helper()
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, l.Close())
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// respondingDaemon is a health probe for a daemon that answers and reports
// version v.
func respondingDaemon(v string) func() daemon.HealthStatus {
	return func() daemon.HealthStatus {
		return daemon.HealthStatus{SocketPath: "/fake/daemon.sock", SocketExists: true, DaemonVersion: v}
	}
}

func TestVersionSkew_DaemonOlderThanClient_Fails(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.180")

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon version")
	require.Equal(t, StatusFail, c.Status)
	require.Contains(t, c.Detail, "1.0.180", "the detail must name the version the daemon runs")
	require.Contains(t, c.Detail, "1.0.192", "the detail must name the client version")
	require.Contains(t, c.Detail, "unknown field", "the detail must connect skew to the symptom users see")
	require.Contains(t, c.Remediation, "af daemon restart")
	require.True(t, c.Problem, "skew must count toward the exit code")
}

// A daemon that answers but reports no version predates the version field, so
// it is necessarily older than the client asking — the exact shape of the
// upgrade-without-restart bug, and the one an older daemon cannot self-report.
func TestVersionSkew_DaemonPredatesVersionReporting_Fails(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("")

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon version")
	require.Equal(t, StatusFail, c.Status)
	require.Contains(t, c.Detail, "predates version reporting")
	require.Contains(t, c.Detail, "1.0.192")
	require.Contains(t, c.Remediation, "af daemon restart")
}

func TestVersionSkew_Matching_Passes(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.192")

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon version")
	require.Equal(t, StatusPass, c.Status)
	require.Contains(t, c.Detail, "1.0.192")
	require.False(t, c.Problem)
}

// A dev client cannot be compared against a released daemon: two dev builds
// report the same string while differing in content. Warn, never fail.
func TestVersionSkew_DevClient_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = devVersion
	opts.daemonHealth = respondingDaemon("1.0.192")

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon version")
	require.Equal(t, StatusWarn, c.Status)
	require.False(t, c.Problem, "an unjudgeable dev build must not fail the run")
}

// With no daemon answering there is no version to compare, and claiming skew
// would be a lie — checkDaemonHealth owns that state.
func TestVersionSkew_NoDaemon_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"

	report, err := Run(opts)
	require.NoError(t, err)
	require.False(t, hasCheck(report, "daemon version"),
		"a version row with nothing to compare would be noise")
}

func TestDuplicateDaemons_TwoOnThisHome_Fails(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	// argv0 "af" + a --daemon token makes these shells read as daemons while
	// being nothing but this test's own children.
	first := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": home})
	second := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": home})

	opts := testOptionsWithHome(t, home, false, first.PID, second.PID)
	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon instances")
	require.Equal(t, StatusFail, c.Status)
	require.Contains(t, c.Detail, "2 daemons")
	require.Contains(t, c.Detail, "pid "+strconv.Itoa(first.PID))
	require.Contains(t, c.Detail, "pid "+strconv.Itoa(second.PID))
	require.Contains(t, c.Remediation, "af reset")
	require.True(t, c.Problem)
}

func TestDuplicateDaemons_SingleDaemon_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	only := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": home})

	report, err := Run(testOptionsWithHome(t, home, false, only.PID))
	require.NoError(t, err)
	require.False(t, hasCheck(report, "daemon instances"), "one daemon is the invariant holding")
}

// A daemon serving a different home is not a duplicate: it is a separate
// install, and killing or counting it here would be wrong (checkForeignDaemons
// owns it).
func TestDuplicateDaemons_OtherHomeIgnored(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	mine := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": home})
	theirs := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": t.TempDir()})

	report, err := Run(testOptionsWithHome(t, home, false, mine.PID, theirs.PID))
	require.NoError(t, err)
	require.False(t, hasCheck(report, "daemon instances"),
		"a daemon on another home must not count as a duplicate of ours")
}

func TestAutostartPath_UnitLaunchesDifferentBinary_Fails(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, Path: "/fake/unit.service", ExecPath: "/usr/local/bin/af"}
	}
	opts.selfBinary = func() (string, error) { return "/home/dev/.local/bin/af", nil }

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart path")
	require.Equal(t, StatusFail, c.Status)
	require.Contains(t, c.Detail, "/usr/local/bin/af")
	require.Contains(t, c.Detail, "/home/dev/.local/bin/af")
	require.Contains(t, c.Remediation, "af daemon install")
	require.True(t, c.Problem)
}

func TestAutostartPath_Matching_Passes(t *testing.T) {
	testguard.IsolateTmux(t)

	// A real path on disk, so EvalSymlinks resolves identically on both sides
	// (on macOS /tmp is itself a symlink, which is exactly the false positive
	// resolvePath exists to avoid).
	bin := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	opts := testOptions(t, false)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: bin}
	}
	opts.selfBinary = func() (string, error) { return bin, nil }

	report, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, StatusPass, findCheck(t, report, "autostart path").Status)
}

// A symlinked install is one binary, not two: resolving both sides is what
// keeps ~/.local/bin/af -> /nix/store/… from reading as a split brain.
func TestAutostartPath_SymlinkedInstall_Passes(t *testing.T) {
	testguard.IsolateTmux(t)

	dir := t.TempDir()
	real := filepath.Join(dir, "af-real")
	require.NoError(t, os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755))
	link := filepath.Join(dir, "af")
	require.NoError(t, os.Symlink(real, link))

	opts := testOptions(t, false)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: link}
	}
	opts.selfBinary = func() (string, error) { return real, nil }

	report, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, StatusPass, findCheck(t, report, "autostart path").Status,
		"a symlink to the same binary is not a path mismatch")
}

func TestAutostartPath_NoUnitInstalled_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	report, err := Run(testOptions(t, false))
	require.NoError(t, err)
	require.False(t, hasCheck(report, "autostart path"), "no unit means nothing to compare")
}

func TestSplitBrainBinaries_DifferentVersions_Fails(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.binaryCandidates = func() []string { return []string{"/usr/local/bin/af", "/home/dev/.local/bin/af"} }
	opts.binaryVersion = func(path string) (string, error) {
		if path == "/usr/local/bin/af" {
			return "1.0.180", nil
		}
		return "1.0.192", nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "af binaries")
	require.Equal(t, StatusFail, c.Status)
	require.Contains(t, c.Detail, "/usr/local/bin/af@1.0.180")
	require.Contains(t, c.Detail, "/home/dev/.local/bin/af@1.0.192")
	require.True(t, c.Problem)
}

func TestSplitBrainBinaries_SameVersion_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.binaryCandidates = func() []string { return []string{"/usr/local/bin/af", "/home/dev/.local/bin/af"} }
	opts.binaryVersion = func(string) (string, error) { return "1.0.192", nil }

	report, err := Run(opts)
	require.NoError(t, err)
	require.False(t, hasCheck(report, "af binaries"), "installs that agree cannot strand a daemon")
}

func TestStaleSocket_NoDaemonAnswering_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	sockPath := abandonedSocket(t, filepath.Join(home, "daemon.sock"))
	require.FileExists(t, sockPath)

	opts := testOptionsWithHome(t, home, false)
	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "stale sockets")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "daemon.sock")
	require.Contains(t, c.Remediation, "af reset")
	require.True(t, c.Problem)
}

// The name is a convention, not proof: a plain file that borrowed the name is
// not a socket, and telling the user to `af reset` over it would be wrong.
func TestStaleSocket_RegularFileWithSocketName_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "daemon.sock"), []byte("not a socket"), 0o600))

	report, err := Run(testOptionsWithHome(t, home, false))
	require.NoError(t, err)
	require.False(t, hasCheck(report, "stale sockets"), "only a real socket counts")
}

func TestStaleSocket_DaemonAnswering_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	abandonedSocket(t, filepath.Join(home, "daemon.sock"))

	opts := testOptionsWithHome(t, home, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.192")

	report, err := Run(opts)
	require.NoError(t, err)
	require.False(t, hasCheck(report, "stale sockets"), "a socket a daemon answers on is not stale")
}

// The macOS domain mismatch: the agent is loaded, so it looks supervised, but
// `launchctl kickstart -k gui/<uid>/…` restarts sail past it and the old daemon
// lives on.
func TestAutostartSupervision_LoadedInWrongDomain_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true, Enabled: true,
			Domain: "gui/501/com.agent-factory.daemon", LoadedElsewhere: true,
			Detail: "loaded outside gui/501/com.agent-factory.daemon",
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "gui/501/com.agent-factory.daemon")
	require.Contains(t, c.Remediation, "af daemon install")
	require.True(t, c.Problem)
}

func TestAutostartSupervision_UnitPresentButInactive_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true, Enabled: true, Active: false,
			Detail: "is-enabled=enabled is-active=inactive",
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "not running it")
	require.True(t, c.Problem)
}

func TestAutostartSupervision_EnabledAndActive_Passes(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Enabled: true, Active: true}
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, StatusPass, findCheck(t, report, "autostart supervision").Status)
}

// The all-clear: a healthy machine must produce no skew problem at all, or the
// checks are just noise users learn to ignore.
func TestSkewChecks_HealthyMachine_AllPass(t *testing.T) {
	testguard.IsolateTmux(t)

	bin := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.192")
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: bin}
	}
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Enabled: true, Active: true}
	}
	opts.selfBinary = func() (string, error) { return bin, nil }
	opts.binaryCandidates = func() []string { return []string{bin} }
	opts.binaryVersion = func(string) (string, error) { return "1.0.192", nil }

	report, err := Run(opts)
	require.NoError(t, err)

	for _, name := range []string{"daemon version", "autostart path", "autostart supervision"} {
		require.Equal(t, StatusPass, findCheck(t, report, name).Status, "check %q", name)
	}
	for _, name := range []string{"daemon instances", "af binaries", "stale sockets"} {
		require.False(t, hasCheck(report, name), "healthy machine must not report %q", name)
	}
	require.Zero(t, report.UnresolvedCount(), "a healthy machine must exit 0")
}

func TestRenderJSON_EnvelopeShape(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.180")

	report, err := Run(opts)
	require.NoError(t, err)

	var buf strings.Builder
	require.NoError(t, RenderJSON(&buf, report, false, false))

	var env struct {
		Data struct {
			Checks []struct {
				Name    string `json:"name"`
				Section string `json:"section"`
				Status  string `json:"status"`
				Detail  string `json:"detail"`
				Remedy  string `json:"remedy"`
			} `json:"checks"`
			Summary struct {
				Pass       int `json:"pass"`
				Warn       int `json:"warn"`
				Fail       int `json:"fail"`
				Unresolved int `json:"unresolved"`
			} `json:"summary"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &env))
	require.Nil(t, env.Error, "a successful run carries a null error member")
	require.NotEmpty(t, env.Data.Checks)

	var skew *struct {
		Name    string `json:"name"`
		Section string `json:"section"`
		Status  string `json:"status"`
		Detail  string `json:"detail"`
		Remedy  string `json:"remedy"`
	}
	for i := range env.Data.Checks {
		if env.Data.Checks[i].Name == "daemon version" {
			skew = &env.Data.Checks[i]
		}
	}
	require.NotNil(t, skew, "the skew check must be scriptable")
	require.Equal(t, "FAIL", skew.Status)
	require.Equal(t, sectionDaemon, skew.Section)
	require.Contains(t, skew.Remedy, "af daemon restart")
	require.GreaterOrEqual(t, env.Data.Summary.Fail, 1)
	require.GreaterOrEqual(t, env.Data.Summary.Unresolved, 1)
}

// Passing checks carry no remedy, so `.remedy != ""` is a usable "needs
// action" predicate for scripts.
func TestRenderJSON_PassingCheckHasNoRemedy(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.192")

	report, err := Run(opts)
	require.NoError(t, err)

	payload := BuildJSONReport(report, false, false)
	for _, c := range payload.Checks {
		if c.Status == string(StatusPass) {
			require.Empty(t, c.Remedy, "passing check %q must carry no remedy", c.Name)
		}
	}
}

// Text output must stay byte-clean when it is not going to a terminal —
// piping `af doctor` into a file or a grep must not embed ANSI codes.
func TestRender_NonTerminalWriterHasNoANSI(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.180")

	report, err := Run(opts)
	require.NoError(t, err)

	var buf strings.Builder
	Render(&buf, report, false, false)
	require.NotContains(t, buf.String(), "\x1b[", "no escape codes when the writer is not a terminal")
	require.Contains(t, buf.String(), "FAIL")
}
