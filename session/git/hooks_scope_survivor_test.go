//go:build linux

package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

// installSurvivorSystemctl scripts systemctl so a test can decide what the
// manager reports and observe exactly which subcommands ran.
func installSurvivorSystemctl(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "systemctl.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n" + body
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write systemctl shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func worktreeWithRecordedScope(t *testing.T, prefix string) *GitWorktree {
	t.Helper()
	gw := &GitWorktree{repoPath: t.TempDir(), worktreePath: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gw.hooksCtx = ctx
	gw.hooksCancel = cancel
	gw.SetHookScopeUnitPrefix(prefix)
	return gw
}

// A survivor from a previous daemon generation has no pgid, no cmd.Wait and no
// hooksCancel — all three died with that daemon. The recorded scope prefix is
// the only handle left, and the rebuild/remove path is where it has to be used
// (#2770 ordering: the tree must be provably untouched by the old run).
func TestRebuildPathStopsAHookScopeThatOutlivedItsDaemon(t *testing.T) {
	stopped := filepath.Join(t.TempDir(), "stopped")
	logPath := installSurvivorSystemctl(t, `case "$*" in
  *" stop "*) : > '`+stopped+`'; exit 0 ;;
esac
if [ -f '`+stopped+`' ]; then exit 0; fi
printf '%s\n' 'af-hook-sess1-g-0.scope loaded active running Hook'
exit 0
`)
	gw := worktreeWithRecordedScope(t, "af-hook-sess1")
	if err := gw.cancelAndWaitHooks(); err != nil {
		t.Fatalf("cancelAndWaitHooks: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("systemctl was never consulted: %v", err)
	}
	if !strings.Contains(string(log), "stop -- af-hook-sess1-g-0.scope") {
		t.Fatalf("the surviving hook scope was never stopped before the tree was touched:\n%s", log)
	}
}

// Fail CLOSED, exactly as the in-process join already does on timeout: a
// survivor that could not be stopped is a process that may still be writing
// into the tree the caller is about to rebuild or delete.
func TestRebuildPathRefusesWhenASurvivingScopeWillNotStop(t *testing.T) {
	installSurvivorSystemctl(t, `case "$*" in
  *" stop "*) exit 0 ;;
esac
printf '%s\n' 'af-hook-sess1-g-0.scope loaded active running Hook'
exit 0
`)
	gw := worktreeWithRecordedScope(t, "af-hook-sess1")
	err := gw.cancelAndWaitHooks()
	if err == nil {
		t.Fatal("a hook scope that outlived its stop was treated as gone")
	}
	if !strings.Contains(err.Error(), "refusing to modify the worktree") {
		t.Fatalf("unexpected refusal message: %v", err)
	}
}

// An unreachable user manager is UNKNOWN, not "no survivors". A client that lost
// the bus, or never had XDG_RUNTIME_DIR, gets exactly this error while the
// manager and the hook inside it keep running — so proceeding would hand the
// rebuild a tree a live hook is still writing to.
func TestRebuildPathRefusesWhenTheUserManagerCannotBeQueried(t *testing.T) {
	installSurvivorSystemctl(t, "echo 'Failed to connect to bus: No such file or directory' >&2\nexit 1\n")
	gw := worktreeWithRecordedScope(t, "af-hook-sess1")
	err := gw.cancelAndWaitHooks()
	if err == nil {
		t.Fatal("a manager that could not be queried was read as proof that no hook survives")
	}
	if !strings.Contains(err.Error(), "refusing to modify the worktree") {
		t.Fatalf("unexpected refusal message: %v", err)
	}
}

// The other half of the same rule: a session that never entered a scope, on a
// machine with no systemd at all, must not be wedged by that refusal. Absence of
// a recorded handle is what keeps the sweep — and therefore the refusal — off
// every pre-#3650 record.
func TestNoRecordedScopeIsNotWedgedByAnUnreachableManager(t *testing.T) {
	installSurvivorSystemctl(t, "echo 'Failed to connect to bus: No such file or directory' >&2\nexit 1\n")
	gw := worktreeWithRecordedScope(t, "")
	if err := gw.cancelAndWaitHooks(); err != nil {
		t.Fatalf("a session with no recorded scope was wedged by an unreachable manager: %v", err)
	}
}

// The refinement on the accepted design: a survivor over an INTACT tree is left
// to finish. Nothing outside cancelAndWaitHooks may stop it, and a record with
// no recorded prefix — a legacy session, or one whose hooks never entered a
// scope — must not reach the manager at all.
func TestNoRecordedScopeNeverConsultsTheManager(t *testing.T) {
	logPath := installSurvivorSystemctl(t, "exit 0\n")
	gw := worktreeWithRecordedScope(t, "")
	if err := gw.cancelAndWaitHooks(); err != nil {
		t.Fatalf("cancelAndWaitHooks: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		raw, _ := os.ReadFile(logPath)
		t.Fatalf("a session with no recorded scope reached the systemd user manager:\n%s", raw)
	}
}

// The persisted handle can be MISSING for a scope that exists: the hook
// goroutine records it in memory and the instance reaches disk on some later
// checkpoint, so a daemon that died in that window left a live survivor behind a
// record saying no scope ever existed. The sweep must not depend on that write.
func TestRebuildPathStopsASurvivorWhoseHandleWasNeverPersisted(t *testing.T) {
	const sessionID = "4f7e2a10-5555-4000-8000-abcabcabcabc"
	prefix := systemdunit.HookScopeUnitPrefix(sessionID)
	stopped := filepath.Join(t.TempDir(), "stopped")
	logPath := installSurvivorSystemctl(t, `case "$*" in
  *" stop "*) : > '`+stopped+`'; exit 0 ;;
esac
if [ -f '`+stopped+`' ]; then exit 0; fi
printf '%s\n' '`+prefix+`-g-0.scope loaded active running Hook'
exit 0
`)
	claimDaemonProcess(t)
	gw := worktreeWithRecordedScope(t, "")
	gw.SetHookScopeSessionID(sessionID)
	if got := gw.HookScopeUnitPrefix(); got != "" {
		t.Fatalf("this test requires an unpersisted handle, got %q", got)
	}

	if err := gw.cancelAndWaitHooks(); err != nil {
		t.Fatalf("cancelAndWaitHooks: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("systemctl was never consulted: %v", err)
	}
	if !strings.Contains(string(log), "stop -- "+prefix+"-g-0.scope") {
		t.Fatalf("a survivor with no persisted handle was never stopped:\n%s", log)
	}
}

// Same gap for the fallback identity a hook run with no session id uses. It is
// not derivable from the session, so the worktree path is the only name it ever
// had — and the sweep has to look for it too.
func TestRebuildPathStopsASurvivorNamedByTheWorktreeFallbackIdentity(t *testing.T) {
	worktree := t.TempDir()
	prefix := systemdunit.HookScopeUnitPrefix(worktreePathScopeIdentity(worktree))
	stopped := filepath.Join(t.TempDir(), "stopped")
	logPath := installSurvivorSystemctl(t, `case "$*" in
  *" stop "*) : > '`+stopped+`'; exit 0 ;;
esac
if [ -f '`+stopped+`' ]; then exit 0; fi
printf '%s\n' '`+prefix+`-g-0.scope loaded active running Hook'
exit 0
`)
	claimDaemonProcess(t)
	gw := worktreeWithRecordedScope(t, "")
	gw.worktreePath = worktree

	if err := gw.cancelAndWaitHooks(); err != nil {
		t.Fatalf("cancelAndWaitHooks: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("systemctl was never consulted: %v", err)
	}
	if !strings.Contains(string(log), "stop -- "+prefix+"-g-0.scope") {
		t.Fatalf("a survivor named by the fallback identity was never stopped:\n%s", log)
	}
}

// hookStopTimeout fails CLOSED, so it has to outlast everything a normally
// progressing teardown spends or it refuses rebuilds that were about to succeed.
// Two serial costs sit under it and they ADD; the arithmetic is easy to break by
// changing either one, so assert it rather than re-derive it (#3650 review).
func TestHookStopTimeoutOutlastsWaitDelayPlusScopeShutdown(t *testing.T) {
	scopeStop, err := time.ParseDuration(systemdunit.HookScopeStopTimeout)
	if err != nil {
		t.Fatalf("HookScopeStopTimeout %q is not a duration: %v", systemdunit.HookScopeStopTimeout, err)
	}
	floor := hookWaitDelay + scopeStop
	if hookStopTimeout <= floor {
		t.Fatalf("hookStopTimeout %s must exceed hookWaitDelay %s + scope shutdown %s = %s; below that sum, a hook with a SIGTERM-ignoring descendant makes the rebuild REFUSE instead of waiting out a teardown that was progressing",
			hookStopTimeout, hookWaitDelay, scopeStop, floor)
	}
}

// Cancelling must stop the RUN, not merely the command that is in flight. This
// is the invariant the archive path now depends on: stopping a live runner's
// scope without cancelling its context makes it read the dead command as an
// ordinary failure and start the NEXT configured command — in a fresh scope,
// into a tree the caller is about to move (#3650 review).
func TestCancelAndJoinHooksEndsTheRunBeforeTheNextCommand(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first-started")
	second := filepath.Join(dir, "second-started")
	repoPath := freshRepoConfig(t, []string{
		fmt.Sprintf("printf started > %q; sleep 60", first),
		fmt.Sprintf("printf started > %q", second),
	})

	gw := &GitWorktree{repoPath: repoPath, worktreePath: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gw.hooksCtx = ctx
	gw.hooksCancel = cancel
	gw.hooksDone = gw.runHooks()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(first); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("the first hook never started, so this test proves nothing: %v", err)
	}

	if err := gw.CancelAndJoinHooks(); err != nil {
		t.Fatalf("CancelAndJoinHooks: %v", err)
	}
	if _, err := os.Stat(second); err == nil {
		t.Fatal("the run advanced to the next command after cancellation; a fresh hook would start into a tree the caller is about to rebuild or move")
	}
}

// startStubHookLauncher starts a live process that looks EXACTLY like a
// systemd-run which has not registered its scope yet: argv[0] is systemd-run and
// its argv carries --unit=<unit>.
//
// It is a stub and not a real systemd-run on purpose. The window under test is
// the sub-second interval between systemd-run's execve and its
// StartTransientUnit reply; a test that tried to catch a real launcher inside it
// would be a coin flip about whether the defect was even reachable on that run.
//
// The argv[0] spoof reproduces production rather than faking it: exec.Command
// sets Args[0] to the NAME it was given, so the real launcher reports
// argv[0]="systemd-run" in /proc/<pid>/cmdline for the same reason this does.
func startStubHookLauncher(t *testing.T, unit, body string) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no sh to build a stub launcher from: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "launcher.pid")
	cmd := &exec.Cmd{
		Path: shell,
		Args: []string{
			"systemd-run", "-c", fmt.Sprintf("printf '%%s' \"$$\" > %q; %s", pidFile, body),
			"--user", "--scope", "--quiet", "--collect", "--unit=" + unit, "--", "sh", "-c", "make dev_install",
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
	// Until execve completes the child's /proc entry still carries the TEST
	// binary's argv, so the sweep must not start before the pid file appears.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stub launcher never published its pid to %s", pidFile)
}

// #3667, at the place the hazard lands. A daemon dies between systemd-run's
// execve and its StartTransientUnit reply, so its replacement sweeps a prefix
// whose unit DOES NOT EXIST YET and gets an empty answer from the manager. That
// answer is not proof of anything: the launcher is alive, and the moment it
// registers it execs the operator's command with the cwd this path is about to
// rebuild or move.
//
// The launcher exits on its own here, so the claim is that the rebuild path
// WAITED for it — not that it refuses forever.
func TestRebuildPathWaitsForALauncherThatHasNotRegisteredItsScope(t *testing.T) {
	installSurvivorSystemctl(t, "exit 0\n") // the manager lists nothing: this IS the window
	const prefix = "af-hook-sess3667"
	const hold = 2 * time.Second
	startStubHookLauncher(t, prefix+"-g0-0.scope", "sleep 2")

	gw := worktreeWithRecordedScope(t, prefix)
	start := time.Now()
	err := gw.cancelAndWaitHooks()
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("cancelAndWaitHooks refused after the launcher was gone: %v", err)
	}
	if elapsed < hold {
		t.Fatalf("the rebuild path proceeded after %s while a systemd-run for %s-* was live and unregistered; "+
			"the manager's silence was read as proof no hook survives, and the tree is about to be rebuilt under one (#3667)", elapsed, prefix)
	}
}
