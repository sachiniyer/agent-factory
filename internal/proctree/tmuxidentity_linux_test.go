//go:build linux

package proctree

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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

// isolatedTmux returns a runner for a tmux socket of the test's own, the
// environment it runs tmux with, and the socket's path. Every command names the
// socket with -S and runs without $TMUX, so nothing here can reach any other
// tmux server; the cleanup kills this socket's server by that socket.
func isolatedTmux(t *testing.T) (run func(args ...string) (string, error), env []string, sock string) {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	// /tmp, not t.TempDir(): sun_path is short, and the test name is long.
	dir, err := os.MkdirTemp("/tmp", "af-pxs-")
	require.NoError(t, err)
	sock = filepath.Join(dir, "s")
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
	return run, env, sock
}

// serverPIDOf asks the server on run's socket for its pid.
func serverPIDOf(t *testing.T, run func(args ...string) (string, error)) int {
	t.Helper()
	out, err := run("display-message", "-p", "#{pid}")
	require.NoError(t, err, out)
	pid, err := strconv.Atoi(out)
	require.NoError(t, err, "server pid %q", out)
	return pid
}

// privateTmuxServer starts a tmux server on a socket of its own (isolatedTmux),
// holding one keep-alive session, and returns a runner for it and the server's
// pid.
func privateTmuxServer(t *testing.T) (run func(args ...string) (string, error), serverPID int) {
	t.Helper()
	run, _, _ = isolatedTmux(t)
	out, err := run("new-session", "-d", "-s", "keep", "sleep 600")
	require.NoError(t, err, out)
	out, err = run("set-option", "-s", "exit-empty", "off")
	require.NoError(t, err, out)
	return run, serverPIDOf(t, run)
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

// The #6581 identity defect, on a real server. While tmux spawns a pane or a
// job, the forked child carries the server's task name (`tmux: server`) and
// argv until it execs, and it has already put SIGUSR1 back to its default
// action, so the socket-absent probe's SIGUSR1 kills it before the pane's
// program runs. The watcher asks IsTmuxServer about every child of this test's
// server while sessions and jobs spawn. Before the parent rule it said yes to
// those children in the fork-to-exec window: the same window that cost 199 of
// 200 sessions when the probe's signal followed the answer.
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
		// A job child is a plain fork that keeps the server's session.
		out, err = run("run-shell", "-b", "true")
		require.NoError(t, err, out)
	}
	close(stop)
	seen := <-accepted
	require.Empty(t, seen, "IsTmuxServer accepted a tmux server's own child; "+
		"the socket-absent probe would SIGUSR1 a pane before it execs")
}

// The fork shape is read off the stat line by field number, past a task name
// that may itself hold spaces and parentheses.
func TestParseForkShape(t *testing.T) {
	line := func(tty, blocked string) []byte {
		fields := make([]string, 52-2)
		for i := range fields {
			fields[i] = "0"
		}
		fields[0] = "S"
		fields[7-3] = tty
		fields[32-3] = blocked
		return []byte("4242 (tmux: (x) y) " + strings.Join(fields, " ") + "\n")
	}
	usr1 := strconv.Itoa(1 << (int(syscall.SIGUSR1) - 1))
	for _, tc := range []struct {
		name           string
		tty, blocked   string
		ctty, blocked1 bool
	}{
		{"no terminal, nothing blocked", "0", "0", false, false},
		{"a terminal", "34816", "0", true, false},
		{"SIGUSR1 blocked", "0", usr1, false, true},
		{"every signal blocked", "0", "2147483647", false, true},
		{"other signals blocked", "0", strconv.Itoa(1<<(int(syscall.SIGTERM)-1) | 1<<(int(syscall.SIGHUP)-1)), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseForkShape(4242, line(tc.tty, tc.blocked))
			require.NoError(t, err)
			require.Equal(t, forkShape{known: true, ctty: tc.ctty, usr1Blocked: tc.blocked1}, got)
		})
	}
	_, err := parseForkShape(4242, []byte("4242 (tmux) S 1 4242 4242 0 -1"))
	require.Error(t, err, "a truncated line has no blocked mask")
	_, err = parseForkShape(4242, line("tty", "0"))
	require.Error(t, err)

	p, shape, err := lookupForkShape(os.Getpid())
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), p.PID)
	require.True(t, shape.known, "a live process's fork shape is read on Linux")
}

