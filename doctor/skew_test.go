package doctor

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/proctree"
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

// socketTempHome is t.TempDir() for tests that must bind a Unix socket inside
// the home.
//
// A socket path is capped at sun_path — 104 bytes on darwin, 108 on Linux — and
// macOS puts TMPDIR under /var/folders/<32 chars>/T/, so t.TempDir() plus a test
// name plus "daemon-http.sock" overflows it and bind fails with "invalid
// argument". Linux's short /tmp hides the limit entirely, which is why this only
// surfaced on the macOS runner. /tmp is short on both.
func socketTempHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "afdoc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
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

// stubForeignProcess makes pid look, to doctor, like it belongs to another user
// with an environ we may not read — what a second user's `af --daemon` really is
// on a shared box. Every other pid keeps its real facts.
//
// Staged rather than observed: a test cannot spawn a process as a second user
// without root, and "some pid has an unreadable environ" is not portable — pid 1
// is unreadable on the dev box but owned by the test user inside the container.
func stubForeignProcess(t *testing.T, pid int) {
	t.Helper()
	prevEnv, prevUID := daemonProcessEnvLookup, daemonProcessOwnerUID
	t.Cleanup(func() {
		daemonProcessEnvLookup, daemonProcessOwnerUID = prevEnv, prevUID
	})
	daemonProcessEnvLookup = func(p int, key string) (string, bool, error) {
		if p == pid {
			return "", false, os.ErrPermission
		}
		return prevEnv(p, key)
	}
	daemonProcessOwnerUID = func(p int) (int, bool) {
		if p == pid {
			return os.Getuid() + 1, true
		}
		return prevUID(p)
	}
}

// stubProcessHomes makes doctor read a chosen AGENT_FACTORY_HOME for the given
// pids, leaving every other pid's real environ alone.
//
// Injected rather than inherited: /proc/<pid>/environ reflects a process's
// INITIAL environment, so neither t.Setenv nor spawnWithEnv can give a running
// process a home spelled the way a test needs (a raw tilde, say).
func stubProcessHomes(t *testing.T, homes map[int]string) {
	t.Helper()
	prev := daemonProcessEnvLookup
	t.Cleanup(func() { daemonProcessEnvLookup = prev })
	daemonProcessEnvLookup = func(pid int, key string) (string, bool, error) {
		if home, ok := homes[pid]; ok && key == "AGENT_FACTORY_HOME" {
			return home, true, nil
		}
		return prev(pid, key)
	}
}

// stubProcessEnv gives one pid a complete fake environ, so a test can put a
// daemon in a frame of reference (its own HOME, its own AGENT_FACTORY_HOME
// spelling) that differs from doctor's. Other pids keep their real environ.
//
// A variable absent from env reads as absent-but-readable — the state that
// legitimately means "this daemon serves the default home".
func stubProcessEnv(t *testing.T, pid int, env map[string]string) {
	t.Helper()
	prev := daemonProcessEnvLookup
	t.Cleanup(func() { daemonProcessEnvLookup = prev })
	daemonProcessEnvLookup = func(p int, key string) (string, bool, error) {
		if p != pid {
			return prev(p, key)
		}
		v, ok := env[key]
		return v, ok, nil
	}
}

// stubProcessCwd fixes the working directory doctor reads for the given pids;
// any pid absent from the map reads as unreadable.
func stubProcessCwd(t *testing.T, cwds map[int]string) {
	t.Helper()
	prev := daemonProcessCwd
	t.Cleanup(func() { daemonProcessCwd = prev })
	daemonProcessCwd = func(pid int) (string, bool) {
		dir, ok := cwds[pid]
		return dir, ok
	}
}

// ourAutostartUnit declares that the installed autostart unit is THIS home's,
// which is the precondition for any autostart row to be about us at all. Tests
// that inject a unit must say so explicitly — the default is "no unit for this
// home", because a unit file existing proves nothing about whose home it serves.
func ourAutostartUnit(opts *Options) {
	opts.autostartServesHome = func(string) (bool, bool, error) { return true, true, nil }
}

// respondingDaemon is a health probe for a daemon that answers and reports
// version v.
func respondingDaemon(v string) func() daemon.HealthStatus {
	return func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath: "/fake/daemon.sock", SocketExists: true, DaemonVersion: v,
			// A healthy daemon has a healthy HTTP listener too; tests that care
			// about the HTTP socket override these.
			HTTPSocketPath: "/fake/daemon-http.sock", HTTPSocketExists: true, HTTPListening: daemon.AnswerYes(),
		}
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

