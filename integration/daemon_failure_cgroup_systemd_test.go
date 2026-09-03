//go:build linux

package integration_test

import (
	"encoding/json"
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
	"github.com/sachiniyer/agent-factory/task"
)

const (
	systemdLifecycleTestEnv = "AF_SYSTEMD_LIFECYCLE_TEST"

	// watchTaskID names the one watch task this test arms. It is written into
	// the fixture store, read back through `af tasks list`, and removed at the
	// end, so it is a value rather than the same literal in four places.
	watchTaskID = "scope2284"
)

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
# PROBE ONLY (#3820): force the slow path this test's budget has to survive.
# Every watcher generation delays its first PID-log write by 20s, which is what
# a loaded runner does to the unit-activation -> daemon-start -> arm -> spawn
# chain that the old single wall clock covered.
sleep 20
printf '%%s\n' "$$" >> %s
sh -c 'trap "" TERM; while :; do sleep 600; done' &
child=$!
printf '%%s\n' "$child" >> %s
wait "$child"
`, shellSingleQuote(watchRootLog), shellSingleQuote(watchChildLog)), 0o700)
	writeTasksFile(t, h.home, []map[string]interface{}{
		{
			"id":           watchTaskID,
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

	// Two waits, not one, because the chain between `daemon install` and the
	// script's first write has two halves that fail for unrelated reasons and
	// take unrelated amounts of time (#3820). The first half — systemd
	// activating the unit, a cold daemon binary starting, and its supervisor
	// arming the watch task — is all of what a loaded runner slows down, and the
	// daemon publishes its own verdict on it: every task record carries the LIVE
	// arming observation (#3623), and for a watch task "armed" means THIS daemon
	// is holding a running watcher for THIS definition. The second half is
	// whatever is left after that: one fork/exec and two printfs.
	//
	// One wall clock over both spends the same budget on a cold start and on a
	// spawn, so runner contention is indistinguishable from a watcher that never
	// started — and 15s of it got reported as a broken invariant (the #2879
	// class). Observing the seam between the halves means the budget below covers
	// only the spawn, and a timeout names which half ran out.
	awaitStage(t, 60*time.Second, "the daemon to arm the watch task",
		func() (bool, string) {
			arming, err := taskArming(h, watchTaskID)
			switch {
			case err != nil:
				return false, "af tasks list: " + err.Error()
			// An EMPTY arming is not "not armed". `af tasks list` takes the
			// non-spawning read path and falls back to this machine's task store
			// when no daemon answers, and a disk read has observed nothing about a
			// running supervisor — which is the whole interval this stage waits
			// through. Both keep waiting; they differ only in the note.
			case arming == "":
				return false, "no daemon has observed the task yet"
			default:
				return arming == task.ArmingArmed, "arming=" + arming
			}
		},
		func() string { return "unit " + systemdUnitSummary(systemdunit.DaemonUnitName) })
	awaitStage(t, 30*time.Second, "initial watcher tree",
		func() (bool, string) {
			roots, children := readPIDLog(watchRootLog), readPIDLog(watchChildLog)
			return len(roots) == 1 && len(children) == 1,
				fmt.Sprintf("roots=%v children=%v", roots, children)
		}, nil)
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
	//
	// The budget is staged for the same reason the startup one above is, and the
	// case here is sharper: this window covers systemd's RestartSec=5 delay, a
	// cold daemon start, AND the watcher spawn, so five of its seconds were pure
	// sleep before the replacement was even exec'd. The arming read rebases it
	// onto the spawn alone the moment the replacement daemon reports the task
	// armed, and until then the outer budget is generous enough to be about the
	// restart rather than about the runner's load.
	//
	// That read costs a process spawn, and it runs INSIDE the sampling loop: a
	// violation of this invariant is not a knife-edge event. The old tree ignores
	// SIGTERM and sleeps for ten minutes, so in a violating world it stays alive
	// until the scope's own SIGKILL — TimeoutStopUSec=4s, asserted above — and an
	// occasional 50ms sample gap cannot step over seconds of overlap.
	overlapped := false
	var newDaemon, newWatchRoot, newWatchChild int
	var armedAt, lastArmingRead time.Time
	armingNote := "never read"
	deadline := time.Now().Add(90 * time.Second)
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
		if armedAt.IsZero() && time.Since(lastArmingRead) >= 250*time.Millisecond {
			lastArmingRead = time.Now()
			arming, err := taskArming(h, watchTaskID)
			switch {
			case err != nil:
				armingNote = "af tasks list: " + err.Error()
			case arming == "":
				armingNote = "no daemon has observed the task yet"
			default:
				armingNote = "arming=" + arming
			}
			if arming == task.ArmingArmed {
				// From here the budget belongs to the SPAWN, not to the restart:
				// 30s from the moment the replacement reported the task armed. When
				// arming is quick that is less total time than the outer budget had
				// left, which is exactly the intent — the outer one is sized for a
				// cold start under contention, and that part is now over.
				armedAt = lastArmingRead
				deadline = armedAt.Add(30 * time.Second)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if overlapped {
		t.Fatalf("replacement watcher pid %d started while old tree %d/%d was still alive", newWatchRoot, oldWatchRoot, oldWatchChild)
	}
	if newDaemon <= 1 || newWatchRoot <= 1 || newWatchChild <= 1 {
		t.Fatalf("replacement did not become healthy: daemon=%d watcher=%d/%d roots=%v children=%v; replacement task arming: %s (armed=%v); unit %s",
			newDaemon, newWatchRoot, newWatchChild, readPIDLog(watchRootLog), readPIDLog(watchChildLog),
			armingNote, !armedAt.IsZero(), systemdUnitSummary(systemdunit.DaemonUnitName))
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
	h.run("tasks", "remove", watchTaskID)
	h.run("sessions", "kill", "failure-survivor")
	h.run("daemon", "uninstall")
}

// awaitStage waits for ONE step of a startup chain, and on timeout reports the
// last thing it saw rather than only naming the step it was on.
//
// It is waitUntil's diagnostic sibling, and the difference is the whole point of
// staging a wait (#3820): a chain observed step by step produces a timeout that
// says which step, holding what value, while a single wait on the end state can
// only say the end state never arrived. observe returns its verdict and a short
// note about what it just read; diagnose, which may be nil, is evaluated ONCE on
// failure, so a report that costs a subprocess is not paid for on every poll.
func awaitStage(t *testing.T, budget time.Duration, what string, observe func() (bool, string), diagnose func() string) {
	t.Helper()
	last := "nothing observed"
	deadline := time.Now().Add(budget)
	for {
		ok, note := observe()
		if ok {
			return
		}
		if note != "" {
			last = note
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	report := ""
	if diagnose != nil {
		report = "; " + diagnose()
	}
	t.Fatalf("timeout after %s waiting for %s; last observation: %s%s", budget, what, last, report)
}

// taskArming reports the daemon's live arming observation for one task, read
// through the CLI from this test's private AF home.
//
// `af tasks list` is a NON-SPAWNING read: it prefers the running daemon's
// answer and falls back to this machine's task store when none is reachable, so
// polling it can never conjure the daemon whose startup is being observed. The
// two cases are told apart by the field itself — a disk read leaves Arming empty
// ("nothing observed this row"), and only a daemon writes ArmingArmed
// /ArmingNotArmed. --all keeps the read independent of how the CLI resolves the
// cwd's project, which is a second thing that could go wrong in the middle of a
// wait that is about something else.
func taskArming(h *harness, id string) (string, error) {
	out, err := h.runResult("tasks", "list", "--all")
	if err != nil {
		return "", err
	}
	var tasks []task.Task
	if err := json.Unmarshal([]byte(out), &tasks); err != nil {
		return "", fmt.Errorf("parse af tasks list: %w\n%s", err, out)
	}
	for _, candidate := range tasks {
		if candidate.ID == id {
			return candidate.Arming, nil
		}
	}
	return "", fmt.Errorf("task %q is not in the store", id)
}

// systemdUnitSummary renders the unit's live state for a failure message —
// including NRestarts and Result, which are how a daemon that is crash-looping
// behind a systemd Restart= policy looks from the outside. It never fails the
// test: it is only ever called on a path that is already reporting one.
func systemdUnitSummary(unit string) string {
	out, err := exec.Command("systemctl", "--user", "show", unit,
		"-p", "ActiveState", "-p", "SubState", "-p", "MainPID",
		"-p", "NRestarts", "-p", "Result").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s: systemctl show failed: %v", unit, err)
	}
	return unit + " " + strings.Join(strings.Fields(string(out)), " ")
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