const (
	// tmuxInitTitleEnv gates the reexec helper below and names the task name it
	// takes. tmuxInitSockEnv is the socket it starts its server on.
	tmuxInitTitleEnv = "PROCTREE_TEST_TMUX_INIT_TITLE"
	tmuxInitSockEnv  = "PROCTREE_TEST_TMUX_INIT_SOCK"
)

// The reexec helper for the tmux-init test: a stand-in for a PID namespace
// whose init is tmux. It becomes a child subreaper, so an orphan below it is
// reparented to it exactly as one in a container is reparented to its pid 1,
// takes a tmux task name, starts a daemonised server, and holds until its stdin
// closes.
func TestTmuxInitHelperProcess(t *testing.T) {
	title := os.Getenv(tmuxInitTitleEnv)
	if title == "" {
		t.Skip("reexec helper — not a test")
	}
	fail := func(what string, err error) {
		fmt.Printf("FAIL %s: %v\n", what, err)
		os.Exit(2)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fail("PR_SET_CHILD_SUBREAPER", err)
	}
	// /proc/self/comm names the thread-group leader, whichever thread writes it.
	if err := os.WriteFile("/proc/self/comm", []byte(title), 0); err != nil {
		fail("retitle", err)
	}
	out, err := exec.Command("tmux", "-S", os.Getenv(tmuxInitSockEnv), "-f", "/dev/null",
		"new-session", "-d", "-s", "keep", "sleep 600").CombinedOutput()
	if err != nil {
		fail("new-session: "+strings.TrimSpace(string(out)), err)
	}
	fmt.Println("READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// R1 on #7535. In a PID namespace whose init is tmux (a container started as
// `tmux`, or with `tmux -D` as its entrypoint, and no --init), every daemonised
// server is reparented to that tmux. A rule that refused every process with a
// tmux parent refused them all, af's own server included, so the socket-absent
// probe could never ask it to recreate a removed socket. Such a server cannot
// be its parent's fork-before-exec child: it leads its own session, has no
// terminal, and runs with signals unblocked. So it is still a server.
func TestIsTmuxServer_AcceptsAServerOrphanedToATmuxInit(t *testing.T) {
	for _, title := range []string{"tmux: client", "tmux: server"} {
		t.Run(title, func(t *testing.T) {
			run, env, sock := isolatedTmux(t)
			helper := exec.Command(os.Args[0], "-test.run=^TestTmuxInitHelperProcess$")
			helper.Env = append(env, tmuxInitTitleEnv+"="+title, tmuxInitSockEnv+"="+sock)
			stdin, err := helper.StdinPipe()
			require.NoError(t, err)
			stdout, err := helper.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, helper.Start())
			// Registered after isolatedTmux's, so it runs first: the server is
			// killed by its socket before the helper that reaps it goes.
			t.Cleanup(func() {
				_, _ = run("kill-server")
				_ = stdin.Close()
				_ = helper.Wait()
			})
			ready := make(chan string, 1)
			go func() {
				line, _ := bufio.NewReader(stdout).ReadString('\n')
				ready <- strings.TrimSpace(line)
			}()
			select {
			case line := <-ready:
				require.Equal(t, "READY", line, "the helper could not start its server")
			case <-time.After(10 * time.Second):
				t.Fatal("the helper never started its server")
			}

			server := serverPIDOf(t, run)
			reaper, err := Lookup(helper.Process.Pid)
			require.NoError(t, err)
			require.Equal(t, title, reaper.Comm, "fixture: the stand-in init carries a tmux task name")
			p, shape, err := lookupForkShape(server)
			require.NoError(t, err)
			require.Equal(t, helper.Process.Pid, p.PPID, "fixture: the daemonised server is reparented to the tmux init")
			require.Equal(t, server, p.SID, "fixture: daemon(3) leaves the server leading its own session")
			require.False(t, shape.ctty, "fixture: daemon(3) leaves the server no controlling terminal")

			require.True(t, IsTmuxServer(server), "a daemonised server whose parent is a tmux init is still a server")
		})
	}
}