// A daemon built from source reports "dev", which identifies no release and so
// is neither equal nor unequal to a client version in any way that predicts
// compatibility. Failing here would tell everyone running a self-built daemon to
// restart it, forever.
func TestVersionSkew_DevDaemon_WarnsNotFails(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon(devVersion)

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon version")
	require.Equal(t, StatusWarn, c.Status, "an unjudgeable version must not FAIL")
	require.Contains(t, c.Detail, "cannot be judged", "the row must say why it is not a verdict")
	require.False(t, c.Problem, "an unjudgeable version must not drive a nonzero exit")
	require.Zero(t, report.UnresolvedCount(), "a dev daemon on a dev box must exit 0")
}

// A versionless daemon is a DEFINITE verdict, even for a dev client, and must
// be judged before the "cannot judge an unreleased build" catch-all.
//
// An empty version means the daemon predates the Ping field entirely, so it is
// older than any client that can ask — a source-built client included, since it
// carries the very field it is asking with. Letting the dev case swallow this
// hides real skew from exactly the people running dev builds.
func TestVersionSkew_DevClientVersionlessDaemon_StillFails(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = devVersion
	opts.daemonHealth = respondingDaemon("") // predates version reporting

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon version")
	require.Equal(t, StatusFail, c.Status,
		"a daemon older than the version field is skew we can prove, dev client or not")
	require.Contains(t, c.Detail, "predates version reporting")
	require.True(t, c.Problem)
	require.Positive(t, report.UnresolvedCount())
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
	// The remedy stops a daemon; it must never reach for the user's data.
	require.Contains(t, c.Remediation, "af daemon restart")
	require.NotContains(t, c.Remediation, "af reset")
	require.True(t, c.Problem)
}

// The false negative, which is the same lie told the other way round: run
// `af doctor` from a daemon-spawned child (a watch task's shell) and the real
// serving daemon is an ancestor. Dropping ancestors from the scan left the stale
// EXTRA daemon looking like the single legitimate one — PASS on a split-brained
// box.
//
// This test's own process stands in for the ancestor daemon: it is in
// selfAncestors by construction, which is exactly the condition under test.
func TestDuplicateDaemons_AncestorDaemonIsCounted(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	stubDaemonProcessProbe(t,
		func(int) bool { return true },
		func(int) []string { return []string{"af", "--daemon"} })

	stale := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": home})
	// Both the ancestor (us) and the stale extra serve this home.
	stubProcessHomes(t, map[int]string{os.Getpid(): home, stale.PID: home})

	opts := testOptionsWithHome(t, home, false, os.Getpid(), stale.PID)
	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon instances")
	require.Equal(t, StatusFail, c.Status,
		"a duplicate must be found even when the real daemon is our own ancestor")
	require.Contains(t, c.Detail, "2 daemons")
	require.Contains(t, c.Detail, "pid "+strconv.Itoa(stale.PID))
	require.Contains(t, c.Detail, "pid "+strconv.Itoa(os.Getpid()))
}

// Counting an ancestor is safe; killing one is not. --fix must never offer to
// kill the daemon that is running this very command.
func TestForeignDaemons_AncestorNeverOfferedForKill(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	stubDaemonProcessProbe(t,
		func(int) bool { return true },
		func(int) []string { return []string{"af", "--daemon"} })
	// Our own process, presented as an ancestor daemon serving ANOTHER home —
	// the shape that would otherwise reach the foreign-daemon kill path.
	otherHome := t.TempDir()
	stubProcessHomes(t, map[int]string{os.Getpid(): otherHome})

	report, err := Run(testOptionsWithHome(t, home, true, os.Getpid()))
	require.NoError(t, err)

	for _, f := range report.Findings {
		require.NotContains(t, f.FixAction, strconv.Itoa(os.Getpid()),
			"doctor must never offer to kill its own ancestor")
	}
	require.False(t, hasFinding(report, "foreign-daemon"),
		"an ancestor is not a foreign daemon to reap")
}

