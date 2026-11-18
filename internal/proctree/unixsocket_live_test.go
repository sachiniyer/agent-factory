//go:build linux || darwin

package proctree

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// socketTmux starts a tmux server bound to sock, with cmd.Dir set to dir so a
// relative sock resolves there, and returns its pid. Every command names the
// socket with -S and runs without $TMUX or $TMUX_TMPDIR, so nothing here can
// reach any other tmux server; the cleanup kills this one by its socket. The
// cleanup reads the pid and socket first, so it names what it kills.
func socketTmux(t *testing.T, dir, sock string) int {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TMUX_TMPDIR=") {
			env = append(env, kv)
		}
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, append([]string{"-S", sock, "-f", "/dev/null"}, args...)...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	out, err := run("new-session", "-d", "-s", "keep", "sleep 600")
	require.NoError(t, err, out)
	out, err = run("display-message", "-p", "#{pid}")
	require.NoError(t, err, out)
	pid, err := strconv.Atoi(out)
	require.NoError(t, err, "server pid %q", out)
	t.Cleanup(func() {
		// A test may have removed the socket. SIGUSR1 makes the server recreate
		// it, so kill-server can reach it; it is sent only to a pid that is still
		// positively this tmux server, never to a pid that may have been reused.
		if _, err := os.Stat(socketAt(dir, sock)); err != nil && IsTmuxServer(pid) {
			_ = syscall.Kill(pid, syscall.SIGUSR1)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(socketAt(dir, sock)); err == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		if p, err := run("display-message", "-p", "#{pid}"); err == nil && p == strconv.Itoa(pid) {
			_, _ = run("kill-server")
		}
	})
	return pid
}

// socketAt is sock as seen from dir.
func socketAt(dir, sock string) string {
	if filepath.IsAbs(sock) {
		return sock
	}
	return filepath.Join(dir, sock)
}

// privateSocketDir is a fresh directory under /tmp, not t.TempDir(): sun_path
// holds 104 bytes on darwin, and a macOS runner's TMPDIR alone is ~50.
func privateSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "af-uls-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	real, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return real
}

// #7533: the path a tmux server is bound to is read from the kernel, and it
// survives the unlink of the socket file. That is the fact the socket-absent
// probe needs: which server would recreate this missing socket. On darwin this
// is also the runner's check of the PROC_PIDFDSOCKETINFO decoder against a real
// tmux, path for path.
func TestUnixListenerPaths_ATmuxServerKeepsItsPathAfterTheSocketIsRemoved(t *testing.T) {
	dir := privateSocketDir(t)
	sock := filepath.Join(dir, "s")
	server := socketTmux(t, dir, sock)

	paths, err := UnixListenerPaths(server)
	require.NoError(t, err)
	require.Equal(t, []string{sock}, paths, "a tmux server listens on exactly its own socket")

	require.NoError(t, os.Remove(sock))
	paths, err = UnixListenerPaths(server)
	require.NoError(t, err)
	require.Equal(t, []string{sock}, paths, "the bound path outlives the socket file")
}

// A relative -S path is bound as given; it resolves against the server's
// working directory, which is where tmux itself would rebind it on SIGUSR1.
func TestUnixListenerPaths_ARelativeSocketResolvesAgainstTheServersWorkingDirectory(t *testing.T) {
	dir := privateSocketDir(t)
	server := socketTmux(t, dir, "./r")

	paths, err := UnixListenerPaths(server)
	require.NoError(t, err)
	require.Equal(t, []string{dir + "/./r"}, paths, "the bound spelling, prefixed with the working directory and not cleaned")
}

// Only listeners count: this process's own listener is reported, and the
// connection it accepts, which carries the same path on Linux, is not.
func TestUnixListenerPaths_OnlyListenersAndOnlyTheirOwnPaths(t *testing.T) {
	dir := privateSocketDir(t)
	sock := filepath.Join(dir, "l")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	client, err := net.Dial("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	accepted, err := ln.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = accepted.Close() })

	paths, err := UnixListenerPaths(os.Getpid())
	require.NoError(t, err)
	require.Contains(t, paths, sock)
	n := 0
	for _, p := range paths {
		if p == sock {
			n++
		}
	}
	require.Equal(t, 1, n, "the listener is reported once; the accepted connection and the client are not listeners")
}

// A read is complete only if the process it began on is still the one running
// when it ends (#7543 review R3). A process that exits part-way makes its later
// descriptors read as closed and be skipped, so the list would look complete.
// The lookups bracketing the read are stubbed here so the process exits,
// becomes a zombie, or is replaced by another with its pid, between them.
func TestUnixSocketsAnswersExitedWhenTheProcessChangesMidRead(t *testing.T) {
	dir := privateSocketDir(t)
	sock := filepath.Join(dir, "m")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	real, err := Lookup(os.Getpid())
	require.NoError(t, err)

	readers := map[string]func(int) error{
		"UnixSockets": func(pid int) error { _, err := UnixSockets(pid); return err },
	}
	if _, err := UnixListenerPathsByTable(os.Getpid()); !errors.Is(err, ErrUnsupportedPlatform) {
		readers["UnixListenerPathsByTable"] = func(pid int) error { _, err := UnixListenerPathsByTable(pid); return err }
	}
	denied := errors.New("lookup refused")
	for _, tc := range []struct {
		name          string
		before, after func() (Process, error)
		want          error // nil means the read must succeed
	}{
		{"the same instance throughout", nil, nil, nil},
		{"exited by the end (no /proc entry)", nil, func() (Process, error) { return Process{}, os.ErrNotExist }, ErrProcessExited},
		{"a zombie by the end", nil, func() (Process, error) { return Process{}, ErrProcessExited }, ErrProcessExited},
		{"replaced by another process with the pid", nil, func() (Process, error) {
			p := real
			p.StartID++
			return p, nil
		}, ErrProcessExited},
		{"gone before the read began", func() (Process, error) { return Process{}, os.ErrNotExist }, nil, ErrProcessExited},
		{"a lookup refused for another reason", nil, func() (Process, error) { return Process{}, denied }, denied},
	} {
		for rname, read := range readers {
			t.Run(rname+": "+tc.name, func(t *testing.T) {
				calls := 0
				prev := instanceLookup
				instanceLookup = func(pid int) (Process, error) {
					calls++
					if calls == 1 && tc.before != nil {
						return tc.before()
					}
					if calls == 2 && tc.after != nil {
						return tc.after()
					}
					return prev(pid)
				}
				t.Cleanup(func() { instanceLookup = prev })
				err := read(os.Getpid())
				if tc.want == nil {
					require.NoError(t, err)
					require.Equal(t, 2, calls, "the read is bracketed by one lookup on each side")
					return
				}
				require.ErrorIs(t, err, tc.want)
				if !errors.Is(tc.want, ErrProcessExited) {
					require.NotErrorIs(t, err, ErrProcessExited, "a refused lookup is not an exit")
				}
			})
		}
	}
}
