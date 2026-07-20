package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestDaemonRestartPreservesDaemonSpawnedTmux is the #2176 regression at the
// lifecycle boundary. It runs only in the disposable testbox (or ephemeral CI):
// a fake user service manager gives us deterministic cgroup semantics without
// granting a container access to the host's real cgroups or systemd manager.
// The daemon, CLI restart, tmux server, pane, unit install, and RPC shutdown are
// all real.
//
// The fake manager models the two facts that matter:
//   - control-group kills a tmux server inherited from the daemon service;
//   - a server launched through systemd-run --scope is outside that cgroup.
//
// Comparing the server and pane PIDs before and after each restart is stronger
// than checking only the session name: lost-restore can recreate the same name
// after killing the original pane, which is the outage this test must catch.
func TestDaemonRestartPreservesDaemonSpawnedTmux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("#2176 exercises the Linux/systemd unit; Darwin is covered by the platform-specific launch wrapper tests")
	}
	if !disposableLifecycleEnvironment() {
		t.Skip("destructive daemon lifecycle regression runs only in the container fence or ephemeral CI")
	}

	fakeBin := t.TempDir()
	managerLog := filepath.Join(t.TempDir(), "fake-systemd.log")
	writeScript(t, filepath.Join(fakeBin, "systemctl"), fakeSystemctl)
	writeScript(t, filepath.Join(fakeBin, "systemd-run"), fakeSystemdRun)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))
	t.Setenv("AF_FAKE_MANAGER_LOG", managerLog)

	h := newHarness(t)
	t.Setenv("AF_FAKE_AF_BIN", h.bin)

	h.run("daemon", "install")
	ready := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pid, ok := daemonPID(h.home)
		if ok && pidAlive(pid) {
			info, err := os.Stat(filepath.Join(h.home, "daemon.sock"))
			if err == nil && info.Mode()&os.ModeSocket != 0 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		log, _ := os.ReadFile(managerLog)
		t.Fatalf("fake-systemd daemon did not become ready:\n%s", log)
	}

	created := h.createSession("restart-survivor")
	if created.TmuxName == "" {
		t.Fatal("daemon-created session reported no tmux name")
	}
	t.Setenv("AF_FAKE_TMUX_SESSION", created.TmuxName)

	serverPID, panePID := tmuxProcessIDs(t, created.TmuxName)
	restartAndAssertTmuxPIDs(t, h, created.TmuxName, serverPID, panePID, managerLog)

	// Fault-inject the old unit policy after proving the generated unit is safe.
	// The second restart can survive only if the daemon originally created the
	// tmux server through the transient scope, independently of KillMode.
	unitPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", "agent-factory-daemon.service")
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read installed unit: %v", err)
	}
	if !strings.Contains(string(unit), "KillMode=process\n") {
		t.Fatalf("installed unit does not protect existing daemon-owned servers with KillMode=process:\n%s", unit)
	}
	unsafeUnit := strings.Replace(string(unit), "KillMode=process\n", "KillMode=control-group\n", 1)
	if err := os.WriteFile(unitPath, []byte(unsafeUnit), 0644); err != nil {
		t.Fatalf("fault-inject control-group unit: %v", err)
	}
	t.Setenv("AF_FAKE_FORCE_CONTROL_GROUP", "1")

	restartAndAssertTmuxPIDs(t, h, created.TmuxName, serverPID, panePID, managerLog)
	refreshedUnit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read refreshed unit: %v", err)
	}
	if !strings.Contains(string(refreshedUnit), "KillMode=process\n") {
		t.Fatalf("pre-restart migration did not repair the legacy unit:\n%s", refreshedUnit)
	}
	log, err := os.ReadFile(managerLog)
	if err != nil {
		t.Fatalf("read fake manager log: %v", err)
	}
	if !strings.Contains(string(log), "preserved scoped tmux server") {
		t.Fatalf("forced control-group fault was not demonstrably survived; manager log:\n%s", log)
	}
}