// A remedy must cost no more than the problem it answers.
//
// `af reset` is a factory wipe: it removes ALL sessions (live and archived),
// all tasks, all AF worktrees and branches, and all state. Doctor is what people
// reach for when af is already misbehaving, and they follow what it says — so a
// disproportionate remedy here is worse than none, because they would never have
// run a wipe on their own. Nothing doctor detects about daemon LIFECYCLE
// (duplicates, stale sockets, skew, autostart) is answered by destroying the
// user's work; a daemon costs nothing to restart.
//
// This asserts over every row a real run can produce, so a future check cannot
// quietly reintroduce it.
func TestRemedies_NeverRecommendDestructiveResetForDaemonProblems(t *testing.T) {
	testguard.IsolateTmux(t)

	home := socketTempHome(t)
	// Stage every daemon-lifecycle problem at once: two daemons, a stale HTTP
	// socket, a skewed daemon, and a mismatched autostart unit.
	stubDaemonProcessProbe(t,
		func(int) bool { return true },
		func(int) []string { return []string{"af", "--daemon"} })
	first := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": home})
	second := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": home})
	stubProcessHomes(t, map[int]string{first.PID: home, second.PID: home})
	abandonedSocket(t, filepath.Join(home, "daemon-http.sock"))

	opts := testOptionsWithHome(t, home, false, first.PID, second.PID)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.180")

	report, err := Run(opts)
	require.NoError(t, err)
	require.Positive(t, report.UnresolvedCount(), "test premise: real problems were detected")

	for _, c := range BuildJSONReport(report, false, true).Checks {
		require.NotContains(t, c.Remedy, "af reset",
			"check %q recommends a factory wipe (all sessions, tasks, worktrees, state) — "+
				"no daemon-lifecycle problem justifies that", c.Name)
	}
	// And the duplicate row must still give an answer that actually works.
	require.Contains(t, findCheck(t, report, "daemon instances").Remediation, "af daemon restart")
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

// The shared-box false positive, staged against a genuinely foreign process:
// pid 1 is owned by root and its /proc/1/environ is unreadable to us, while
// /proc/1/cmdline is world-readable — exactly the asymmetry that makes another
// user's `af --daemon` visible but unattributable. Only the argv is faked, so
// nothing but ownership and the unreadable environ can exclude it.
//
// The active home must be the DEFAULT home for this to bite: the old code
// answered "environ unreadable" with "then it must serve the default home", so
// the foreign process landed on ours and became a second daemon that does not
// exist.
func TestDuplicateDaemons_ForeignUserDaemon_NotAttributedHere(t *testing.T) {
	testguard.IsolateTmux(t)

	// Make every process in the (2-pid) snapshot look like an af daemon, so
	// only ownership and the unreadable environ can tell them apart.
	stubDaemonProcessProbe(t,
		func(int) bool { return true },
		func(int) []string { return []string{"af", "--daemon"} })

	// $HOME is not sandboxed by the package harness (only AGENT_FACTORY_HOME
	// is), so leaving it alone would point this test at the developer's real
	// ~/.agent-factory and read their live daemon's sockets. Pin it.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	defaultHome := filepath.Join(fakeHome, ".agent-factory")
	require.NoError(t, os.MkdirAll(defaultHome, 0o755))

	// This user's own daemon, explicitly on the default home, and a second
	// process standing in for the other user's daemon. Both are real processes
	// this test owns; only doctor's view of the second one is foreign.
	mine := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": defaultHome})
	theirs := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubForeignProcess(t, theirs.PID)

	opts := testOptionsWithHome(t, defaultHome, false, mine.PID, theirs.PID)
	report, err := Run(opts)
	require.NoError(t, err)

	require.False(t, hasCheck(report, "daemon instances"),
		"another user's daemon must never be counted as a duplicate on this home")
	require.False(t, hasFinding(report, "foreign-daemon"),
		"nor may it be reported as a foreign daemon we could kill")
	require.Zero(t, report.UnresolvedCount(),
		"a healthy machine that merely shares /proc with another af user must exit 0")
}

func hasFinding(r *Report, check string) bool {
	for _, f := range r.Findings {
		if f.Check == check {
			return true
		}
	}
	return false
}

// The same guard one level down: a daemon whose environ cannot be read is
// unattributed, never assumed to be ours. This is the single line that decides
// whether another user's daemon becomes our phantom duplicate.
func TestDaemonProcessHome_UnreadableEnvironIsUnknownNotDefault(t *testing.T) {
	proc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubForeignProcess(t, proc.PID)

	home, known := daemonProcessHome(proc.PID)
	require.False(t, known, "an unreadable environ must not be reported as a known home")
	require.Empty(t, home, "no home may be guessed from an unreadable environ")
}

// A daemon's home must be resolved in the DAEMON's frame, never ours. This one
// carries no AGENT_FACTORY_HOME and a HOME of its own, so its home is
// <its HOME>/.agent-factory — nothing to do with the HOME doctor happens to run
// under. Using ours attributes a sandbox/debug daemon to the home we are
// inspecting, or hides a genuinely foreign one.
func TestDaemonProcessHome_DefaultResolvesAgainstDaemonHOME(t *testing.T) {
	daemonHome := t.TempDir()
	ourHome := t.TempDir()
	require.NotEqual(t, daemonHome, ourHome)
	t.Setenv("HOME", ourHome) // doctor's frame — deliberately different

	proc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	// The daemon's environ: its own HOME, and no AGENT_FACTORY_HOME at all.
	stubProcessEnv(t, proc.PID, map[string]string{"HOME": daemonHome})

	home, known := daemonProcessHome(proc.PID)
	require.True(t, known)
	require.Equal(t, filepath.Join(daemonHome, ".agent-factory"), home,
		"the default home must derive from the DAEMON's HOME, not doctor's")
}

// A tilde in the daemon's AGENT_FACTORY_HOME expands against ITS HOME.
func TestDaemonProcessHome_TildeExpandsAgainstDaemonHOME(t *testing.T) {
	daemonHome := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // doctor's frame — different again

	proc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubProcessEnv(t, proc.PID, map[string]string{
		"HOME":               daemonHome,
		"AGENT_FACTORY_HOME": "~/af-home",
	})

	home, known := daemonProcessHome(proc.PID)
	require.True(t, known)
	require.Equal(t, filepath.Join(daemonHome, "af-home"), home,
		"the tilde must expand against the DAEMON's HOME")
}

// A relative AGENT_FACTORY_HOME (which config.GetConfigDir accepts as-is) means
// whatever the DAEMON's cwd makes it mean. Resolving against doctor's cwd makes
// the same daemon classify differently depending on where doctor was invoked.
func TestDaemonProcessHome_RelativeResolvesAgainstDaemonCwd(t *testing.T) {
	daemonCwd := t.TempDir()
	proc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubProcessEnv(t, proc.PID, map[string]string{
		"HOME":               t.TempDir(),
		"AGENT_FACTORY_HOME": "af-home", // relative
	})
	stubProcessCwd(t, map[int]string{proc.PID: daemonCwd})

	home, known := daemonProcessHome(proc.PID)
	require.True(t, known)
	require.Equal(t, filepath.Join(daemonCwd, "af-home"), home,
		"a relative home must resolve against the DAEMON's cwd, not doctor's")
}

// A relative home whose frame we cannot read is unknown, not guessed.
func TestDaemonProcessHome_RelativeWithUnreadableCwdIsUnknown(t *testing.T) {
	proc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubProcessEnv(t, proc.PID, map[string]string{
		"HOME":               t.TempDir(),
		"AGENT_FACTORY_HOME": "af-home",
	})
	stubProcessCwd(t, map[int]string{}) // cwd unreadable for every pid

	_, known := daemonProcessHome(proc.PID)
	require.False(t, known, "a relative home with no readable cwd cannot be resolved")
}

// The other half of the guard: a readable environ still yields a known home.
// Tightening the unreadable case must not make doctor blind to real daemons.
//
// CanonicalTempDir, not t.TempDir: daemonProcessHome resolves the home through
// resolveHomeIn, which EvalSymlinks it — and on macOS an EXISTING temp dir
// resolves /var/folders/… to /private/var/folders/… (the #1918 class). This
// test spawns a real process and reads its environ, so it began RUNNING on macOS
// only once this PR gave that read a darwin backend AND removed the RequireProcFS
// skip that spawnWithEnv used to carry — before, it was skipped there. Comparing
// the resolved home against an unresolved t.TempDir() is the stale assumption
// that first-ever-macOS-run then exposed; setting the env to the canonical
// spelling at the source makes the expectation correct on both platforms. The
// sibling tests above avoid this only because their expected homes are
// non-existent subdirs, which EvalSymlinks leaves unresolved.
func TestDaemonProcessHome_ReadableEnvironIsKnown(t *testing.T) {
	explicit := testguard.CanonicalTempDir(t)
	proc := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": explicit})

	home, known := daemonProcessHome(proc.PID)
	require.True(t, known, "our own readable environ yields a known home")
	require.Equal(t, explicit, home)
}

// Two spellings of one home must compare equal, or the duplicate check reads a
// real duplicate as a daemon on some other home and stays silent. The tilde is
// the case that actually ships: config.GetConfigDir expands AGENT_FACTORY_HOME
// while a daemon's environ preserves the raw "~/af-home" it was launched with.
func TestDuplicateDaemons_HomeSpellingsCompareEqual(t *testing.T) {
	testguard.IsolateTmux(t)

	realHome := t.TempDir()
	t.Setenv("HOME", filepath.Dir(realHome))
	tildeSpelling := filepath.Join("~", filepath.Base(realHome))
	require.Equal(t, realHome, config.ExpandTilde(tildeSpelling), "test premise: the two spell one dir")

	stubDaemonProcessProbe(t,
		func(int) bool { return true },
		func(int) []string { return []string{"af", "--daemon"} })

	first := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	second := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	// One daemon spells the home with a tilde, the other with a trailing slash.
	// Both serve the very home doctor is inspecting.
	stubProcessHomes(t, map[int]string{
		first.PID:  tildeSpelling,
		second.PID: realHome + "/",
	})

	opts := testOptionsWithHome(t, realHome, false, first.PID, second.PID)
	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon instances")
	require.Equal(t, StatusFail, c.Status,
		"two spellings of this home are two daemons on this home")
	require.Contains(t, c.Detail, "2 daemons")
	require.False(t, hasFinding(report, "foreign-daemon"),
		"a differently-spelled home is not a foreign home")
}

func TestNormalizeHome_EquatesSpellings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Dir(dir))
	tilde := filepath.Join("~", filepath.Base(dir))

	want := normalizeHome(dir)
	require.Equal(t, want, normalizeHome(tilde), "tilde vs expanded")
	require.Equal(t, want, normalizeHome(dir+"/"), "trailing slash")
	require.Equal(t, want, normalizeHome(dir+"/./"), "unclean path")

	// A symlinked home is the same home (on macOS /var vs /private/var is this
	// exact case, #1918).
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	require.Equal(t, want, normalizeHome(link), "symlinked home")

	require.NotEqual(t, want, normalizeHome(t.TempDir()), "genuinely different homes stay different")
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

