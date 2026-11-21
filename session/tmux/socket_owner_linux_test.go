//go:build linux

package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// The #7695 acceptance cases, end to end on real tmux servers: every title
// shape a task-name rule has ever missed — an argv[0] of 2, 10 and 16 chars
// (the last past tmux's 15-byte task-name truncation), a renamed binary
// copy, and a title that reads like a client's — is found by bound-socket
// evidence, and a socket unlinked under the live server never reads as a
// determinate empty.
//
// Each case starts a server on a private -S socket, waits for the retitle
// that broke the title rules, unlinks the socket, and asserts the two
// halves of the verdict: the kernel's tables tie the server to the path
// (sweep.bound), and NoServerRunning refuses the determinate empty — with
// nobody signalled, because a renamed server is not positively identified.
func TestSocketOwnerSweepFindsARenamedServerByBinding(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	tmuxPath, err := exec.LookPath("tmux")
	require.NoError(t, err)

	cases := []struct {
		name string
		// The server's launch argv[0]: exec -a rewrites it (the task title
		// follows getprogname()), a binary copy carries it on disk.
		argv0 string
		// commPrefix is the leading fragment of the kernel task name the
		// server retitles to — tmux spells it "<argv0>: server", truncates
		// to 15 bytes, then cuts at the last space, so "abcdefghij" reads
		// "abcdefghij:" and "a: client" reads "a: client:" (measured on
		// tmux 3.5). A prefix check is what the shape needs: never
		// "tmux"-led, so every title rule misses it.
		commPrefix string
	}{
		{name: "two-character name", argv0: "ab", commPrefix: "ab: server"},
		{name: "ten-character name", argv0: "abcdefghij", commPrefix: "abcdefghij"},
		{name: "sixteen-character name, truncated", argv0: "abcdefghijklmnop", commPrefix: "abcdefghijklmno"},
		{name: "a title that reads like a client", argv0: "a: client", commPrefix: "a: client"},
		{name: "a renamed binary copy", argv0: "", commPrefix: "bx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := testguard.SocketPath(t, "owner.sock")

			var launcher string
			var prefix []string
			if tc.argv0 == "" {
				// A copy of the tmux binary named otherwise, as a user has on
				// disk: the file name is argv[0], so the server retitles to it.
				src, err := os.ReadFile(tmuxPath)
				require.NoError(t, err)
				launcher = filepath.Join(t.TempDir(), "bx")
				require.NoError(t, os.WriteFile(launcher, src, 0o755))
			} else {
				prefix = []string{"-a", tc.argv0, tmuxPath}
				launcher = "exec"
			}

			args := append(prefix, "-S", sock, "new-session", "-d", "-s", "af_owns", "sleep 600")
			var out []byte
			if launcher == "exec" {
				out, err = exec.Command("bash", "-c",
					"exec "+strings.Join(shellQuoteEach(args), " ")).CombinedOutput()
			} else {
				out, err = exec.Command(launcher, args...).CombinedOutput()
			}
			require.NoError(t, err, "server launch: %s", out)

			out, err = exec.Command(tmuxPath, "-S", sock, "display-message", "-p", "#{pid}").CombinedOutput()
			require.NoError(t, err, "display-message: %s", out)
			serverPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
			require.NoError(t, err)
			t.Cleanup(func() {
				// The socket may be unlinked by then; kill by pid, never a bare
				// tmux command.
				_ = syscall.Kill(serverPID, syscall.SIGTERM)
				_ = exec.Command(tmuxPath, "-S", sock, "kill-server").Run()
			})

			var comm string
			require.Eventually(t, func() bool {
				p, lerr := proctree.Lookup(serverPID)
				if lerr != nil {
					return false
				}
				comm = p.Comm
				return strings.HasPrefix(comm, tc.commPrefix) &&
					!strings.HasPrefix(comm, "tmux")
			}, 5*time.Second, 10*time.Millisecond,
				"the server must retitle away from 'tmux' under its argv[0] (now %q) — the shape title matching missed", comm)

			// Nobody may be signalled: a renamed server is never positively
			// identified, and nothing else is bound to a private path.
			var signalled []int
			pinServerProbeSignalForTest(t, func(pid int, sig syscall.Signal) error {
				signalled = append(signalled, pid)
				return syscall.Kill(pid, sig)
			})

			require.NoError(t, os.Remove(sock), "unlink the socket under the live server")

			sweep := probeSocketOwners(sock)
			owner, tied := sweep.bound[serverPID]
			require.True(t, tied, "the kernel's tables must tie the retitled server (%q) to its unlinked socket", comm)
			require.NotEmpty(t, owner.spelling)

			_, err = exec.Command(tmuxPath, "-S", sock, "ls").Output()
			require.Error(t, err)
			diag, ok := tmuxExitOneDiagnostic(err)
			require.True(t, ok, "tmux ls must exit 1 on the unlinked socket")
			require.Equal(t, socketAbsent, classifyNoServerDiagnostic(diag))
			require.False(t, NoServerRunning(err),
				"a live server whose socket was unlinked must refuse the determinate empty")
			require.Empty(t, signalled, "a bound pid that is not positively a server is never signalled")
		})
	}
}

func shellQuoteEach(args []string) []string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return quoted
}

