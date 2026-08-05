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

// The "did not burn its budget" assertions below are a FRACTION of the budget
// they are given, never an absolute wall-clock figure. The property is "returns
// as soon as the process is gone rather than polling to its deadline", and a
// fraction states exactly that while staying true on a loaded machine — where an
// absolute ceiling like 250ms is a statement about the scheduler, not about the
// code under test (#2879). A regression that really does burn the budget takes
// the whole of it and still fails.
var (
	// The budget these calls are GIVEN. Large, so an implementation that polls to
	// its deadline is unmistakable and no honest run can reach it under load.
	exitWaitBudget = 30 * time.Second
	// The bound that "did not burn its budget" is measured against. It has to sit
	// BELOW the production wait: an implementation that always waited paneExitWait
	// before answering is exactly the slow teardown this guards, so any bound above
	// that would wave it through. Derived from it so the relationship survives a
	// change to either, and still ~1000x the detection this measures in practice.
	exitWaitPrompt = paneExitWait * 2 / 3
)

func TestWaitForProcessExit_ExitedProcess(t *testing.T) {
	start := time.Now()
	require.True(t, waitForProcessExit(exitedProcess(t), exitWaitBudget),
		"an already-exited PID must report exited")
	require.Less(t, time.Since(start), exitWaitPrompt,
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
	require.True(t, waitForProcessExit(process, exitWaitBudget),
		"a pane that exited but remains an unreaped zombie is no longer writing")
	require.Less(t, time.Since(start), exitWaitPrompt,
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
//
// The fixture states the case in the shape tmux actually produces, which matters
// since #2962 made the destructive path distinguish a session that is GONE from a
// pane list it could not READ. Measured against tmux 3.4 for a missing session:
//
//	display-message -p -t '=X:' '#{pane_pid}'  ->  exit 0, EMPTY output
//	list-panes -s -t '=X:' -F '#{pane_pid}'    ->  exit 1, "can't find session: X"
//
// The bare fmt.Errorf this replaces modelled neither: it is an unclassifiable
// failure, which now correctly refuses cleanup. That direction is covered by
// TestCloseAndWaitUnqueryablePaneStillChecksTheProcessSet, so both outcomes stay
// pinned rather than one being traded for the other.
func TestCloseAndWaitForPaneExit_SessionGone(t *testing.T) {
	killed := false
	sessionName := toTmuxName("close-wait-gone", "")
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				killed = true
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "list-panes") {
				return nil, tmuxCantFindSessionError(t, sessionName)
			}
			// display-message answers, with nothing: exit 0 and empty output.
			return nil, nil
		},
	}

	session := newTmuxSession(sessionName, "claude", NewMockPtyFactory(t), cmdExec)

	start := time.Now()
	state, err := session.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state,
		"tmux answered every command, so the pane's fate is established")
	require.Less(t, time.Since(start), time.Second)
	require.True(t, killed, "kill-session must still run when the pane PID is unqueryable")
}

// tmuxCantFindSessionError builds the error tmux really returns for a command
// against a session that does not exist: an *exec.ExitError with status 1 and
// the diagnostic on STDERR. A hand-rolled fmt.Errorf has no Stderr, so the
// classifier that separates "gone" from "unreadable" cannot see it — the whole
// distinction #2962 turns on.
func tmuxCantFindSessionError(t *testing.T, sanitizedName string) error {
	t.Helper()
	c := exec.Command("sh", "-c", `printf "can't find session: %s\n" "$NAME" >&2; exit 1`)
	c.Env = append(os.Environ(), "NAME="+sanitizedName)
	_, err := c.Output()
	if err == nil {
		t.Fatal("the fixture must actually fail, or it proves nothing")
	}
	return err
}
