package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/session"
)

// installArchiveHookScopeShim replaces systemd-run with a recording shim that
// execs the command, and answers systemctl so the teardown stop is a no-op. The
// log path is baked in rather than passed through the environment: the hook's
// child environment is filtered down to an operator-approved set, so a shim
// reading a test variable would see it unset and fail under `set -eu`.
func installArchiveHookScopeShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "systemd-run.log")
	quoted := "'" + strings.ReplaceAll(logPath, "'", `'\''`) + "'"
	script := `#!/bin/sh
set -eu
if [ "${1:-}" = "--help" ]; then
    printf '%s\n' '    --expand-environment=BOOL'
    exit 0
fi
printf '%s\n' "$*" >> ` + quoted + `
while [ "$#" -gt 0 ]; do
    case "$1" in
        --user|--scope|--quiet|--collect|--expand-environment=no) shift ;;
        --unit=*) shift ;;
        --property=*) shift ;;
        --) shift; break ;;
        *) echo "unexpected systemd-run argument: $1" >&2; exit 64 ;;
    esac
done
exec "$@"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(script), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// installArchiveHookScopeShimWithSystemctl is the same shim pair with a scripted
// systemctl, so a test can decide what the manager reports about survivors and
// observe which subcommands ran.
func installArchiveHookScopeShimWithSystemctl(t *testing.T, systemctlBody, systemctlLog string) {
	t.Helper()
	installArchiveHookScopeShim(t)
	dir := strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + systemctlLog + "'\n" + systemctlBody
	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o700))
}

func readArchiveScopeLog(t *testing.T, logPath string) string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read systemd-run log: %v", err)
	}
	return string(raw)
}

// requireLinuxScopes guards the tests whose subject is the transient systemd
// scope. RunningDaemonProcess() is a compile-time false on non-Linux, so these
// would fail on the required macOS job rather than skip (#3650 review). The
// darwin contract — nothing is relocated — is asserted on every platform by
// TestArchiveHookIsNotRelocatedOutsideTheSystemdDaemon.
func requireLinuxScopes(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("transient systemd scopes are Linux-only; the non-Linux contract is asserted by TestArchiveHookIsNotRelocatedOutsideTheSystemdDaemon")
	}
}

// on_archive_command is the other daemon-reachable spawn that skipped the child
// scope helper (#3650). An operator's dependency sweep can be as expensive as
// their build, and it was charged to the daemon unit exactly the same way.
func TestArchiveHookEntersAnUnboundScopeWhenTheDaemonSpawnsIt(t *testing.T) {
	requireLinuxScopes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	logPath := installArchiveHookScopeShim(t)
	t.Setenv(autostartSystemdMarker, autostartUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))

	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, "touch "+marker)
	hookCtx := archiveHookContext(t)
	require.NoError(t, runOnArchiveHook(hookCtx))
	require.FileExists(t, marker, "the hook must actually have run, or this test proves nothing")

	got := readArchiveScopeLog(t, logPath)
	require.NotEmpty(t, got, "daemon-spawned on_archive_command bypassed systemd-run and stayed in the daemon's own cgroup (#3650)")
	require.Contains(t, got, "--user --scope --quiet --collect")
	require.Contains(t, got, "--unit="+systemdunit.HookScopeUnitPrefix(hookCtx.sessionID)+"-",
		"the archive hook must share the session's scope prefix so the pre-rebuild sweep names it too")
	for _, forbidden := range []string{"BindsTo=", "After=", "PartOf="} {
		require.NotContains(t, got, forbidden,
			"a dependency edge to the daemon unit kills a long dependency sweep on every daemon restart (#3650)")
	}
}

// The gate is being the process systemd started, nothing else. A manually
// launched daemon must spawn what it spawns today and must not reach for a user
// manager to stop a scope it never created.
func TestArchiveHookIsNotRelocatedOutsideTheSystemdDaemon(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	logPath := installArchiveHookScopeShim(t)
	t.Setenv(autostartSystemdMarker, "")
	t.Setenv("SYSTEMD_EXEC_PID", "")
	if systemdunit.RunningDaemonProcess() {
		t.Skip("this process really is the daemon; the non-daemon path is not observable here")
	}

	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, "touch "+marker)
	require.NoError(t, runOnArchiveHook(archiveHookContext(t)))
	require.FileExists(t, marker)
	require.Empty(t, readArchiveScopeLog(t, logPath), "a non-systemd daemon relocated its archive hook")
}

// Archive is retryable and it MOVES the tree, so a hook that outlived a previous
// daemon generation must be stopped before another one starts. Running the
// operator's command twice over one tree — the first still writing through its
// old cwd — is the #2770 hazard restated across the restart boundary.
func TestArchiveHookStopsASurvivorFromAPreviousDaemonBeforeRunning(t *testing.T) {
	requireLinuxScopes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	dir := t.TempDir()
	logPath := filepath.Join(dir, "systemctl.log")
	stopped := filepath.Join(dir, "stopped")
	hookCtx := archiveHookContext(t)
	prefix := systemdunit.HookScopeUnitPrefix(hookCtx.sessionID)
	installArchiveHookScopeShimWithSystemctl(t, `case "$*" in
  *" stop "*) : > '`+stopped+`'; exit 0 ;;
