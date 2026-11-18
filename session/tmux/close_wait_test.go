package tmux

import (
	"bytes"
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
	"github.com/sachiniyer/agent-factory/log"
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

// #7491 item 3, on an isolated server — the trusting vanished-session close
// reaches PaneStateKnown when the ONLY marked survivor is a positive
// placement: a carrier whose ancestry resolves to a VERIFIED af daemon is
// proven foreign, so it is excluded from signalling, named in the WARNING
// log, and does NOT block the close. The end-to-end property — a revert
// that routed the placement into the refusal error blocks a close that
// should be Known.
func TestCloseAndWaitForPaneExit_VanishedPlacedCarrierLeavesTheCloseKnown(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_placedknown_%d", time.Now().UnixNano())
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	gen := "gen-placedknown"

	// The session whose close converges: its own members admit and die.
	agentSpec := agentSleeperSpec(t)
	host, rehosted := spawnMarkedSessionWithRehostedChild(t, name, home, gen, agentSpec[0], agentSpec[1:]...)
	s := NewTmuxSessionFromSanitizedName(name, agentSpec[0])
	root, err := s.PaneRootProcess()
	require.NoError(t, err)
	require.GreaterOrEqual(t, rehosted.StartID, root.StartID,
		"the re-hosted member must postdate the recorded root for tier 2 to admit it")

	// The placed carrier: a marked sleep whose parent is a VERIFIED af
	// daemon — the test binary reexec'd with daemon argv, whose kernel
	// buildinfo attests this repo's module (#7491 item 1). Its markers
	// match the vanished session's cohort; its ancestry proves it foreign.
	// The daemon itself sits in the marked child's kernel session, so the
	// sweep's SessionMembers pass pulls it into candidates too — it must
	// carry markers to be judged at all, and a FOREIGN home is the honest
	// mark: another installation's daemon is excluded by the home gate,
	// never by pretending it belongs to this cohort.
	dir := t.TempDir()
	foreignHome := t.TempDir()
	childPidFile := filepath.Join(dir, "child.pid")
	daemon := exec.Command(os.Args[0])
	daemon.Args = []string{"af", "--daemon"}
	daemon.Env = append(os.Environ(),
		decisionDaemonEnv+"=1",
		decisionDaemonSpecEnv+"="+name+"\t"+home+"\t"+gen+"\t"+childPidFile,
		EnvMarkerSession+"="+name,
		EnvMarkerHome+"="+foreignHome,
		EnvMarkerGeneration+"="+gen)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, daemon.Start())
	t.Cleanup(func() {
		_ = daemon.Process.Kill()
		_, _ = daemon.Process.Wait()
	})
	require.Eventually(t, func() bool {
		return proctree.ArgvIsAFDaemon(proctree.Argv(daemon.Process.Pid))
	}, 5*time.Second, 20*time.Millisecond,
		"the daemon argv claim must land — the test binary's kernel exe VERIFIES it")
	waitForPIDFile(t, childPidFile)
	placed := processFromPIDFile(t, childPidFile)

	var warnings bytes.Buffer
	oldWarnings := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(oldWarnings) })

	vanishSessionWithObservableEscapee(t, name, host)

	state, blind, err := s.CloseAndWaitForPaneExitTrustingOwnGeneration()
	require.True(t, blind, "a session gone before the close ran is the vanished-sweep path")
	require.NoError(t, err,
		"the only marked survivor is positively placed — nothing blocks")
	require.Equal(t, PaneStateKnown, state,
		"a close whose only exclusions are placements is KNOWN — teardown may touch the worktree")

	require.Eventually(t, func() bool {
		return !proctree.AliveSame(host) && !proctree.AliveSame(rehosted)
	}, 10*time.Second, 50*time.Millisecond,
		"the session's own members must be reaped")

	require.True(t, proctree.AliveSame(placed),
		"the positively-placed carrier was signalled — the exclusion failed")
	require.Contains(t, warnings.String(), "LEFT ALIVE",
		"the WARNING names that the carrier keeps running under a Known close")
	require.Contains(t, warnings.String(), "pid "+strconv.Itoa(placed.PID)+" ",
		"the WARNING names the placed carrier's pid for the operator")
}
