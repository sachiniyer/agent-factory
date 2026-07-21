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
if [ "${1:-}" = "--help" ]; then
    printf '%s\n' '    --expand-environment=BOOL'
    exit 0
fi
printf '%s\n' "$*" >> "$AF_TEST_SCOPE_LOG"
expand=yes
while [ "$#" -gt 0 ]; do
    case "$1" in
        --user|--scope|--quiet|--collect) shift ;;
        --expand-environment=no) expand=no; shift ;;
        --property=*) shift ;;
        --) shift; break ;;
        *) echo "unexpected systemd-run argument: $1" >&2; exit 64 ;;
    esac
done
export AF_TEST_BOUND_DAEMON_CHILD=1
# systemd expansion sees the whole sh -c argument without understanding shell
# quotes. Emulate the destructive case so this test proves argv preservation,
# rather than merely checking that a flag happens to be present.
if [ "$expand" = yes ] && [ "${1:-}" = sh ] && [ "${2:-}" = -c ]; then
    rewritten="$(printf '%s' "${3:-}" | sed 's/\${AF_SYSTEMD_RUN_LITERAL}/expanded-by-systemd-run/g')"
    exec "$1" "$2" "$rewritten"
fi
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
		"--user --scope --quiet --collect --expand-environment=no",
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
		`printf 'scope=%s literal=%s\n' "${AF_TEST_BOUND_DAEMON_CHILD:-}" '${AF_SYSTEMD_RUN_LITERAL}'`,
		dir,
	)))

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "scoped watcher to exit", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})
	if got := rec.eventsSnapshot(); len(got) != 1 || got[0] != "scope001:scope=1 literal=${AF_SYSTEMD_RUN_LITERAL}" {
		t.Fatalf("watcher argv changed while entering the bound scope: events=%v", got)
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
