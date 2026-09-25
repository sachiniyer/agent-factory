package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #4506 review: a process tab runs under remain-on-exit, so a finished command
// leaves a HELD DEAD PANE. tmux keeps reporting that pane's pane_pid, which is
// the pid that already exited and was reaped. Measured on tmux 3.4 with
// `sh -c 'echo hello; exit 7'`:
//
//	list-panes -F '#{pane_pid} #{pane_dead}'  ->  "<pid> 1", and <pid> is not in the process table
//
// The teardown read that pid as a pane process that "disappeared before its
// descendants could be captured" and refused, so archive, kill and the
// account-scope stop all failed on a finished process tab.

// startHeldPane starts a real session on the test's private server with
// remain-on-exit set in the same invocation (as Start does), and waits until
// tmux reports the pane dead.
func startHeldPane(t *testing.T, name, command string) {
	t.Helper()
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", name, command,
		";", "set-option", "-w", "-t", exactTarget(name), "remain-on-exit", "on").Run())
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).Run() })
	require.Eventually(t, func() bool {
		out, err := exec.Command("tmux", "display-message", "-p", "-t", exactTarget(name), "#{pane_dead}").Output()
		return err == nil && strings.TrimSpace(string(out)) == "1"
	}, 5*time.Second, 20*time.Millisecond, "the pane never died")
}

// probeHeldExit probes a held dead pane once and requires its exit status. One
// probe is the contract: ProbePaneExit waits for tmux to collect the root and
// nudges a tmux that lost the root's SIGCHLD (#4682), so a status missing here
// is a product failure, not a slow runner. The failure says which of the two
// states tmux was left in.
func probeHeldExit(t *testing.T, s *TmuxSession, name string) (status int, at time.Time) {
	t.Helper()
	dead, status, statusKnown, at, known := s.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
	if !statusKnown {
		out, _ := exec.Command("tmux", "display-message", "-p", "-t", exactTarget(name),
			"#{pane_dead_status}|#{pane_pid}").Output()
		fields := strings.Split(strings.TrimSpace(string(out)), "|")
		state := "unreadable"
		if pid, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
			_, lookupErr := proctree.Lookup(pid)
			switch {
			case errors.Is(lookupErr, proctree.ErrProcessExited):
				state = "still a zombie: tmux never collected it"
			case pidGone(pid):
				state = "gone"
			default:
				state = "running"
			}
		}
		t.Fatalf("no exit status after one probe (tmux now reports %q; root %s)", out, state)
	}
	return status, at
}

func requireSessionGone(t *testing.T, name string) {
	t.Helper()
	require.Error(t, exec.Command("tmux", "has-session", "-t", exactTarget(name)).Run(),
		"the held dead pane's session must be gone after teardown")
}

func TestCloseAndWaitForPaneExit_TearsDownAHeldDeadPane(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_dead_pane_%d", time.Now().UnixNano())
	before := time.Now().Truncate(time.Second)
	startHeldPane(t, name, "sh -c 'echo hello; exit 7'")

	s := NewTmuxSessionFromSanitizedName(name, "sh")
	status, at := probeHeldExit(t, s, name)
	require.Equal(t, 7, status)
	require.False(t, at.Before(before), "death time %v predates the pane", at)

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state,
		"a pane whose command already exited has nothing left to wait for")
	requireSessionGone(t, name)
	require.True(t, s.ClosedConclusively(), "the observed close must latch its proof")
}

// #4682, on real tmux: Ubuntu's tmux 3.4 leaves about one in sixteen of these
// roots uncollected (25 of 400 measured), so a single pane passes by luck. Many
// panes on one server make the lost SIGCHLD all but certain to occur, and every
// probe must still recover its status. On a tmux that loses nothing (3.6+, or a
// build without utempter) this passes trivially.
func TestProbePaneExitRecoversTheStatusOfEveryHeldExit(t *testing.T) {
	testguard.IsolateTmux(t)
	const panes = 60
	for i := range panes {
		name := fmt.Sprintf("af_test_held_exit_%d_%d", time.Now().UnixNano(), i)
		startHeldPane(t, name, "sh -c 'echo hello; exit 7'")
		status, _ := probeHeldExit(t, NewTmuxSessionFromSanitizedName(name, "sh"), name)
		require.Equal(t, 7, status, "pane %d", i)
		require.NoError(t, exec.Command("tmux", "kill-session", "-t", exactTarget(name)).Run())
	}
}

// The root exited, but a child it backgrounded is still running: it left the
// pane's ppid tree when its parent died, and only its kernel session still ties
// it to the pane. The teardown must find it through that session and stop it.
func TestCloseAndWaitForPaneExit_HeldDeadPaneReapsItsDetachedSurvivor(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_dead_pane_bg_%d", time.Now().UnixNano())
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	// The child writes its pid only after it ignores SIGHUP, and the root exits
	// only after that: the root's exit hangs up the terminal, and a child still
	// starting would die of it and leave nothing to reap.
	childScript := filepath.Join(dir, "child.sh")
	require.NoError(t, os.WriteFile(childScript, []byte(
		"trap '' HUP\necho $$ > \"$1\"\nexec sleep 300 </dev/null >/dev/null 2>&1\n"), 0o600))
	startHeldPane(t, name, fmt.Sprintf(
		`sh -c 'sh %[1]s %[2]s & while [ ! -s %[2]s ]; do sleep 0.02; done; exit 3'`,
		strconv.Quote(childScript), strconv.Quote(childFile)))

	childPID := readPIDFile(t, childFile)
	child := processIdentity(t, childPID)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	require.NotEqual(t, child.SID, childPID, "the fixture must keep the survivor in the dead root's session")

	s := NewTmuxSessionFromSanitizedName(name, "sh")
	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	requireSessionGone(t, name)
	require.False(t, proctree.AliveSame(child),
		"a detached child of a finished command survived the teardown")
}