// A foreign binary that happens to be named `af` must never be read as an
// agent-factory install. Plenty of programs answer `version` with a zero exit
// and print something; accepting "the last word of line 1" would let any of
// them fabricate a split-brain install and exit nonzero.
func TestSplitBrainBinaries_ForeignAfBinaryIgnored(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.binaryCandidates = func() []string { return []string{"/usr/local/bin/af", "/home/dev/.local/bin/af"} }
	opts.binaryVersion = func(path string) (string, error) {
		if path == "/usr/local/bin/af" {
			// Some other tool called af (an assembler, a fetcher…) answering
			// `version` perfectly happily.
			return parseAFVersion("af 4.2.0 (GNU binutils)"), nil
		}
		return parseAFVersion("agent-factory version 1.0.192"), nil
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.False(t, hasCheck(report, "af binaries"),
		"a non-agent-factory binary named af must not fabricate a split brain")
	require.Zero(t, report.UnresolvedCount())
}

// The candidate list is INSTALL locations. The binary you happen to be running
// is not one: a scratch build out of a temp dir or worktree — routine on a dev
// box — would otherwise read as a rogue install at a different version, and the
// remedy ("remove the stale install") would be actively wrong advice.
func TestDefaultBinaryCandidates_ExcludesRunningBinary(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)

	for _, c := range defaultBinaryCandidates() {
		require.NotEqual(t, resolvePath(self), resolvePath(c),
			"the running test binary is not an af install")
	}
}

