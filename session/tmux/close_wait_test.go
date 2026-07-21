package tmux

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

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestCloseAndWaitForPaneExit_ReapsLivingDescendantBeforeReturning(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_close_desc_%d", time.Now().UnixNano())
	childFile := filepath.Join(t.TempDir(), "child.pid")
	// The pane leader exits on tmux's SIGHUP. Its child inherits ignored HUP/TERM
	// dispositions and therefore survives until the captured-tree reaper reaches
	// SIGKILL — exactly the process the old leader-only wait returned ahead of.
	command := fmt.Sprintf("sh -c 'trap \"\" HUP TERM; echo $$ > %s; exec sleep 300' & exec sleep 300", strconv.Quote(childFile))
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", name, command).Run())
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })

	var childPID int
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(childFile)
		if err != nil {
			return false
		}
		childPID, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && childPID > 1
	}, 3*time.Second, 20*time.Millisecond)
	child := processIdentity(t, childPID)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	s := NewTmuxSessionFromSanitizedName(name, "sh")
	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.False(t, proctree.AliveSame(child),
		"destructive teardown returned while a captured pane descendant could still write to the worktree")
}

func TestCloseAndWaitForPaneExit_UnobservableProcessTreeKeepsCleanupUnsafe(t *testing.T) {
	process := exitedProcess(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if strings.Contains(command.String(), "display-message") {
				return []byte(fmt.Sprintf("%d\n", process.PID)), nil
			}
			if strings.Contains(command.String(), "list-panes") {
				return []byte("not-a-pane-pid\n"), nil
			}
			return nil, nil
		},
	}

	s := newTmuxSession(toTmuxName("close-wait-unobservable-tree", ""), "claude", NewMockPtyFactory(t), cmdExec)
	state, err := s.CloseAndWaitForPaneExit()
	require.ErrorContains(t, err, "complete process tree")
	require.Equal(t, PaneStateUnknown, state,
		"an unreadable descendant set must not be reduced to the observed exit of the pane leader")
}

func processIdentity(t *testing.T, pid int) proctree.Process {
	t.Helper()
	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	process, ok := snap[pid]
	require.True(t, ok, "pid %d missing from process snapshot", pid)
	return process
}

// exitedProcess returns the captured identity of a process that is now fully
// exited and reaped, so a later PID reuse cannot change the expected answer.
func exitedProcess(t *testing.T) proctree.Process {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	process := processIdentity(t, cmd.Process.Pid)
	require.NoError(t, cmd.Process.Kill())
	_, _ = cmd.Process.Wait()
	return process
}

func TestWaitForProcessExit_ExitedProcess(t *testing.T) {
	start := time.Now()
	require.True(t, waitForProcessExit(exitedProcess(t), 2*time.Second),
		"an already-exited PID must report exited")
	require.Less(t, time.Since(start), time.Second,
		"a dead PID must be detected without burning the timeout")
}

func TestWaitForProcessExit_AliveProcessTimesOut(t *testing.T) {
	// Our own PID is alive for the duration of the test.
	require.False(t, waitForProcessExit(processIdentity(t, os.Getpid()), 120*time.Millisecond),
		"a live PID must report not-exited once the timeout elapses")
}

func TestWaitForProcessExit_ZombieCountsAsExited(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	process := processIdentity(t, cmd.Process.Pid)
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })
	require.NoError(t, cmd.Process.Kill())

	start := time.Now()
	require.True(t, waitForProcessExit(process, 500*time.Millisecond),
		"a pane that exited but remains an unreaped zombie is no longer writing")
	require.Less(t, time.Since(start), 250*time.Millisecond,
		"an exited pane must not burn the teardown wait budget")
}

func TestCloseAndWaitForPaneExit_AlivePaneKeepsCleanupUnsafe(t *testing.T) {
	oldWait := paneExitWait
	paneExitWait = 20 * time.Millisecond
	t.Cleanup(func() { paneExitWait = oldWait })

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "display-message") {
				return []byte(fmt.Sprintf("%d\n", os.Getpid())), nil
			}
			return nil, nil
		},
	}
	session := newTmuxSession(toTmuxName("close-wait-live", ""), "claude", NewMockPtyFactory(t), cmdExec)

	state, err := session.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state,
		"a pane still flushing after kill-session must veto worktree cleanup")
}

// TestCloseAndWaitForPaneExit_QueriesPaneBeforeKill verifies the #802
// ordering contract: the pane PID is captured via display-message BEFORE
// kill-session runs (afterwards there is nothing left to query), and the
// wait happens on that PID after the session is gone.
func TestCloseAndWaitForPaneExit_QueriesPaneBeforeKill(t *testing.T) {
	process := exitedProcess(t)
	pid := process.PID
	var calls []string

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				calls = append(calls, "kill-session")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "display-message") {
				calls = append(calls, "display-message")
				return []byte(fmt.Sprintf("%d\n", pid)), nil
			}
			return []byte(""), nil
		},
	}

	session := newTmuxSession(toTmuxName("close-wait", ""), "claude", NewMockPtyFactory(t), cmdExec)

	start := time.Now()
	state, err := session.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state,
		"tmux answered every command, so the pane's fate is established")
	require.Less(t, time.Since(start), time.Second,
		"wait on an already-dead agent process must return promptly")

	require.Equal(t, []string{"display-message", "kill-session"}, calls,
		"pane PID must be captured before kill-session destroys the session")
}

// TestCloseAndWaitForPaneExit_SessionGone: when the pane PID cannot be
// queried (session already dead), the method must skip the wait and still
// perform the Close teardown.
func TestCloseAndWaitForPaneExit_SessionGone(t *testing.T) {
	killed := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				killed = true
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("can't find session")
		},
	}

	session := newTmuxSession(toTmuxName("close-wait-gone", ""), "claude", NewMockPtyFactory(t), cmdExec)

	start := time.Now()
	state, err := session.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state,
		"tmux answered every command, so the pane's fate is established")
	require.Less(t, time.Since(start), time.Second)
	require.True(t, killed, "kill-session must still run when the pane PID is unqueryable")
}