esac
if [ -f '`+stopped+`' ]; then exit 0; fi
printf '%s\n' '`+prefix+`-old-0.scope loaded active running Hook'
exit 0
`, logPath)
	t.Setenv(autostartSystemdMarker, autostartUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))

	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, "touch "+marker)
	require.NoError(t, runOnArchiveHook(hookCtx))
	require.FileExists(t, marker)

	log, err := os.ReadFile(logPath)
	require.NoError(t, err, "systemctl was never consulted")
	require.Contains(t, string(log), "stop -- "+prefix+"-old-0.scope",
		"a hook that outlived a previous daemon was not stopped before a second one started")
}

// And fail closed: an archive that does not run its hook is recoverable, two
// concurrent runs over one tree are not.
func TestArchiveHookRefusesWhenASurvivorCannotBeStopped(t *testing.T) {
	requireLinuxScopes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	hookCtx := archiveHookContext(t)
	prefix := systemdunit.HookScopeUnitPrefix(hookCtx.sessionID)
	installArchiveHookScopeShimWithSystemctl(t, `case "$*" in
  *" stop "*) exit 0 ;;
esac
printf '%s\n' '`+prefix+`-old-0.scope loaded active running Hook'
exit 0
`, filepath.Join(t.TempDir(), "systemctl.log"))
	t.Setenv(autostartSystemdMarker, autostartUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))

	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, "touch "+marker)
	err := runOnArchiveHook(hookCtx)
	require.Error(t, err, "a hook survivor that would not stop was ignored")
	require.Contains(t, err.Error(), "refusing to run the on-archive hook")
	require.NoFileExists(t, marker, "the second hook ran anyway, over a tree the first is still using")
}

// The MOVE is the hazard, not only the second hook run: a session with no
// on_archive_command configured still relocates its worktree here, so a
// post-worktree hook that outlived a previous daemon must be stopped even
// though this call will not run anything itself.
func TestArchiveStopsASurvivorEvenWithNoArchiveCommandConfigured(t *testing.T) {
	requireLinuxScopes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	dir := t.TempDir()
	logPath := filepath.Join(dir, "systemctl.log")
	stopped := filepath.Join(dir, "stopped")
	hookCtx := archiveHookContext(t)
	prefix := systemdunit.HookScopeUnitPrefix(hookCtx.sessionID)
	installArchiveHookScopeShimWithSystemctl(t, `case "$*" in
  *" stop "*) : > '`+stopped+`'; exit 0 ;;
esac
if [ -f '`+stopped+`' ]; then exit 0; fi
printf '%s\n' '`+prefix+`-old-0.scope loaded active running Hook'
exit 0
`, logPath)
	t.Setenv(autostartSystemdMarker, autostartUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))

	// No on_archive_command at all.
	require.NoError(t, runOnArchiveHook(hookCtx))

	log, err := os.ReadFile(logPath)
	require.NoError(t, err, "systemctl was never consulted")
	require.Contains(t, string(log), "stop -- "+prefix+"-old-0.scope",
		"a survivor was left running into the worktree the archive is about to move")
}

// Archive must cancel and JOIN the in-process hook runner before it touches the
// tree, and refuse the archive when that cannot be confirmed — not treat it as
// the best-effort hook failure teardown deliberately tolerates. Stopping a live
// runner's scope without cancelling its context makes it read the dead command
// as an ordinary failure and start the NEXT configured command, in a fresh
// scope, into a tree that is about to move (#3650 review).
func TestArchiveRefusesWhenHooksCannotBeConfirmedStopped(t *testing.T) {
	requireLinuxScopes(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "hookworker")

	gw, err := inst.GetGitWorktree()
	require.NoError(t, err)
	prefix := systemdunit.HookScopeUnitPrefix(inst.ID)
	gw.SetHookScopeUnitPrefix(prefix)

	// A manager that reports the scope as alive and never stops it.
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in\n  *\" stop \"*) exit 0 ;;\nesac\nprintf '%s\\n' '" +
		prefix + "-g-0.scope loaded active running Hook'\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "hookworker", RepoID: repoID})
	require.Error(t, err, "archive proceeded while a hook may still have been writing into the tree it was about to move")
	require.Contains(t, err.Error(), "post-worktree hooks could not be confirmed stopped")
	require.Contains(t, err.Error(), "no session teardown was started")
	require.DirExists(t, srcPath, "the worktree was moved despite the refusal")
}

// The move must not happen when the hook's own scope could not be stopped. A
// descendant that called setsid is outside the process group the kill reaches
// but still inside the control group, and `systemctl stop` waits for its job —
// StopScopeUnits passes no --no-block — so a failure is no proof the stop
// completed. Every ORDINARY hook failure is best-effort here and relocates
// anyway, which is why this one has to be a distinguishable safety error
// (#3650 review).
func TestArchiveDoesNotRelocateWhenTheHookScopeCannotBeStopped(t *testing.T) {
	requireLinuxScopes(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	_, srcPath := registerArchivable(t, manager, repoID, repoPath, "scopeworker")

	// Nothing survives from a previous generation (so the pre-run sweeps pass),
	// but the stop of the scope this run creates never completes.
	installArchiveHookScopeShimWithSystemctl(t,
		"case \"$*\" in\n  *\" stop \"*) echo 'Failed to stop unit: Connection timed out' >&2; exit 1 ;;\nesac\nexit 0\n",
		filepath.Join(t.TempDir(), "systemctl.log"))
	t.Setenv(autostartSystemdMarker, autostartUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	writeOnArchiveCommand(t, "true")

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "scopeworker", RepoID: repoID})
	require.Error(t, err, "the worktree was relocated while a hook descendant may still have been writing to it")
	require.ErrorIs(t, err, session.ErrHookTeardownUnconfirmed)
	require.DirExists(t, srcPath, "the worktree moved despite the refusal")
}
