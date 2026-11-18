package tmux

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

// pinSocketBindingForTest stages what tmuxServerSocketBinding answers, for t.
func pinSocketBindingForTest(t *testing.T, binding func(pid int, socketPath string) (socketBinding, string)) {
	t.Helper()
	prev := tmuxServerSocketBinding
	tmuxServerSocketBinding = binding
	t.Cleanup(func() { tmuxServerSocketBinding = prev })
}

// pinServerIdentityForTest stages which pids count as positively a tmux server.
func pinServerIdentityForTest(t *testing.T, servers ...int) {
	t.Helper()
	prev := tmuxServerIdentified
	tmuxServerIdentified = func(pid int) bool {
		for _, s := range servers {
			if s == pid {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { tmuxServerIdentified = prev })
}

// pinReclaimWindowForTest sets how long a signalled owner is watched for.
func pinReclaimWindowForTest(t *testing.T, d time.Duration) {
	t.Helper()
	prev := socketReclaimWindow
	socketReclaimWindow = d
	t.Cleanup(func() { socketReclaimWindow = prev })
}

// Every candidate shape #7533's probe decides, with fake pids standing in for
// servers so each one is exactly the case its row names. The signal is recorded
// and never delivered. A row that the probe should answer without watching runs
// with a 30 s watch window and must return in well under it.
func TestTmuxSocketClaimedDecisionTable(t *testing.T) {
	const (
		notServer  = 9001 // passes the comm prefilter, not positively a server
		elsewhere  = 9002 // a server bound to another socket
		elsewhere2 = 9003
		unresolved = 9004 // a server whose sockets could not be read
		owner      = 9005 // a server bound to the missing socket
		owner2     = 9006
	)
	bindings := map[int]socketBinding{
		elsewhere: socketBindingElsewhere, elsewhere2: socketBindingElsewhere,
		unresolved: socketBindingUnresolved, owner: socketBindingBound, owner2: socketBindingBound,
	}
	for _, tc := range []struct {
		name        string
		pids        []int
		killErr     map[int]error
		recreatedBy int // the owner whose signal recreates the socket
		socketThere bool
		wantClaimed bool
		wantSignal  []int
		watches     bool
	}{
		{name: "no candidates: unclaimed", pids: nil},
		{name: "unreadable process table (0 sentinel): claimed", pids: []int{0}, wantClaimed: true},
		{name: "not positively a server: claimed, never signalled", pids: []int{notServer}, wantClaimed: true},
		{name: "every server bound elsewhere: unclaimed at once, nobody signalled", pids: []int{elsewhere, elsewhere2}},
		{name: "a server bound elsewhere, but the socket is back: claimed", pids: []int{elsewhere}, socketThere: true, wantClaimed: true},
		{name: "an unresolved server: claimed at once, never signalled", pids: []int{unresolved, elsewhere}, wantClaimed: true},
		{name: "the owner recreates the socket: claimed", pids: []int{elsewhere, owner}, recreatedBy: owner, wantClaimed: true, wantSignal: []int{owner}, watches: true},
		{name: "the owner leaves it absent: unclaimed", pids: []int{elsewhere, owner}, wantSignal: []int{owner}, watches: true},
		{name: "owner plus an unresolved server, socket left absent: claimed", pids: []int{owner, unresolved}, wantClaimed: true, wantSignal: []int{owner}, watches: true},
		{name: "owner plus a non-server, socket left absent: claimed", pids: []int{notServer, owner}, wantClaimed: true, wantSignal: []int{owner}, watches: true},
		{name: "the owner refuses the signal (EPERM): claimed", pids: []int{owner}, killErr: map[int]error{owner: syscall.EPERM}, wantClaimed: true, wantSignal: []int{owner}},
		{name: "the owner is gone before the signal (ESRCH): unclaimed", pids: []int{owner}, killErr: map[int]error{owner: syscall.ESRCH}, wantSignal: []int{owner}},
		{name: "two owners, one refuses: claimed", pids: []int{owner, owner2}, killErr: map[int]error{owner2: syscall.EACCES}, wantClaimed: true, wantSignal: []int{owner, owner2}, watches: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "default")
			if tc.socketThere {
				require.NoError(t, os.WriteFile(socket, nil, 0o600))
			}
			pinServerIdentityForTest(t, elsewhere, elsewhere2, unresolved, owner, owner2)
			pinSocketBindingForTest(t, func(pid int, path string) (socketBinding, string) {
				require.Equal(t, socket, path)
				b, ok := bindings[pid]
				require.True(t, ok, "binding read for pid %d, which is not a server", pid)
				if b == socketBindingElsewhere {
					return b, "/elsewhere/" + strconv.Itoa(pid)
				}
				if b == socketBindingBound {
					return b, path
				}
				return b, ""
			})
			var signalled []int
			pinServerProbeSignalForTest(t, func(pid int, sig syscall.Signal) error {
				require.Equal(t, syscall.SIGUSR1, sig)
				signalled = append(signalled, pid)
				if err := tc.killErr[pid]; err != nil {
					return err
				}
				if pid == tc.recreatedBy {
					require.NoError(t, os.WriteFile(socket, nil, 0o600))
				}
				return nil
			})
			window := 50 * time.Millisecond
			if !tc.watches {
				window = 30 * time.Second
			}
			pinReclaimWindowForTest(t, window)

			start := time.Now()
			require.Equal(t, tc.wantClaimed, tmuxSocketClaimed(socket, tc.pids))
			require.Equal(t, tc.wantSignal, signalled, "only a server bound to the missing socket is signalled")
			if !tc.watches {
				require.Less(t, time.Since(start), 10*time.Second, "nothing was left to watch, so the answer comes at once")
			}
		})
	}
}

