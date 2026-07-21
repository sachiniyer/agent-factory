//go:build linux

package integration_test

import (
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
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const systemdLifecycleTestEnv = "AF_SYSTEMD_LIFECYCLE_TEST"

// TestAbruptDaemonFailureReapsOwnedChildrenAndPreservesTmux is the real-systemd
// #2284 boundary test. It is destructive to Agent Factory's fixed user-unit
// name, so it runs only on an explicitly prepared ephemeral CI runner. The
// ordinary suite skips it even inside the testbox (which has no user manager).
//
// One TERM-ignoring watcher tree makes the old failure observable: with the
// unit's necessary KillMode=process, a direct child survives the daemon's
// SIGKILL and overlaps the watcher started by systemd's replacement. The fixed
// path puts that tree in a BindsTo+After scope, so systemd reaps the whole old
// scope before it starts the replacement daemon. At the same time, the tmux
// server and pane must retain their exact PIDs across the failure.
func TestAbruptDaemonFailureReapsOwnedChildrenAndPreservesTmux(t *testing.T) {
	if os.Getenv(systemdLifecycleTestEnv) != "1" || os.Getenv("CI") != "true" {
		t.Skip("real systemd lifecycle test requires an explicitly prepared ephemeral CI runner")
	}
	requireTool(t, "systemctl")
	requireTool(t, "systemd-run")
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("tmux is required: %v", err)
	}
	if out, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
		t.Fatalf("the opted-in runner has no reachable systemd user manager: %v\n%s", err, out)
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("resolve user config dir: %v", err)
	}
	unitPath := filepath.Join(configDir, "systemd", "user", systemdunit.DaemonUnitName)
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("refusing to replace a pre-existing %s on the CI runner (stat err=%v)", unitPath, err)
	}

	h := newHarness(t)
	// A unit does not inherit the test process's TMUX_TMPDIR. Put a tmux shim
	// first in the PATH captured by the unit so both the daemon and this test use
	// one explicitly named, throwaway socket. Cleanup therefore never needs a
	// bare kill-server.
	tmuxDir := testguard.SocketTempDir(t)
	tmuxSocket := filepath.Join(tmuxDir, "tmux.sock")
	shimDir := t.TempDir()
	tmuxShim := filepath.Join(shimDir, "tmux")
	writeFile(t, tmuxShim, "#!/bin/sh\nexec "+shellSingleQuote(realTmux)+" -S "+shellSingleQuote(tmuxSocket)+" \"$@\"\n", 0o700)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	watchRootLog := filepath.Join(h.home, "watch-roots")
	watchChildLog := filepath.Join(h.home, "watch-children")
	watchScript := filepath.Join(h.home, "stubborn-watch.sh")
	writeFile(t, watchScript, fmt.Sprintf(`#!/bin/sh
set -eu
trap '' TERM
printf '%%s\n' "$$" >> %s
sh -c 'trap "" TERM; while :; do sleep 600; done' &
child=$!
printf '%%s\n' "$child" >> %s
wait "$child"
`, shellSingleQuote(watchRootLog), shellSingleQuote(watchChildLog)), 0o700)
	writeTasksFile(t, h.home, []map[string]interface{}{
		{
			"id":           "scope2284",
			"name":         "scope lifecycle",
			"prompt":       "",
			"watch_cmd":    shellSingleQuote(watchScript),
			"project_path": h.repo,
			"program":      tmux.ProgramClaude,
			"enabled":      true,
			"created_at":   time.Now().Format(time.RFC3339Nano),
		},
	})

	// Register the safety cleanup before installing anything. It names only the
	// fixed unit this test first proved absent, the private AF home, and the
	// explicit tmux socket above.
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(h.home, "tasks.json"), []byte("[]\n"), 0o600)
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdunit.DaemonUnitName).Run()
		_ = os.Remove(unitPath)
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		_ = exec.Command(realTmux, "-S", tmuxSocket, "kill-server").Run()
	})

	h.run("daemon", "install")
	waitUntil(t, 15*time.Second, "initial watcher tree", func() bool {
		return len(readPIDLog(watchRootLog)) == 1 && len(readPIDLog(watchChildLog)) == 1
	})
	oldRoots := readPIDLog(watchRootLog)
	oldChildren := readPIDLog(watchChildLog)
	oldWatchRoot, oldWatchChild := oldRoots[0], oldChildren[0]
	if !pidAlive(oldWatchRoot) || !pidAlive(oldWatchChild) {
		t.Fatalf("initial watcher tree is not alive: root=%d child=%d", oldWatchRoot, oldWatchChild)
	}

	watchScope := processUnitComponent(t, oldWatchRoot, ".scope")
	show := runExternal(t, "", "systemctl", "--user", "show", watchScope,
		"-p", "BindsTo", "-p", "After", "-p", "KillMode", "-p", "TimeoutStopUSec")
	for _, dependency := range []string{"BindsTo", "After"} {
		if !systemdPropertyHasWord(show, dependency, systemdunit.DaemonUnitName) {
			t.Fatalf("watcher scope %s %s does not include %s:\n%s",
				watchScope, dependency, systemdunit.DaemonUnitName, show)
		}
	}
	for _, want := range []string{
		"KillMode=control-group",
		"TimeoutStopUSec=4s",
	} {
		if !strings.Contains(show, want) {
			t.Fatalf("watcher scope %s is missing %q:\n%s", watchScope, want, show)
		}
	}

	created := h.createSession("failure-survivor")
	if created.TmuxName == "" {
		t.Fatal("daemon-created session reported no tmux name")
	}
	oldServerPID, oldPanePID := tmuxProcessIDs(t, created.TmuxName)
	if unit := processUnitComponent(t, oldServerPID, ".scope"); unit == watchScope {
		t.Fatalf("tmux server and watcher share a kill domain %s", unit)
	}

	oldDaemon := readDaemonPID(t, h.home)
	mainPIDText := strings.TrimSpace(runExternal(t, "", "systemctl", "--user", "show",
		systemdunit.DaemonUnitName, "-p", "MainPID", "--value"))
	mainPID, err := strconv.Atoi(mainPIDText)
	if err != nil || mainPID != oldDaemon {
		t.Fatalf("serving daemon pid=%d, systemd MainPID=%q", oldDaemon, mainPIDText)
	}
	if err := syscall.Kill(oldDaemon, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL disposable daemon pid %d: %v", oldDaemon, err)
	}

	// Observe continuously, not just at the end: a replacement that briefly
	// overlaps the old watcher and cleans it later still violates the invariant.
	overlapped := false
	var newDaemon, newWatchRoot, newWatchChild int
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		roots := readPIDLog(watchRootLog)
		children := readPIDLog(watchChildLog)
		if len(roots) >= 2 {
			newWatchRoot = roots[1]
		}
		if len(children) >= 2 {
			newWatchChild = children[1]
		}
		if newWatchRoot > 1 && (pidAlive(oldWatchRoot) || pidAlive(oldWatchChild)) {
			overlapped = true
		}
		if pid, ok := daemonPID(h.home); ok && pid != oldDaemon && pidAlive(pid) {
			newDaemon = pid
		}
		if newDaemon > 1 && newWatchRoot > 1 && newWatchChild > 1 &&
			pidAlive(newWatchRoot) && pidAlive(newWatchChild) &&
			!pidAlive(oldWatchRoot) && !pidAlive(oldWatchChild) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if overlapped {
		t.Fatalf("replacement watcher pid %d started while old tree %d/%d was still alive", newWatchRoot, oldWatchRoot, oldWatchChild)
	}
	if newDaemon <= 1 || newWatchRoot <= 1 || newWatchChild <= 1 {
		t.Fatalf("replacement did not become healthy: daemon=%d watcher=%d/%d roots=%v children=%v",
			newDaemon, newWatchRoot, newWatchChild, readPIDLog(watchRootLog), readPIDLog(watchChildLog))
	}

	newServerPID, newPanePID := tmuxProcessIDs(t, created.TmuxName)
	if newServerPID != oldServerPID || newPanePID != oldPanePID {
		t.Fatalf("abrupt daemon failure replaced the live tmux tree: server %d -> %d, pane %d -> %d",
			oldServerPID, newServerPID, oldPanePID, newPanePID)
	}
	if roots := readPIDLog(watchRootLog); len(roots) != 2 {
		t.Fatalf("watcher generations=%v, want exactly old+replacement", roots)
	}

	// Leave normal teardown observable too; the fallback cleanup above remains
	// for every earlier fatal path.
	h.run("tasks", "remove", "scope2284")
	h.run("sessions", "kill", "failure-survivor")
	h.run("daemon", "uninstall")
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readPIDLog(path string) []int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(field)
		if err == nil && pid > 1 {
			pids = append(pids, pid)
		}
	}
	return pids
}

func systemdPropertyHasWord(show, property, want string) bool {
	prefix := property + "="
	for _, line := range strings.Split(show, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		for _, value := range strings.Fields(strings.TrimPrefix(line, prefix)) {
			if value == want {
				return true
			}
		}
	}
	return false
}

func processUnitComponent(t *testing.T, pid int, suffix string) string {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatalf("read cgroup for pid %d: %v", pid, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, component := range strings.Split(parts[2], "/") {
			if strings.HasSuffix(component, suffix) {
				return component
			}
		}
	}
	t.Fatalf("pid %d cgroup has no %s unit component: %s", pid, suffix, raw)
	return ""
}
