package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

const markedEscapeeArg = "--tmux-test-marked-escapee"

// Run before TestMain's sandbox rewrites the inherited environment. Using the
// test binary supplies setsid on both Linux and macOS without a setsid(1)
// dependency. The pane shell must not publish $!: that only proves a fork,
// not that the child has exec'd with its markers or installed HUP protection.
func handleMarkedEscapeeExec() {
	if len(os.Args) != 3 || os.Args[1] != markedEscapeeArg {
		return
	}
	if err := runMarkedEscapee(os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runMarkedEscapee(pidFile string) error {
	signal.Ignore(syscall.SIGHUP)
	if _, err := syscall.Setsid(); err != nil {
		return err
	}
	sleeper, err := exec.LookPath("sleep")
	if err != nil {
		return err
	}
	if err := os.WriteFile(pidFile+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	if err := os.Rename(pidFile+".tmp", pidFile); err != nil {
		return err
	}
	// exec keeps the published identity, markers and ignored SIGHUP disposition.
	return syscall.Exec(sleeper, []string{"sleep", "300"}, os.Environ())
}

// Killing tmux is asynchronous with respect to pane exit and reparenting.
// Establish absence AND a live, reparented helper before letting a sweep look.
// Do not require PPID 1: a runner may use a child subreaper.
func vanishSessionWithObservableEscapee(t *testing.T, name string, helper proctree.Process) {
	t.Helper()
	out, err := exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
	require.NoError(t, err, "vanish original tmux session: %s", out)
	require.Eventually(t, func() bool {
		exists, known, probeErr := probeSessionStrict(cmd.MakeExecutor(), name)
		if probeErr != nil || !known || exists {
			return false
		}
		current, lookupErr := proctree.Lookup(helper.PID)
		return lookupErr == nil && current.StartID == helper.StartID &&
			current.SID == helper.PID && current.PPID != helper.PPID
	}, 5*time.Second, 20*time.Millisecond,
		"marked helper %d must be alive and reparented after tmux session %s vanished", helper.PID, name)
}