// Each SIGUSR1 is logged with its target, the path that target is bound to, and
// the code that asked (#7533; #4678 and #6581 both lacked this for attribution).
func TestTmuxSocketClaimedLogsEachSignal(t *testing.T) {
	const owner, elsewhere = 9101, 9102
	socket := filepath.Join(t.TempDir(), "default")
	pinServerIdentityForTest(t, owner, elsewhere)
	pinSocketBindingForTest(t, func(pid int, path string) (socketBinding, string) {
		if pid == owner {
			return socketBindingBound, path
		}
		return socketBindingElsewhere, "/elsewhere"
	})
	pinServerProbeSignalForTest(t, func(int, syscall.Signal) error { return nil })
	pinReclaimWindowForTest(t, 10*time.Millisecond)
	info := &logtest.Buffer{}
	aflog.CaptureOutput(t, aflog.InfoLog, "INFO:", info)

	tmuxSocketClaimed(socket, []int{elsewhere, owner})

	var lines []string
	for _, line := range strings.Split(info.String(), "\n") {
		if strings.Contains(line, "SIGUSR1 to tmux server") {
			lines = append(lines, line)
		}
	}
	require.Len(t, lines, 1, "one line per SIGUSR1, and none for the server bound elsewhere: %q", info.String())
	require.Contains(t, lines[0], "SIGUSR1 to tmux server pid 9101, which is bound to "+socket)
	require.Contains(t, lines[0], "tmux socket "+socket+" is missing")
	require.Contains(t, lines[0], "asked by ")
	require.Contains(t, lines[0], "TestTmuxSocketClaimedLogsEachSignal", "the caller is the code that asked, not the probe")
}

// The refusal names every candidate the probe could not rule out, and only
// those.
func TestPossibleSocketOwners(t *testing.T) {
	const notServer, elsewhere, unresolved, owner = 9201, 9202, 9203, 9204
	pinServerIdentityForTest(t, elsewhere, unresolved, owner)
	pinSocketBindingForTest(t, func(pid int, path string) (socketBinding, string) {
		switch pid {
		case elsewhere:
			return socketBindingElsewhere, "/elsewhere"
		case owner:
			return socketBindingBound, path
		default:
			return socketBindingUnresolved, ""
		}
	})
	require.Equal(t, []int{0, notServer, unresolved, owner},
		possibleSocketOwners("/tmp/tmux-1000/default", []int{0, notServer, elsewhere, unresolved, owner}))
	require.Empty(t, possibleSocketOwners("/tmp/tmux-1000/default", []int{elsewhere}))
}