func disposableLifecycleEnvironment() bool {
	if os.Getenv("CI") == "true" {
		return true
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

func daemonPID(home string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(home, "daemon.pid"))
	if err != nil {
		return 0, false
	}
	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil || pid <= 1 {
		return 0, false
	}
	return pid, true
}

func tmuxProcessIDs(t *testing.T, name string) (serverPID, panePID int) {
	t.Helper()
	out := runExternal(t, "", "tmux", "display-message", "-p", "-t", "="+name+":", "#{pid} #{pane_pid}")
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d %d", &serverPID, &panePID); err != nil {
		t.Fatalf("parse tmux server/pane pids from %q: %v", out, err)
	}
	if serverPID <= 1 || panePID <= 1 {
		t.Fatalf("unsafe tmux process ids: server=%d pane=%d", serverPID, panePID)
	}
	return serverPID, panePID
}

func restartAndAssertTmuxPIDs(t *testing.T, h *harness, name string, wantServerPID, wantPanePID int, managerLog string) {
	t.Helper()
	h.run("daemon", "restart")
	waitUntil(t, 10*time.Second, "daemon restart to restore control-plane readiness", func() bool {
		pid, ok := daemonPID(h.home)
		return ok && pidAlive(pid) && tmuxSessionExists(name)
	})

	gotServerPID, gotPanePID := tmuxProcessIDs(t, name)
	if gotServerPID != wantServerPID || gotPanePID != wantPanePID {
		log, _ := os.ReadFile(managerLog)
		t.Fatalf("daemon restart replaced the live tmux process tree: server pid %d -> %d, pane pid %d -> %d\nmanager log:\n%s",
			wantServerPID, gotServerPID, wantPanePID, gotPanePID, log)
	}
}

const fakeSystemdRun = `
while [ "$#" -gt 0 ]; do
    case "$1" in
        --user|--scope|--quiet|--collect|--same-dir) shift ;;
        --) shift; break ;;
        *) break ;;
    esac
done
export AF_FAKE_OUTSIDE_DAEMON_CGROUP=1
exec "$@"
`

const fakeSystemctl = `
if [ "${1:-}" = "--user" ]; then
    shift
fi

unit_path="${XDG_CONFIG_HOME}/systemd/user/agent-factory-daemon.service"

start_daemon() {
    sh -c 'AGENT_FACTORY_SYSTEMD_UNIT=agent-factory-daemon.service; SYSTEMD_EXEC_PID=$$; export AGENT_FACTORY_SYSTEMD_UNIT SYSTEMD_EXEC_PID; exec "$1" --daemon' fake-systemd "$AF_FAKE_AF_BIN" >>"$AF_FAKE_MANAGER_LOG" 2>&1 &
    printf 'started daemon pid=%s\n' "$!" >>"$AF_FAKE_MANAGER_LOG"
}

case "${1:-}" in
    daemon-reload)
        exit 0
        ;;
    enable)
        start_daemon
        exit 0
        ;;
    restart)
        if [ "${AF_FAKE_FORCE_CONTROL_GROUP:-}" = "1" ] || ! grep -q '^KillMode=process$' "$unit_path"; then
            server_pid="$(tmux display-message -p -t "=${AF_FAKE_TMUX_SESSION}:" '#{pid}' 2>/dev/null || true)"
            if [ -n "$server_pid" ] && [ "$server_pid" -gt 1 ]; then
                if tr '\000' '\n' <"/proc/$server_pid/environ" | grep -q '^AF_FAKE_OUTSIDE_DAEMON_CGROUP=1$'; then
                    printf 'preserved scoped tmux server pid=%s under control-group restart\n' "$server_pid" >>"$AF_FAKE_MANAGER_LOG"
                else
                    printf 'killed daemon-cgroup tmux server pid=%s under control-group restart\n' "$server_pid" >>"$AF_FAKE_MANAGER_LOG"
                    kill -KILL "$server_pid"
                fi
            fi
        fi
        start_daemon
        exit 0
        ;;
    *)
        printf 'unexpected systemctl args: %s\n' "$*" >>"$AF_FAKE_MANAGER_LOG"
        exit 1
        ;;
esac
`