// fakeAFBinary writes an executable that behaves like `af version` per the
// given script body, and returns its path.
func fakeAFBinary(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755))
	return path
}

// The version probe runs a binary doctor merely FOUND on a PATH entry — it may
// be a wrapper, a stale build, or not af at all. If it wedges holding stdout,
// an exec.CommandContext deadline does not bound Output(): the direct child is
// killed and the call still waits on pipe EOF. That hangs `af doctor`, the one
// command a user runs because things are already wedged (#1967).
//
// A real hanging binary through the real probe, so the bound is actually
// exercised.
func TestExecBinaryVersion_HangIsBounded(t *testing.T) {
	// Prints nothing and never exits; a child holds stdout past the kill.
	bin := fakeAFBinary(t, "sleep 300 &\nsleep 300\n")

	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := execBinaryVersion(bin); done <- err }()

	select {
	case err := <-done:
		require.Error(t, err, "a killed probe is not an answer")
		require.Contains(t, err.Error(), "timed out")
	case <-time.After(30 * time.Second):
		t.Fatal("execBinaryVersion never returned: its deadline does not bound it")
	}
	require.Less(t, time.Since(start), 25*time.Second, "the deadline must bound the call")
}

// The mirror image: a binary that ANSWERED but left a straggler holding the pipe
// must still yield its version. Reporting a failure there would drop a real
// install out of the split-brain comparison.
func TestExecBinaryVersion_StragglerStillYieldsTheAnswer(t *testing.T) {
	bin := fakeAFBinary(t, "echo 'agent-factory version 1.0.192'\nsleep 60 &\n")

	got, err := execBinaryVersion(bin)
	require.NoError(t, err, "the binary answered; a lingering pipe-holder is not a failure")
	require.Equal(t, "1.0.192", got)
}