// Two spellings of one socket agree, through a symlinked directory, after the
// socket file is gone, and after its directory is gone too.
func TestSameSocketPath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	real := filepath.Join(root, "real")
	require.NoError(t, os.Mkdir(real, 0o700))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(real, link))

	require.True(t, sameSocketPath(filepath.Join(real, "s"), filepath.Join(link, "s")), "a symlinked directory")
	require.True(t, sameSocketPath(filepath.Join(real, "s"), filepath.Join(real, ".", "x", "..", "s")), "an uncleaned spelling")
	require.True(t, sameSocketPath(filepath.Join(link, "gone", "s"), filepath.Join(real, "gone", "s")),
		"a missing directory under a symlinked one")
	require.False(t, sameSocketPath(filepath.Join(real, "s"), filepath.Join(real, "t")), "another socket in the same directory")
	require.False(t, sameSocketPath(filepath.Join(real, "s"), filepath.Join(root, "s")), "the same name in another directory")

	// ".." after a symlink is the symlink TARGET's parent (Codex on #7537):
	// with child -> real/inner, root/child/../s is real/s, not root/s. Lexical
	// cleaning first would rule a bound owner out.
	require.NoError(t, os.Mkdir(filepath.Join(real, "inner"), 0o700))
	child := filepath.Join(root, "child")
	require.NoError(t, os.Symlink(filepath.Join(real, "inner"), child))
	dotdot := child + "/../s"
	require.True(t, sameSocketPath(dotdot, filepath.Join(real, "s")), "a .. after a symlink resolves through it")
	require.False(t, sameSocketPath(dotdot, filepath.Join(root, "s")), "and is not the lexically cleaned path")
	require.True(t, sameSocketPath(child+"/../gone/s", filepath.Join(real, "gone", "s")),
		"the same, below a directory that no longer exists")

	// A dangling symlink is still followed (Codex on #7537): a client that
	// reached the socket through alias.sock -> actual.sock names the server
	// bound to actual.sock once that file is unlinked. Absolute and relative
	// link targets, and a directory link whose target is gone.
	require.NoError(t, os.Symlink(filepath.Join(root, "actual.sock"), filepath.Join(root, "alias.sock")))
	require.True(t, sameSocketPath(filepath.Join(root, "alias.sock"), filepath.Join(root, "actual.sock")), "a dangling socket alias")
	require.False(t, sameSocketPath(filepath.Join(root, "alias.sock"), filepath.Join(root, "other.sock")))
	require.NoError(t, os.Symlink("actual2.sock", filepath.Join(root, "relalias.sock")))
	require.True(t, sameSocketPath(filepath.Join(root, "relalias.sock"), filepath.Join(root, "actual2.sock")), "a relative dangling alias")
	require.NoError(t, os.Symlink(filepath.Join(root, "gone-dir"), filepath.Join(root, "dirlink")))
	require.True(t, sameSocketPath(filepath.Join(root, "dirlink", "s"), filepath.Join(root, "gone-dir", "s")), "a dangling directory link")
}

// A server is ruled out only by a certain mismatch. A symlink loop on either
// side makes the comparison uncertain, which leaves the server unresolved.
func TestBindingAgainst(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Symlink(filepath.Join(root, "actual.sock"), filepath.Join(root, "alias.sock")))
	loopA, loopB := filepath.Join(root, "loopa"), filepath.Join(root, "loopb")
	require.NoError(t, os.Symlink(loopB, loopA))
	require.NoError(t, os.Symlink(loopA, loopB))
	_, ok := canonicalSocketPath(loopA + "/s")
	require.False(t, ok, "a symlink loop resolves to nothing certain")

	for _, tc := range []struct {
		name   string
		target string
		bound  []string
		want   socketBinding
	}{
		{"bound through a dangling alias", root + "/alias.sock", []string{root + "/x.sock", root + "/actual.sock"}, socketBindingBound},
		{"bound elsewhere", root + "/alias.sock", []string{root + "/x.sock"}, socketBindingElsewhere},
		{"a bound path that loops", root + "/s", []string{loopA + "/s"}, socketBindingUnresolved},
		{"a looping bound path beside a certain mismatch", root + "/s", []string{root + "/x.sock", loopA + "/s"}, socketBindingUnresolved},
		{"a looping bound path beside the owner's", root + "/s", []string{loopA + "/s", root + "/s"}, socketBindingBound},
		{"a target that loops", loopA + "/s", []string{root + "/s"}, socketBindingUnresolved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := bindingAgainst(tc.target, tc.bound)
			require.Equal(t, tc.want, got)
		})
	}
}

