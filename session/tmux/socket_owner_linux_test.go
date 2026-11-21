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
		// wantComm is the kernel task name the server settles into — each
		// one a shape a title rule misses: past the 15-byte task-name
		// truncation, or reading like a client.
		wantComm string
	}{
		{name: "two-character name", argv0: "ab", wantComm: "ab: server"},
		{name: "ten-character name", argv0: "abcdefghij", wantComm: "abcdefghij: ser"},
		{name: "sixteen-character name, truncated", argv0: "abcdefghijklmnop", wantComm: "abcdefghijklmno"},
		{name: "a title that reads like a client", argv0: "a: client", wantComm: "a: client: ser"},
		{name: "a renamed binary copy", argv0: "", wantComm: "bx: server"},
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

			require.Eventually(t, func() bool {
				p, lerr := proctree.Lookup(serverPID)
				return lerr == nil && p.Comm == tc.wantComm
			}, 5*time.Second, 10*time.Millisecond,
				"the server must retitle to %q — the shape title matching missed", tc.wantComm)

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
			require.True(t, tied, "the kernel's tables must tie the retitled server (%q) to its unlinked socket", tc.wantComm)
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
	// Wait for the non-dumpable state to take: its fd dir must refuse before
	// the sweep runs, or the fixture proves nothing.
	require.Eventually(t, func() bool {
		_, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(nd.Process.Pid), "fd"))
		return err != nil && os.IsPermission(err)
	}, 5*time.Second, 10*time.Millisecond, "the fixture must become non-dumpable")

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