// pane_dead is not proof the command exited. A root that closes its terminal
// makes tmux mark the pane dead while the process keeps running, and tmux has
// not reaped it, so no status, signal or death time is reported. The teardown
// must still treat that root as a live pane process.
//
// Linux only: that state is how a Linux pty answers the root closing its last
// terminal descriptor. On macOS (tmux 3.7c in CI) the pane is never marked dead
// while the root runs, so there is no such pane to tear down there.
func TestCloseAndWaitForPaneExit_DeadPaneWithARunningRootStopsTheRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("a pane is held dead with its root running only on Linux ptys, not on %s", runtime.GOOS)
	}
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_dead_pane_eof_%d", time.Now().UnixNano())
	// HUP is ignored before the terminal is closed: closing it is what makes tmux
	// hang the pane up.
	startHeldPane(t, name, `sh -c 'trap "" HUP; exec </dev/null >/dev/null 2>&1; exec sleep 300'`)

	out, err := exec.Command("tmux", "display-message", "-p", "-t", exactTarget(name),
		"#{pane_pid}|#{pane_dead_status}#{pane_dead_signal}#{pane_dead_time}").Output()
	require.NoError(t, err)
	fields := strings.Split(strings.TrimSpace(string(out)), "|")
	require.Len(t, fields, 2)
	require.Empty(t, fields[1], "the fixture must hold a dead pane whose root tmux has not reaped")
	rootPID, err := strconv.Atoi(fields[0])
	require.NoError(t, err)
	root := processIdentity(t, rootPID)
	t.Cleanup(func() { _ = syscall.Kill(rootPID, syscall.SIGKILL) })

	s := NewTmuxSessionFromSanitizedName(name, "sh")
	dead, _, _, _, known := s.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "a pane whose root is still running has not finished")

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.False(t, proctree.AliveSame(root), "the running root of a dead pane survived the teardown")
}

// heldPaneExec answers every pane query as tmux would for one held dead pane:
// it expands whatever format was asked for, so the fixture tracks the query
// instead of a canned shape. Every command is accepted, including kill-session.
func heldPaneExec(pid int, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	expand := strings.NewReplacer(
		"#{pane_pid}", strconv.Itoa(pid),
		"#{pane_dead}", "1",
		"#{pane_dead_status}", status,
		"#{pane_dead_signal}", signal,
		"#{pane_dead_time}", deadTime,
	)
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "kill-session") && killed != nil {
				*killed = true
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			s := c.String()
			if strings.Contains(s, "list-panes") || strings.Contains(s, "display-message") {
				return []byte(expand.Replace(c.Args[len(c.Args)-1]) + "\n"), nil
			}
			return nil, nil
		},
	}
}

// The mock form of the defect: a held dead pane reports a pid that is absent
// from the process table. Refusing it made a finished process tab impossible to
// archive or kill.
func TestCloseAndWaitForPaneExit_HeldPaneWithAnAbsentPidIsConclusive(t *testing.T) {
	gone := exitedProcess(t)
	killed := false
	s := newTmuxSession(toTmuxName("dead-pane-absent", ""), "sh", NewMockPtyFactory(t),
		heldPaneExec(gone.PID, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed, "the dead pane's session must still be killed")
}

// tmux reaped the dead root long ago, and the kernel has since handed its pid to
// an unrelated process. That process is not the pane's: nothing may wait on it,
// and nothing may signal it.
func TestCloseAndWaitForPaneExit_HeldPaneNeverTouchesARecycledPid(t *testing.T) {
	shrinkReapWaits(t)
	stranger := startStranger(t, nil)
	s := newTmuxSession(toTmuxName("dead-pane-recycled", ""), "sh", NewMockPtyFactory(t),
		heldPaneExec(stranger.PID, "0", "", "1726000000", nil))

	start := time.Now()
	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.Less(t, time.Since(start), paneExitWait, "the teardown waited on a process that is not the pane's")
	require.True(t, proctree.AliveSame(stranger), "the teardown signalled a process that merely reuses the dead pane's pid")
}

// Without a reported status the root may not be reaped yet, and tmux older than
// 3.3 never reports a signal death either, so a live process behind the pid is
// ambiguous. A launch marker naming ANOTHER session settles it: the pid now
// belongs to a different pane.
func TestCloseAndWaitForPaneExit_UnreapedDeadPaneSkipsAPidMarkedForAnotherSession(t *testing.T) {
	shrinkReapWaits(t)
	stranger := startStranger(t, []string{EnvMarkerSession + "=af_someone_else"})
	s := newTmuxSession(toTmuxName("dead-pane-marked", ""), "sh", NewMockPtyFactory(t),
		heldPaneExec(stranger.PID, "", "", "", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, proctree.AliveSame(stranger), "the teardown signalled another session's process")
}

// startStranger starts a process the teardown under test must leave alone.
//
// It leads its own kernel session, and that is a safety property rather than
// realism. This package's test binary is named tmux.test, so its direct children
// pass the "child of a tmux server" check. A teardown that wrongly adopts the
// stranger as a pane root also reaps the stranger's whole session. Without
// Setsid, that session is the one `go test` runs in, and the unfixed code killed
// the shell that launched the test.
func startStranger(t *testing.T, extraEnv []string) proctree.Process {
	t.Helper()
	c := exec.Command("sleep", "300")
	c.Env = append(os.Environ(), extraEnv...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, c.Start())
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	return processIdentity(t, c.Process.Pid)
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 1
	}, 3*time.Second, 20*time.Millisecond)
	return pid
}