func TestParseAFVersion_ShapeIsRequired(t *testing.T) {
	// Accepted: our real output, including the released two-line form and an
	// unreleased build.
	require.Equal(t, "1.0.192", parseAFVersion("agent-factory version 1.0.192"))
	require.Equal(t, "1.0.192", parseAFVersion("agent-factory version 1.0.192\nhttps://example.com/releases/tag/v1.0.192\n"))
	require.Equal(t, "1.0.193-rc1", parseAFVersion("agent-factory version 1.0.193-rc1"))
	require.Equal(t, "dev", parseAFVersion("agent-factory version dev"))

	// Rejected: anything that is not us announcing ourselves.
	for _, out := range []string{
		"af 4.2.0 (GNU binutils)",
		"version 1.0.192",
		"agent-factory version",
		"agent-factory version not-a-version",
		"some-tool version 1.0.192",
		"AGENT-FACTORY VERSION 1.0.192",
		"",
	} {
		require.Empty(t, parseAFVersion(out), "must reject %q", out)
	}
}

// One condition, one finding. A stale control socket is already
// checkDaemonHealth's FAIL, with the same remedy; billing it again here would
// inflate the issue count and hand a script two actionable rows for one fix.
func TestStaleSocket_ControlSocketNotDoubleCounted(t *testing.T) {
	testguard.IsolateTmux(t)

	home := socketTempHome(t)
	// ONLY the control socket is stale.
	abandonedSocket(t, filepath.Join(home, daemon.ControlSocketName()))

	opts := testOptionsWithHome(t, home, false)
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath:   filepath.Join(home, daemon.ControlSocketName()),
			SocketExists: true,
			PingErr:      errNoDaemon,
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	// checkDaemonHealth owns it and reports it.
	require.Equal(t, StatusFail, findCheck(t, report, "daemon").Status)
	require.False(t, hasCheck(report, "stale sockets"),
		"the control socket is checkDaemonHealth's row; a second row is the same issue billed twice")

	actionable := 0
	for _, c := range BuildJSONReport(report, false, false).Checks {
		if c.Actionable {
			actionable++
		}
	}
	require.Equal(t, 1, actionable, "one stale control socket must produce exactly one actionable row")
}

// The HTTP socket is nobody else's job: health never probes it, so if this check
// stayed quiet about it, nothing would mention it at all.
func TestStaleSocket_HTTPSocketStillReported(t *testing.T) {
	testguard.IsolateTmux(t)

	home := socketTempHome(t)
	abandonedSocket(t, filepath.Join(home, "daemon-http.sock"))

	report, err := Run(testOptionsWithHome(t, home, false))
	require.NoError(t, err)

	c := findCheck(t, report, "stale sockets")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "daemon-http.sock")
	require.NotContains(t, c.Detail, daemon.ControlSocketName())
	require.True(t, c.Problem)
}

// A healthy control socket says nothing about the HTTP socket: they are
// separate listeners, RunDaemon keeps serving the control socket when
// startHTTPServer fails, and the TUI/web/HTTP clients dial the HTTP one. Gating
// on the ping let doctor report all-clear over a dead web surface.
func TestHTTPSocket_StaleWhileControlHealthy_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath: "/fake/daemon.sock", SocketExists: true, DaemonVersion: "1.0.192",
			// Control socket perfectly healthy (PingErr nil)...
			HTTPSocketPath: "/fake/daemon-http.sock", HTTPSocketExists: true,
			HTTPListening: daemon.AnswerNo(), // ...but nothing answers on HTTP.
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	require.Equal(t, StatusPass, findCheck(t, report, "daemon").Status,
		"the control socket really is healthy — that is the point")
	c := findCheck(t, report, "http socket")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "nothing answers")
	require.True(t, c.Problem, "a dead web surface is a real problem, not advisory")
	require.Positive(t, report.UnresolvedCount(), "doctor must not report all-clear")
}

// startHTTPServer failing is non-fatal to RunDaemon, so the socket can simply
// not be there while the daemon runs happily.
func TestHTTPSocket_MissingWhileDaemonRuns_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath: "/fake/daemon.sock", SocketExists: true, DaemonVersion: "1.0.192",
			HTTPSocketPath: "/fake/daemon-http.sock", HTTPSocketExists: false,
			HTTPListening: daemon.AnswerNo(),
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "http socket")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "not there")
	require.True(t, c.Problem)
}

func TestHTTPSocket_Healthy_Passes(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.192")

	report, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, StatusPass, findCheck(t, report, "http socket").Status)
}

