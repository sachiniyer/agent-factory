package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
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
	waitForRestartDaemonReady(t, "", managerLog, restartReadinessContext{stage: "install", home: h.home, restartOutput: "not_run"})

	created := h.createSession("restart-survivor")
	if created.TmuxName == "" {
		t.Fatal("daemon-created session reported no tmux name")
	}
	t.Setenv("AF_FAKE_TMUX_SESSION", created.TmuxName)

	serverPID, panePID := tmuxProcessIDs(t, created.TmuxName)
	restartAndAssertTmuxPIDs(t, h, "first restart (generated unit)", created.TmuxName, serverPID, panePID, managerLog)

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

	restartAndAssertTmuxPIDs(t, h, "second restart (forced control-group)", created.TmuxName, serverPID, panePID, managerLog)
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
	if got := strings.Count(string(log), "reset failed state"); got != 3 {
		t.Fatalf("install and both restarts must clear a prior start-limit failure; reset count=%d log:\n%s", got, log)
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

func restartAndAssertTmuxPIDs(t *testing.T, h *harness, stage, name string, wantServerPID, wantPanePID int, managerLog string) {
	t.Helper()
	detail := restartReadinessContext{stage: stage, home: h.home, tmuxName: name, restartOutput: "not_run"}
	before := waitForRestartDaemonReady(t, "", managerLog, detail)
	detail.restartOutput = h.run("daemon", "restart")
	if strings.TrimSpace(detail.restartOutput) != "daemon restarted" {
		log, _ := os.ReadFile(managerLog)
		t.Fatalf("%s: expected a restart, not a no-daemon no-op; output=%q\nmanager log:\n%s", stage, detail.restartOutput, log)
	}
	waitForRestartDaemonReady(t, before.BootID, managerLog, detail)

	gotServerPID, gotPanePID := tmuxProcessIDs(t, name)
	if gotServerPID != wantServerPID || gotPanePID != wantPanePID {
		log, _ := os.ReadFile(managerLog)
		t.Fatalf("daemon restart replaced the live tmux process tree: server pid %d -> %d, pane pid %d -> %d\nmanager log:\n%s",
			wantServerPID, gotServerPID, wantPanePID, gotPanePID, log)
	}
}

// The PID file can still name the departing daemon after its socket closes.
// Waiting for that PID and the surviving tmux session can let the next restart
// run during the handoff gap, where a correct no-daemon no-op skips the legacy
// unit migration (#4189). Require a ready responder from a NEW boot.
func waitForRestartDaemonReady(t *testing.T, previousBootID, managerLog string, detail restartReadinessContext) daemon.HealthStatus {
	t.Helper()
	var last daemon.HealthStatus
	var pid int
	var pidValid bool
	pidLive, tmuxExists := "not_checked", "not_checked"
	waitUntil(t, 10*time.Second, detail.stage+" to report a ready daemon after boot "+previousBootID, func() bool {
		last = daemon.Health()
		// Supplemental observations retain the old PID/tmux short-circuit order.
		// They do not gate the handoff: its readiness predicate below is unchanged.
		pid, pidValid = daemonPID(detail.home)
		pidLive, tmuxExists = "not_checked", "not_checked"
		if pidValid {
			alive := pidAlive(pid)
			pidLive = fmt.Sprint(alive)
			if alive && detail.tmuxName != "" {
				tmuxExists = fmt.Sprint(tmuxSessionExists(detail.tmuxName))
			}
		}
		return last.PingErr == nil && last.Phase == daemon.DaemonPhaseReady &&
			last.BootID != "" && last.BootID != previousBootID &&
			last.ServingPID > 1 && last.ServingPID == last.PIDFilePID
	}, func() {
		// Report the retained observations, not a new post-timeout probe.
		log, logErr := os.ReadFile(managerLog)
		t.Logf("%s: last handoff observation after boot %q: ping=%v phase=%s boot=%q servingPID=%d recordedPID=%d\n"+
			"supplemental observations: pid=%d pid_file_valid=%t pid_alive=%s tmux_session=%q tmux_exists=%s; restart_output=%q; manager_log_error=%v\nmanager log:\n%s",
			detail.stage, previousBootID, last.PingErr, last.Phase, last.BootID, last.ServingPID, last.PIDFilePID,
			pid, pidValid, pidLive, detail.tmuxName, tmuxExists, detail.restartOutput, logErr, log)
	})
	return last
}

type restartReadinessContext struct {
	stage         string
	home          string
	tmuxName      string
	restartOutput string
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
manager_pid_file="${AF_FAKE_MANAGER_LOG}.pid"

start_daemon() {
    sh -c 'AGENT_FACTORY_SYSTEMD_UNIT=agent-factory-daemon.service; SYSTEMD_EXEC_PID=$$; export AGENT_FACTORY_SYSTEMD_UNIT SYSTEMD_EXEC_PID; exec "$1" --daemon' fake-systemd "$AF_FAKE_AF_BIN" >>"$AF_FAKE_MANAGER_LOG" 2>&1 &
    printf '%s\n' "$!" >"$manager_pid_file"
    printf 'started daemon pid=%s\n' "$!" >>"$AF_FAKE_MANAGER_LOG"
}

case "${1:-}" in
    daemon-reload)
        exit 0
        ;;
    reset-failed)
        printf 'reset failed state\n' >>"$AF_FAKE_MANAGER_LOG"
        exit 0
        ;;
    enable)
        start_daemon
        exit 0
        ;;
    restart)
        # A real service manager serializes stop completion before start. Socket
        # closure alone is too early: the old daemon still saves state and holds
        # its home lock. Track our own child rather than its disappearing PID file.
        old_pid="$(cat "$manager_pid_file")"
        attempts=0
        while [ "$attempts" -lt 200 ]; do
            state="$(ps -o stat= -p "$old_pid" 2>/dev/null || true)"
            case "$state" in
                ""|Z*) break ;;
            esac
            attempts=$((attempts + 1))
            sleep 0.05
        done
        if [ "$attempts" -eq 200 ]; then
            printf 'previous daemon pid=%s did not exit before restart\n' "$old_pid" >>"$AF_FAKE_MANAGER_LOG"
            exit 1
        fi
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