// The real binding read, against this process's own listener: bound to its
// path, elsewhere for any other, unresolved for a relative path and for a
// process that holds no listener at all.
func TestReadTmuxServerSocketBinding(t *testing.T) {
	if _, err := proctree.UnixListenerPaths(os.Getpid()); errors.Is(err, proctree.ErrUnsupportedPlatform) {
		t.Skip("no unix-socket backend on this platform")
	}
	dir, err := os.MkdirTemp("/tmp", "af-7533-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	b, bound := readTmuxServerSocketBinding(os.Getpid(), sock)
	require.Equal(t, socketBindingBound, b)
	require.True(t, sameSocketPath(bound, sock))

	b, _ = readTmuxServerSocketBinding(os.Getpid(), filepath.Join(dir, "other"))
	require.Equal(t, socketBindingElsewhere, b)

	b, _ = readTmuxServerSocketBinding(os.Getpid(), "l")
	require.Equal(t, socketBindingUnresolved, b, "a relative path cannot be placed")

	sleeper := exec.Command("sleep", "30")
	require.NoError(t, sleeper.Start())
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	b, _ = readTmuxServerSocketBinding(sleeper.Process.Pid, sock)
	require.Equal(t, socketBindingUnresolved, b, "a process with no listener names nothing to compare")
}

// socketServer starts a tmux server bound to sock and returns its pid and a
// runner for it. Every command names the socket with -S, so nothing reaches
// another server, and the cleanup kills this one by its socket after
// recreating it if a test removed it (SIGUSR1, sent only to a pid that is still
// positively a tmux server).
func socketServer(t *testing.T, sock string) (pid int, run func(args ...string) ([]byte, error)) {
	t.Helper()
	run = func(args ...string) ([]byte, error) {
		return exec.Command("tmux", append([]string{"-S", sock, "-f", "/dev/null"}, args...)...).Output()
	}
	out, err := exec.Command("tmux", "-S", sock, "-f", "/dev/null", "new-session", "-d", "-s", "x", "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux -S %s new-session: %s", sock, out)
	out, err = run("display-message", "-p", "#{pid}")
	require.NoError(t, err)
	pid, err = strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := os.Stat(sock); err != nil && proctree.IsTmuxServer(pid) {
			_ = syscall.Kill(pid, syscall.SIGUSR1)
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if _, err := os.Stat(sock); err == nil {
					break
				}
			}
		}
		if out, err := run("display-message", "-p", "#{pid}"); err == nil && strings.TrimSpace(string(out)) == strconv.Itoa(pid) {
			_, _ = run("kill-server")
		}
	})
	require.Eventually(t, func() bool { return proctree.IsTmuxServer(pid) }, 5*time.Second, 25*time.Millisecond,
		"the server on %s must become positively identifiable", sock)
	return pid, run
}

// #7533, end to end on real servers (Linux and macOS). Server A's socket is
// unlinked; server B is unrelated. Probing A's path signals A, which recreates
// it, and never B. Probing a path no server ever bound signals nobody and
// answers "unclaimed" at once, with no watch. Before #7533 both probes sent
// SIGUSR1 to every server, B included, and B briefly unlinked and rebound its
// own socket under its clients.
func TestSocketAbsentProbeSignalsOnlyTheServerBoundToTheMissingSocket(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := os.Getenv("TMUX_TMPDIR")
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	serverA, runA := socketServer(t, a)
	serverB, _ := socketServer(t, b)
	t.Cleanup(PinServerProbeForTest(serverA, serverB))

	var mu sync.Mutex
	var signalled []int
	pinServerProbeSignalForTest(t, func(pid int, sig syscall.Signal) error {
		mu.Lock()
		signalled = append(signalled, pid)
		mu.Unlock()
		return syscall.Kill(pid, sig)
	})

	require.NoError(t, os.Remove(a), "stage A's unlinked socket")
	_, err := runA("ls")
	require.Error(t, err)
	diagnostic, exitOne := tmuxExitOneDiagnostic(err)
	require.True(t, exitOne, "fixture: tmux exits 1 on A's missing socket: %v", err)
	require.Equal(t, socketAbsent, classifyNoServerDiagnostic(diagnostic), "fixture: tmux reports A's socket missing: %q", diagnostic)
	require.False(t, NoServerRunning(err), "A is bound to the missing socket and recreates it: the socket is claimed")
	require.Equal(t, []int{serverA}, signalled, "only A, the server bound to the missing socket, is signalled")
	_, statErr := os.Stat(a)
	require.NoError(t, statErr, "A recreated its socket")

	mu.Lock()
	signalled = nil
	mu.Unlock()
	pinReclaimWindowForTest(t, 30*time.Second)
	never := filepath.Join(dir, "never")
	_, err = exec.Command("tmux", "-S", never, "-f", "/dev/null", "ls").Output()
	require.Error(t, err)
	start := time.Now()
	require.True(t, NoServerRunning(err), "no live server is bound to a path nobody ever bound")
	require.Less(t, time.Since(start), 10*time.Second, "nothing was signalled, so nothing was watched")
	require.Empty(t, signalled, "neither server is bound to it, so neither is signalled")
}