// With no daemon at all, its HTTP socket being gone is expected — the row would
// be noise, and any leftover file is checkStaleSockets' story.
func TestHTTPSocket_NoDaemon_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	report, err := Run(testOptions(t, false))
	require.NoError(t, err)
	require.False(t, hasCheck(report, "http socket"))
}

// A diagnostic that cannot see must say so, never PASS. proctree is /proc-only,
// so on macOS the process scan yields nothing (#1939) and this check never runs
// — silence would render as a clean bill of health for a check that did not
// execute.
func TestDuplicateDaemons_NoProcessSnapshot_SaysSoRatherThanPassing(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.snapshot = func() (map[int]proctree.Process, error) {
		return nil, errNoDaemon // what Snapshot() does on darwin
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "daemon instances")
	require.Equal(t, StatusWarn, c.Status, "an unrun check must not read as PASS")
	require.Contains(t, c.Detail, "cannot scan processes")
	require.False(t, c.Problem, "the user cannot fix their platform; this is advisory")
}

// "The control socket did not answer" is not evidence about a DIFFERENT
// listener. They are separate binds, and RunDaemon keeps one when the other
// fails — so a live HTTP listener must never be called debris and swept up on
// the strength of the control socket's silence. Same principle as the probe fix:
// I did not get an answer is not nobody is there.
func TestStaleSocket_LiveListenerIsNotStale(t *testing.T) {
	testguard.IsolateTmux(t)

	home := socketTempHome(t)
	sockPath := filepath.Join(home, "daemon-http.sock")
	// A REAL listener, actually accepting connections.
	l, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	opts := testOptionsWithHome(t, home, false)
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			PingErr:          errNoDaemon, // the control socket is silent...
			HTTPSocketPath:   sockPath,
			HTTPSocketExists: true,
			HTTPListening:    daemon.AnswerYes(), // ...but this listener answers.
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.False(t, hasCheck(report, "stale sockets"),
		"a socket something is listening on is not stale, whatever the control socket is doing")
}

// A socket nobody dialed is not claimed either: absence of evidence is not
// evidence of death.
func TestStaleSocket_UnprobedSocketNotClaimed(t *testing.T) {
	testguard.IsolateTmux(t)

	home := socketTempHome(t)
	abandonedSocket(t, filepath.Join(home, "daemon-http.sock"))

	opts := testOptionsWithHome(t, home, false)
	opts.daemonHealth = func() daemon.HealthStatus {
		// Nothing answered the control socket, and the HTTP socket was never
		// probed at all (no path, no dial result).
		return daemon.HealthStatus{PingErr: errNoDaemon}
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.False(t, hasCheck(report, "stale sockets"),
		"without a dial that failed, doctor has not earned the claim")
}

// The name is a convention, not proof: a plain file that borrowed the name is
// not a socket, and telling the user to `af reset` over it would be wrong.
func TestStaleSocket_RegularFileWithSocketName_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "daemon-http.sock"), []byte("not a socket"), 0o600))

	report, err := Run(testOptionsWithHome(t, home, false))
	require.NoError(t, err)
	require.False(t, hasCheck(report, "stale sockets"), "only a real socket counts")
}

func TestStaleSocket_DaemonAnswering_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	home := socketTempHome(t)
	abandonedSocket(t, filepath.Join(home, "daemon.sock"))

	opts := testOptionsWithHome(t, home, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.192")

	report, err := Run(opts)
	require.NoError(t, err)
	require.False(t, hasCheck(report, "stale sockets"), "a socket a daemon answers on is not stale")
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
	ourAutostartUnit(&opts)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: bin}
	}
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Enabled: daemon.AnswerYes(), Active: daemon.AnswerYes()}
	}
	opts.selfBinary = func() (string, error) { return bin, nil }
	opts.binaryCandidates = func() []string { return []string{bin} }
	opts.binaryVersion = func(string) (string, error) { return "1.0.192", nil }

	report, err := Run(opts)
	require.NoError(t, err)

	for _, name := range []string{"daemon version", "autostart path", "autostart supervision"} {
		require.Equal(t, StatusPass, findCheck(t, report, name).Status, "check %q", name)
	}
	for _, name := range []string{"af binaries", "stale sockets"} {
		require.False(t, hasCheck(report, name), "healthy machine must not report %q", name)
	}
	// "daemon instances" is platform-dependent, and honestly so: proctree is
	// /proc-only, so on darwin the scan cannot run and the check says it did not
	// (#1939) rather than passing. Either way it must not make a healthy machine
	// exit nonzero.
	if hasCheck(report, "daemon instances") {
		c := findCheck(t, report, "daemon instances")
		require.Equal(t, StatusWarn, c.Status)
		require.Contains(t, c.Detail, "cannot scan processes",
			"the only reason to report on a healthy machine is being unable to look")
		require.False(t, c.Problem)
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
			require.False(t, c.Actionable, "passing check %q cannot be actionable", c.Name)
		}
	}
}

