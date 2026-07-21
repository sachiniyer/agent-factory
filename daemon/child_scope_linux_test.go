//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// installBoundScopeShim replaces systemd-run with a recording shim that marks
// the command it eventually execs. The tests below deliberately exercise the
// real watcher/editor spawn paths: a helper-only argv test could stay green if
// either production caller later regressed to exec.Command.
func installBoundScopeShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "systemd-run.log")
	shim := filepath.Join(dir, "systemd-run")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$AF_TEST_SCOPE_LOG"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --user|--scope|--quiet|--collect) shift ;;
        --property=*) shift ;;
        --) shift; break ;;
        *) echo "unexpected systemd-run argument: $1" >&2; exit 64 ;;
    esac
done
export AF_TEST_BOUND_DAEMON_CHILD=1
exec "$@"
`
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatalf("write systemd-run shim: %v", err)
	}
	t.Setenv("AF_TEST_SCOPE_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(autostartSystemdMarker, autostartUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	return logPath
}

func assertBoundScopeInvocation(t *testing.T, logPath, command string) {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("daemon-owned child bypassed systemd-run: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"--user --scope --quiet --collect",
		"--property=BindsTo=" + autostartUnitName,
		"--property=After=" + autostartUnitName,
		"--property=KillMode=control-group",
		"--property=TimeoutStopSec=4s",
		"-- " + command,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("systemd-run invocation %q does not contain %q", strings.TrimSpace(got), want)
		}
	}
}

func TestWatcherSpawnUsesDaemonBoundSystemdScope(t *testing.T) {
	logPath := installBoundScopeShim(t)
	dir := t.TempDir()
	s, rec := newTestSupervisor(t, staticTasks(watchTask(
		"scope001",
		`printf 'scope=%s\n' "${AF_TEST_BOUND_DAEMON_CHILD:-}"`,
		dir,
	)))

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "scoped watcher to exit", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})
	if got := rec.eventsSnapshot(); len(got) != 1 || got[0] != "scope001:scope=1" {
		t.Fatalf("watcher did not execute inside the bound scope shim: events=%v", got)
	}
	assertBoundScopeInvocation(t, logPath, "sh -c")
}

func TestVSCodeSpawnUsesDaemonBoundSystemdScope(t *testing.T) {
	logPath := installBoundScopeShim(t)
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	v := newTestVSCodeSupervisor(t, binary)
	worktree := t.TempDir()

	if _, err := v.ensureServer("scope-editor", worktree); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	assertBoundScopeInvocation(t, logPath, binary)
}