// fdTableRefuses reports whether pid's fd table refuses this uid: the
// directory listing denied, or — root in a container without
// CAP_SYS_PTRACE can LIST yet still refuse the read — an fd link that
// will not resolve. The latter is the refusal the sweep's attribution
// scan actually hits.
func fdTableRefuses(pid int) bool {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		return os.IsPermission(err)
	}
	for _, e := range ents {
		if _, lerr := os.Readlink(filepath.Join(fdDir, e.Name())); os.IsPermission(lerr) {
			return true
		}
	}
	return false
}

// waitNonDumpable waits for pid's non-dumpable state to take: the fd table
// must refuse before the sweep runs, or the fixture proves nothing. Where
// this uid bypasses the ptrace access check entirely (root with
// CAP_SYS_PTRACE, hidepid off) the refusal never comes: the fd table reads
// in full past the deadline, the attribution scan works anyway, and a test
// premised on the refusal skips rather than fails.
func waitNonDumpable(t *testing.T, pid int) {
	t.Helper()
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	deadline := time.Now().Add(5 * time.Second)
	for !fdTableRefuses(pid) {
		_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
		require.NoError(t, err, "the fixture died")
		if time.Now().After(deadline) {
			// Alive and never refused: this uid bypasses the ptrace access
			// check the fixture exists to exercise.
			ents, rerr := os.ReadDir(fdDir)
			require.NoError(t, rerr)
			require.NotEmpty(t, ents)
			t.Skip("this uid reads a non-dumpable process's fd table: the access " +
				"check the fixture exists to exercise does not apply")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// skipIfFdTableReadable skips a test premised on a non-dumpable fd table
// refusing, where this uid bypasses the check the fixture exists to
// exercise — the fixture already proved its state by binding its socket.
func skipIfFdTableReadable(t *testing.T, pid int) {
	t.Helper()
	if fdTableRefuses(pid) {
		return
	}
	ents, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	require.NoError(t, err)
	require.NotEmpty(t, ents)
	t.Skip("this uid reads a non-dumpable process's fd table: the access check " +
		"the fixture exists to exercise does not apply")
}

// The R1 regression, pinned: a same-uid process whose /proc/<pid>/fd and
// /proc/<pid>/exe refuse (PR_SET_DUMPABLE=0 — ssh-agent's shape) bound
// NOTHING, and must not make every missing path read unknown. Its netns
// table still reads, so the sweep's negative stays provable while it lives.
func TestSocketOwnerSweepStaysDeterminateBesideANonDumpableProcess(t *testing.T) {
	nd := exec.Command(sleeper.NonDumpablePath(t), "-", "60")
	require.NoError(t, nd.Start())
	t.Cleanup(func() {
		_ = nd.Process.Kill()
		_, _ = nd.Process.Wait()
	})
	// Wait for the non-dumpable state to take: the fd table must refuse
	// before the sweep runs, or the fixture proves nothing.
	waitNonDumpable(t, nd.Process.Pid)

	neverBound := testguard.SocketPath(t, "never-bound.sock")
	sweep := probeSocketOwners(neverBound)
	require.False(t, sweep.unprobed)
	require.Empty(t, sweep.uncovered,
		"a live same-uid process whose fd table refuses is still covered by its netns table")
	require.Empty(t, sweep.bound)
	require.Empty(t, sweep.uncertain)
	require.Zero(t, sweep.unattributedBound)
	require.Zero(t, sweep.unattributedUncertain)
	require.False(t, socketClaimedByProbe(neverBound),
		"a non-dumpable process that bound nothing must not hold every path unknown — the #7689 R1 shape")
}

// The other half: the non-dumpable process binds the socket itself, then it
// is unlinked. The bound row is the kernel's positive tie, but the holder's
// fd table cannot be read, so no pid may be named: unattributed — and the
// answer is claimed, never a determinate empty.
func TestSocketOwnerSweepNamesAnUnattributedBoundHolder(t *testing.T) {
	sock := testguard.SocketPath(t, "holder.sock")
	nd := exec.Command(sleeper.NonDumpablePath(t), sock, "60")
	require.NoError(t, nd.Start())
	t.Cleanup(func() {
		_ = nd.Process.Kill()
		_, _ = nd.Process.Wait()
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "the fixture must bind its socket")
	// The bind proves the prctl ran. Where this uid bypasses the access check
	// the attribution scan names the holder and the premise skips.
	skipIfFdTableReadable(t, nd.Process.Pid)
	require.NoError(t, os.Remove(sock), "unlink the socket under the non-dumpable holder")

	require.Eventually(t, func() bool {
		sweep := probeSocketOwners(sock)
		return sweep.unattributedBound > 0
	}, 5*time.Second, 10*time.Millisecond,
		"the kernel's tables must show the bound row whose holder's fds refuse")
	sweep := probeSocketOwners(sock)
	require.Empty(t, sweep.bound, "no pid may be named for a holder whose fd table refuses")
	require.False(t, noServerRunningFromAbsent(t, sock),
		"an unattributed bound holder is a possible owner: claimed, never a determinate empty")
}

// noServerRunningFromAbsent builds the ENOENT diagnostic the probe answers
// for a missing socket and asks it, for tests that never exec tmux.
func noServerRunningFromAbsent(t *testing.T, sock string) bool {
	t.Helper()
	return NoServerRunning(tmuxExitOneError(t,
		"error connecting to "+sock+" (No such file or directory)"))
}
