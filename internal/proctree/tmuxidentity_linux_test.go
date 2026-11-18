//go:build linux

package proctree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
	"github.com/stretchr/testify/require"
)

// unreadableArgvProcessIn starts a real process in dir whose command line reads
// as nothing to classify for its whole life. That is the reading a process
// mid-execve gives for a moment (#7238). It is exec'd with an EMPTY argv. Linux
// 5.18 and later substitute argv = [""], so /proc/<pid>/cmdline is "\0"; older
// kernels leave argc at 0, so it is empty. The sleeper needs no arguments and
// does not dispatch on argv[0], so it stays alive.
func unreadableArgvProcessIn(t *testing.T, dir string) int {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	require.NoError(t, err)
	fixture, err := os.StartProcess(sleeper.Path(t), []string{}, &os.ProcAttr{
		Dir:   dir,
		Files: []*os.File{devNull, devNull, devNull},
	})
	_ = devNull.Close()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = fixture.Kill()
		_, _ = fixture.Wait()
	})
	require.Eventually(t, func() bool {
		p, lookupErr := Lookup(fixture.Pid)
		_, cwdOK := WorkingDir(fixture.Pid)
		return lookupErr == nil && p.Comm == "sleeper" && cwdOK
	}, 5*time.Second, 10*time.Millisecond, "fixture: the empty-argv sleeper never finished exec")
	_, err = readArgv(fixture.Pid)
	require.ErrorIs(t, err, ErrNoArgv, "fixture: an empty argv must read as no command line at all")
	return fixture.Pid
}

// An unreadable command line is unknown, never "not tmux" (#7238). Before this,
// IsTmuxProcess reported false here, and the worktree reaper read that false as
// permission to signal.
func TestTmuxIdentityOf_UnreadableArgvIsUnknown(t *testing.T) {
	pid := unreadableArgvProcessIn(t, t.TempDir())
	require.Equal(t, TmuxUnknown, TmuxIdentityOf(pid))
}

// The occupancy gate never signals, so it fails closed the other way. It still
// counts an unclassifiable process it positively saw inside the workspace,
// because an exemption it cannot establish cannot excuse an occupant. The
// report must say why, rather than imply the process is not tmux.
func TestOccupantsOfDir_CountsAnUnclassifiableProcessAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	pid := unreadableArgvProcessIn(t, dir)

	occupants, err := OccupantsOfDir(dir)
	require.NoError(t, err)
	var found *Occupant
	for i := range occupants {
		if occupants[i].Process.PID == pid {
			found = &occupants[i]
		}
	}
	require.NotNil(t, found, "an occupant whose tmux identity is unknown must still be counted")
	require.True(t, found.TmuxUnknown)
	require.Contains(t, DescribeOccupants([]Occupant{*found}), "unclassified: its command line or task name could not be read")
	// And the refusal says what to do next (#7242 review R8).
	advice := UnclassifiedOccupantAdvice(occupants)
	require.Contains(t, advice, "retry")
	require.Contains(t, advice, fmt.Sprintf("ps -o pid,stat,lstart,args -p %d", pid))
}

// privateTmuxServer starts a tmux server on a socket of its own, holding one
// keep-alive session, and returns a runner for it and the server's pid. Every
// command names the socket with -S and runs without $TMUX, so nothing here can
// reach any other tmux server; the cleanup kills this one by that socket.
func privateTmuxServer(t *testing.T) (run func(args ...string) (string, error), serverPID int) {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	// /tmp, not t.TempDir(): sun_path is short, and the test name is long.
	dir, err := os.MkdirTemp("/tmp", "af-pxs-")
	require.NoError(t, err)
	sock := filepath.Join(dir, "s")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TMUX_TMPDIR=") {
			env = append(env, kv)
		}
	}
	run = func(args ...string) (string, error) {
		cmd := exec.Command(bin, append([]string{"-S", sock, "-f", "/dev/null"}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Cleanup(func() {
		_, _ = run("kill-server")
		_ = os.RemoveAll(dir)
	})
	out, err := run("new-session", "-d", "-s", "keep", "sleep 600")
	require.NoError(t, err, out)
	out, err = run("set-option", "-s", "exit-empty", "off")
	require.NoError(t, err, out)
	out, err = run("display-message", "-p", "#{pid}")
	require.NoError(t, err, out)
	serverPID, err = strconv.Atoi(out)
	require.NoError(t, err, "server pid %q", out)
	return run, serverPID
}

// childrenOf lists pid's direct children: the kernel's own list where it has
// one, else a process-table scan.
func childrenOf(pid int) []int {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)); err == nil {
		var kids []int
		for _, f := range strings.Fields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				kids = append(kids, n)
			}
		}
		return kids
	}
	snap, err := Snapshot()
	if err != nil {
		return nil
	}
	var kids []int
	for child, p := range snap {
		if p.PPID == pid {
			kids = append(kids, child)
		}
	}
	return kids
}

// The #6581 identity defect, on a real server. While tmux spawns a pane, the
// forked child carries the server's task name (`tmux: server`) and argv until
// it execs, and it has already put SIGUSR1 back to its default action, so the
// socket-absent probe's SIGUSR1 kills it before the pane's program runs. The
// watcher asks IsTmuxServer about every child of this test's server while
// sessions spawn. Before the parent rule it said yes to those children in the
// fork-to-exec window: the same window that cost 199 of 200 sessions when the
// probe's signal followed the answer.
func TestIsTmuxServer_RefusesAServersChildBeforeItExecs(t *testing.T) {
	run, server := privateTmuxServer(t)
	require.True(t, IsTmuxServer(server), "control: the server itself is a server")

	stop := make(chan struct{})
	accepted := make(chan []string, 1)
	go func() {
		var seen []string
		counted := map[int]bool{}
		for {
			select {
			case <-stop:
				accepted <- seen
				return
			default:
			}
			for _, pid := range childrenOf(server) {
				if pid != server && !counted[pid] && IsTmuxServer(pid) {
					counted[pid] = true
					p, _ := Lookup(pid)
					seen = append(seen, fmt.Sprintf("pid %d comm %q ppid %d", pid, p.Comm, p.PPID))
				}
			}
		}
	}()
	const sessions = 100
	for i := 0; i < sessions; i++ {
		out, err := run("new-session", "-d", "-s", fmt.Sprintf("s%d", i), "sh -c 'echo agent-ready; exec sleep 600'")
		require.NoError(t, err, out)
	}
	close(stop)
	seen := <-accepted
	require.Empty(t, seen, "IsTmuxServer accepted a tmux server's own child; "+
		"the socket-absent probe would SIGUSR1 a pane before it execs")
}