// Two WARNs, same status, opposite meanings: "no autostart unit installed" is
// advisory and leaves the run healthy, while a stale socket means the daemon is
// broken and drives the nonzero exit. Both carry a remedy, so `remedy != ""`
// cannot tell them apart — only `actionable` can.
func TestRenderJSON_AdvisoryWarnDistinguishableFromActionableWarn(t *testing.T) {
	testguard.IsolateTmux(t)

	home := socketTempHome(t)
	abandonedSocket(t, filepath.Join(home, "daemon-http.sock"))

	opts := testOptionsWithHome(t, home, false)
	report, err := Run(opts)
	require.NoError(t, err)

	payload := BuildJSONReport(report, false, false)
	byName := map[string]JSONCheck{}
	for _, c := range payload.Checks {
		byName[c.Name] = c
	}

	advisory, ok := byName["autostart"]
	require.True(t, ok)
	require.Equal(t, string(StatusWarn), advisory.Status)
	require.NotEmpty(t, advisory.Remedy, "the advisory hint is still worth having")
	require.False(t, advisory.Actionable, "an advisory warn must not read as needing action")

	actionable, ok := byName["stale sockets"]
	require.True(t, ok)
	require.Equal(t, string(StatusWarn), actionable.Status)
	require.True(t, actionable.Actionable, "a stale socket is a real problem")

	// The contract that makes it usable: actionable rows are exactly what makes
	// the run exit nonzero, so the two can never disagree.
	require.Positive(t, payload.Summary.Unresolved)
	anyActionable := false
	for _, c := range payload.Checks {
		if c.Actionable {
			anyActionable = true
		}
	}
	require.True(t, anyActionable, "a nonzero exit must be explained by at least one actionable row")
}

// The converse: a run that exits 0 must carry no actionable row, even though it
// is full of advisory warnings with remedies.
func TestRenderJSON_HealthyRunHasNoActionableRows(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = respondingDaemon("1.0.192")

	report, err := Run(opts)
	require.NoError(t, err)
	require.Zero(t, report.UnresolvedCount())

	payload := BuildJSONReport(report, false, false)
	for _, c := range payload.Checks {
		require.False(t, c.Actionable, "check %q must not be actionable in a healthy run", c.Name)
	}
	require.Zero(t, payload.Summary.Unresolved)
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

// "I did not verify" is not "it is fine". A nil error used to mean both "the
// dial succeeded" and "nobody dialed", and PASS fell out of the second one — a
// fabricated positive about the surface the web UI and every HTTP client use.
func TestHTTPSocket_NeverProbed_IsUnknownNotHealthy(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	opts.daemonHealth = func() daemon.HealthStatus {
		// A daemon answering the control socket, and an HTTP socket nobody
		// probed: HTTPListening is the zero value.
		return daemon.HealthStatus{
			SocketPath: "/fake/daemon.sock", SocketExists: true, DaemonVersion: "1.0.192",
			HTTPSocketPath: "/fake/daemon-http.sock", HTTPSocketExists: true,
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "http socket")
	require.NotEqual(t, StatusPass, c.Status, "an unprobed listener must never read as healthy")
	require.Contains(t, c.Detail, "unknown")
	require.False(t, c.Problem, "we did not observe a fault, so we do not assert one")
}

// A client that says it is NOT a daemon must not be counted as one: the scan's
// answer would be about the wrong population.
func TestDuplicateDaemons_DaemonFalseClientIsNotCounted(t *testing.T) {
	testguard.IsolateTmux(t)

	home := t.TempDir()
	// One real daemon, and one client explicitly opting out.
	real := spawnWithEnv(t, "af", []string{"--daemon"}, map[string]string{"AGENT_FACTORY_HOME": home})
	client := spawnWithEnv(t, "af", []string{"--daemon=false"}, map[string]string{"AGENT_FACTORY_HOME": home})
	stubProcessHomes(t, map[int]string{real.PID: home, client.PID: home})

	report, err := Run(testOptionsWithHome(t, home, false, real.PID, client.PID))
	require.NoError(t, err)

	require.False(t, hasCheck(report, "daemon instances"),
		"one daemon and one --daemon=false client is one daemon, not two")
	require.False(t, hasFinding(report, "foreign-daemon"),
		"nor is the client something to reap")
}
