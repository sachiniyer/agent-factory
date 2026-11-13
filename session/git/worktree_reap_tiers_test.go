package git

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/stretchr/testify/require"
)

const (
	fixtureReady   = "fixture: ready"
	fixtureExecing = "fixture: execing tmux"
	// execsTmuxOnSIGTERM is the fixture's script. On SIGTERM it execs $0, a
	// sleeper copied under the name tmux. The background copy of $0 is there
	// only for wait: POSIX has a trapped signal end wait at once, so the exec
	// follows the SIGTERM within a millisecond. It reads as tmux itself, so the
	// reaper leaves it alone and only the trapped SIGTERM can end the wait. Its
	// pid is printed so the test can clean it up.
	execsTmuxOnSIGTERM = `trap 'echo "` + fixtureExecing + `"; exec "$0" 300' TERM
"$0" 120 &
echo "` + fixtureReady + ` $!"
wait`
)

// The reaper's leave-alone identity must hold when it signals, not only when it
// selects (#7244). Selection reads the identity once, and the signal tiers
// follow: SIGTERM, then SIGKILL about two seconds later. AliveSame cannot see an
// exec in between, because exec keeps the pid and the start time. So a process
// that was not tmux at selection and has become tmux since must be left out of
// every tier that comes after the change.
//
// The fixture is a shell, selected as an ordinary writer: its cwd is in the
// worktree, and its argv and task name are sh's. It catches the reaper's own
// SIGTERM and execs a sleeper copied under the name tmux, which puts the exec
// between the two tiers without a timing guess. It is never a real tmux, so
// nothing here can reach the user's server. On Linux the new image reads as
// tmux by its argv[0] and its task name; on darwin, by its argv[0] and p_comm,
// which is the exec'd file name.
//
// It is a shell, not this test binary, because a shell is single-threaded. When
// a multi-threaded process execs from a thread other than its leader, Linux
// shows the old leader as a zombie for about 100 µs while the pid is handed
// over (de_thread; measured on a Go fixture, in 3 of 5 execs). AliveSame reads
// that as an exit, so a wait that polls at that moment ends early and the
// SIGKILL tier never comes, which made a Go fixture's result depend on timing.
// A shell execs from its only thread and never reads as Z (measured: none in 6
// execs under dash and bash).
func TestReapWorktreeWriters_LeavesAProcessThatBecameTmuxOutOfTheLaterTier(t *testing.T) {
	worktree := t.TempDir()
	image := sleeper.Named(t, filepath.Join(t.TempDir(), "tmux"))
	output, fixtureStdout, err := os.Pipe()
	require.NoError(t, err)
	fixture := exec.Command("/bin/sh", "-c", execsTmuxOnSIGTERM, image)
	fixture.Dir = worktree
	fixture.Stdout = fixtureStdout
	require.NoError(t, fixture.Start())
	_ = fixtureStdout.Close()
	var background proctree.Process
	t.Cleanup(func() {
		// The two processes this test started and recorded, and nothing else.
		// The background sleeper is signalled only if it is still the same
		// process.
		if background.PID > 0 {
			_ = proctree.Signal(background, syscall.SIGKILL)
		}
		_ = fixture.Process.Kill()
		_ = fixture.Wait()
		_ = output.Close()
	})
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	ready := requireFixtureLine(t, lines, fixtureReady+" ", 30*time.Second)
	backgroundPID, err := strconv.Atoi(strings.TrimPrefix(ready, fixtureReady+" "))
	require.NoError(t, err)
	background, err = proctree.Lookup(backgroundPID)
	require.NoError(t, err)
	process, err := proctree.Lookup(fixture.Process.Pid)
	require.NoError(t, err)
	root := normalizeWorktreePath(worktree)
	requireEventually(t, 5*time.Second, func() bool {
		cwd, ok := proctree.WorkingDir(process.PID)
		return ok && pathutil.IsAtOrInside(filepath.Clean(cwd), root)
	}, "the fixture's cwd never became observable inside the worktree")
	// What the fixture waits on must read as tmux before the reap, so that the
	// reap leaves it alone and cannot end the wait some other way.
	requireEventually(t, 5*time.Second, func() bool {
		return proctree.TmuxIdentityOf(backgroundPID) == proctree.Tmux
	}, "the fixture's background sleeper never finished starting")
	// The precondition: at selection the fixture is an ordinary writer, which
	// the reaper is right to select and to send SIGTERM.
	require.Equal(t, proctree.NotTmux, proctree.TmuxIdentityOf(process.PID),
		"fixture: before the reap it must read positively as not tmux")
	var info logtest.Buffer
	aflog.CaptureOutput(t, aflog.InfoLog, "INFO: ", &info)

	reapWorktreeWriters(worktree)

	// The reap's SIGTERM is what made the fixture exec, so this line proves the
	// first tier reached it and that the exec came after selection.
	requireFixtureLine(t, lines, fixtureExecing, time.Second)
	require.True(t, proctree.AliveSame(process),
		"a process that execs tmux after selection must not receive a later signal tier: "+
			"the reaper's SIGKILL reached the fixture after it had become tmux")
	require.Equal(t, proctree.Tmux, proctree.TmuxIdentityOf(process.PID),
		"fixture: after its exec it must read as tmux, or this test proves nothing")
	require.Contains(t, info.String(),
		fmt.Sprintf("pid %d (%s) was selected as not tmux, but just before its SIGKILL it reads as tmux", process.PID, process.Comm),
		"the reap must say why it left a selected process alone")
}

// requireFixtureLine waits for the fixture to print a line starting with want,
// and returns it. It fails on timeout or if the fixture's output ends first.
func requireFixtureLine(t *testing.T, lines <-chan string, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	var seen []string
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the fixture's output ended before %q; it printed:\n%s", want, strings.Join(seen, "\n"))
			}
			if strings.HasPrefix(line, want) {
				return line
			}
			seen = append(seen, line)
		case <-deadline:
			t.Fatalf("the fixture did not print %q within %s; it printed:\n%s", want, timeout, strings.Join(seen, "\n"))
		}
	}
}
