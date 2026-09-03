package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

// installScopeShim replaces systemd-run with a recording shim that logs its
// argv and then execs the command. The tests below drive the real hook runner
// rather than a helper's argv: a helper-only test stays green if hooks.go later
// regresses to a raw exec.Command, which is the exact defect #3650 is about.
func installScopeShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "systemd-run.log")
	shim := filepath.Join(dir, "systemd-run")
	// The log path is BAKED IN rather than passed through the environment: the
	// hook runner filters the child environment down to an operator-approved set
	// (sessionenv.Filter), so a shim that read $AF_TEST_SCOPE_LOG would silently
	// see it unset and fail under `set -eu` — a green-looking harness bug that
	// hides the very spawn this test is here to observe.
	script := `#!/bin/sh
set -eu
if [ "${1:-}" = "--help" ]; then
    printf '%s\n' '    --expand-environment=BOOL'
    exit 0
fi
printf '%s\n' "$*" >> ` + shellQuoteForShim(logPath) + `
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
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatalf("write systemd-run shim: %v", err)
	}
	// systemctl is reached on the teardown path once a scope exists. Answer it
	// the way a manager with nothing loaded would, so the shim never turns a
	// stop into a spurious failure.
	systemctl := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write systemctl shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func shellQuoteForShim(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// claimDaemonProcess makes RunningDaemonProcess() answer true for this process,
// which is the one gate that decides whether a spawn is relocated at all.
func claimDaemonProcess(t *testing.T) {
	t.Helper()
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
}

func readScopeLog(t *testing.T, logPath string) string {
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

func runScopedHooks(t *testing.T, sessionID string, cmds []string) (string, *GitWorktree) {
	t.Helper()
	repoPath := freshRepoConfig(t, cmds)
	gw := &GitWorktree{repoPath: repoPath, worktreePath: t.TempDir()}
	gw.SetHookScopeSessionID(sessionID)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gw.hooksCtx = ctx
	gw.hooksCancel = cancel
	done := gw.runHooks()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("post-worktree hooks did not finish")
	}
	return repoPath, gw
}

// requireLinuxScopes guards the tests whose subject is the transient systemd
// scope itself. internal/systemdunit's non-Linux build returns false from
// RunningDaemonProcess() unconditionally, so on the required macOS job these
// would fail on an empty systemd-run log rather than skip — a Linux-only
// mechanism reported as a broken one (#3650 review).
//
// The darwin-side claim is not left uncovered by this: what must hold there is
// that NOTHING is relocated, and TestTUICreatedPostWorktreeHookIsNotRelocated
// asserts exactly that, on every platform.
func requireLinuxScopes(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("transient systemd scopes are Linux-only; the non-Linux contract is asserted by TestTUICreatedPostWorktreeHookIsNotRelocated")
	}
}

// TestDaemonSpawnedPostWorktreeHookEntersAnUnboundScope pins the SHAPE, which is
// the part the design turned on: the bound shape #3650 started from would have
// killed a user's in-flight `make dev_install` on every daemon restart and every
// auto-upgrade. Absence of BindsTo/After/PartOf is therefore an assertion, not
// an omission.
func TestDaemonSpawnedPostWorktreeHookEntersAnUnboundScope(t *testing.T) {
	requireLinuxScopes(t)
	logPath := installScopeShim(t)
	claimDaemonProcess(t)
	marker := filepath.Join(t.TempDir(), "ran")
	const sessionID = "3d3f1b6a-0000-4000-8000-abcdefabcdef"
	runScopedHooks(t, sessionID, []string{fmt.Sprintf("printf ok > %q", marker)})

	got := readScopeLog(t, logPath)
	if got == "" {
		t.Fatal("daemon-spawned post-worktree hook bypassed systemd-run: it is still charged to the daemon's own cgroup (#3650)")
	}
	for _, want := range []string{
		"--user --scope --quiet --collect",
		"--unit=" + systemdunit.HookScopeUnitPrefix(sessionID) + "-",
		".scope ",
		"-- sh -c ",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("systemd-run invocation %q does not contain %q", strings.TrimSpace(got), want)
		}
	}
	for _, forbidden := range []string{"BindsTo=", "After=", "PartOf=", "Requisite="} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("hook scope carries %q: a dependency edge to the daemon unit kills the operator's build on every daemon restart (#3650)", forbidden)
		}
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "ok" {
		t.Fatalf("hook did not run through the scope: data=%q err=%v", data, err)
	}
}

// TestPostWorktreeHookScopeIsRecordedWithTheSession proves the durable handle is
// written the moment a scope EXISTS. A handle recorded only on completion would
// never be recorded in the one case it is for: a daemon that died mid-build.
func TestPostWorktreeHookScopeIsRecordedWithTheSession(t *testing.T) {
	requireLinuxScopes(t)
	installScopeShim(t)
	claimDaemonProcess(t)
	const sessionID = "9a1c2e40-1111-4000-8000-0123456789ab"
	_, gw := runScopedHooks(t, sessionID, []string{"true"})
	want := systemdunit.HookScopeUnitPrefix(sessionID)
	if got := gw.HookScopeUnitPrefix(); got != want {
		t.Fatalf("recorded hook scope prefix = %q, want %q", got, want)
	}
}

// TestTUICreatedPostWorktreeHookIsNotRelocated is the byte-for-byte promise made
// to every non-daemon caller: RunningDaemonProcess() is the whole gate, so a TUI
// or CLI that creates the worktree itself must spawn exactly what it spawns
// today — no systemd-run, and no recorded handle to sweep later.
func TestTUICreatedPostWorktreeHookIsNotRelocated(t *testing.T) {
	logPath := installScopeShim(t)
	t.Setenv(systemdunit.DaemonMarkerEnv, "")
	t.Setenv("SYSTEMD_EXEC_PID", "")
	if systemdunit.RunningDaemonProcess() {
		t.Skip("this process really is the daemon; the non-daemon path is not observable here")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	_, gw := runScopedHooks(t, "5c7d8e90-2222-4000-8000-fedcbafedcba",
		[]string{fmt.Sprintf("printf ok > %q", marker)})

	if got := readScopeLog(t, logPath); got != "" {
		t.Fatalf("non-daemon hook was relocated into a scope: %q", strings.TrimSpace(got))
	}
	if got := gw.HookScopeUnitPrefix(); got != "" {
		t.Fatalf("non-daemon hook recorded a scope handle %q; absence is what makes a legacy record mean today's behaviour", got)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "ok" {
		t.Fatalf("hook did not run: data=%q err=%v", data, err)
	}
}

// TestScopedHookReapsGrandchildHoldingTheCapturePipe is the #610/#769 contract
// re-proven with the scope in the middle. systemd-run --scope EXECs rather than
// forks, so cmd.Process.Pid is still the hook shell and Setpgid still makes it a
// group leader — but that is a measured property of systemd-run, and this is
// what fails if a future systemd (or a shim) ever forks instead: the shell exits
// while a backgrounded grandchild holds the capture pipe, WaitDelay elapses, and
// the process-group SIGKILL must still reach the grandchild.
func TestScopedHookReapsGrandchildHoldingTheCapturePipe(t *testing.T) {
	installScopeShim(t)
	claimDaemonProcess(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// The grandchild inherits stdout/stderr (the capture pipe) and outlives the
	// shell, which is precisely the state hookWaitDelay bounds.
	script := fmt.Sprintf("sleep 30 & printf '%%s' \"$!\" > %q", pidFile)

	start := time.Now()
	runScopedHooks(t, "7b2a4c60-3333-4000-8000-0f0f0f0f0f0f", []string{script})
	elapsed := time.Since(start)

	pid := waitForPidFile(t, pidFile, 3*time.Second)
	if !waitForProcessExit(pid, 5*time.Second) {
		t.Fatalf("backgrounded grandchild pid %d survived a scoped hook — the process group was not killed", pid)
	}
	if elapsed < hookWaitDelay {
		t.Fatalf("scoped hook returned in %s, before WaitDelay %s could elapse; the grandchild never held the capture pipe and this test proved nothing", elapsed, hookWaitDelay)
	}
}
