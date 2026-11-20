package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// A tmux server that holds NO sessions answers every targeted query with `no
// current target` (#3469). cmd_find_target fails to establish a current session
// before it ever looks at `-t`, so the diagnostic names neither the session nor
// a socket, and the two spellings missingTmuxSession matches — `can't find
// session: <name>` and `no server running on <socket>` — both miss it.
//
// The tests below hold the server in that state deliberately with `exit-empty
// off`, tmux's documented option for keeping a server up with no sessions. That
// is not a contrivance to reach a corner: a server passes through the same state
// on its way out after its last session is killed, which is the intermittent form
// this suite kept failing on, and users who set exit-empty off live there
// permanently. Pinning it makes the classification testable without a race.

// sessionlessTmuxServer leaves the test's private tmux server running with zero
// sessions and returns a session name that is definitively not on it.
func sessionlessTmuxServer(t *testing.T) string {
	t.Helper()
	testguard.IsolateTmux(t)

	const name = "af_sessionless_server_probe"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	// Server option: without it the server exits with its last session and the
	// probes below would race the shutdown instead of observing the state.
	out, err = exec.Command("tmux", "set", "-s", "exit-empty", "off").CombinedOutput()
	require.NoError(t, err, "tmux set -s exit-empty off: %s", out)
	out, err = exec.Command("tmux", "kill-session", "-t", ExactTarget(name)).CombinedOutput()
	require.NoError(t, err, "tmux kill-session: %s", out)

	// The precondition, asserted rather than assumed: the server is still up
	// (an authoritative listing) and holds nothing.
	names, err := ListSessionNames(cmd.MakeExecutor())
	require.NoError(t, err, "the private server must still answer after its last session was killed")
	require.Empty(t, names, "the private server must hold no sessions")
	return name
}

// probeSessionStrict is the gate CleanupSessions consults before it may treat a
// session as gone. On a sessionless server it used to report UNKNOWN, so `af
// reset` refused to clean up a session that was definitively absent.
func TestProbeSessionStrictConfirmsAbsenceOnSessionlessServer(t *testing.T) {
	name := sessionlessTmuxServer(t)

	exists, known, err := probeSessionStrict(cmd.MakeExecutor(), name)
	require.NoError(t, err, "a server that answered must not leave the session unknown")
	require.True(t, known, "a server holding no sessions has definitively answered about this one")
	require.False(t, exists)
}

// The same answer reaches the pre-teardown capture. Misclassified, it degrades
// to a generic "cannot list panes" failure, and the vanished-session recovery
// that sweeps marked orphans never runs.
func TestCaptureSessionProcessTreesReportsVanishedOnSessionlessServer(t *testing.T) {
	name := sessionlessTmuxServer(t)

	procs, err := CaptureSessionProcessTrees(cmd.MakeExecutor(), name)
	require.Empty(t, procs)
	require.ErrorIs(t, err, ErrSessionVanishedBeforeCapture,
		"a session absent from a server that answered must classify as vanished, not as an unreadable capture")
}

// The corroboration contract, without tmux: an exit-1 answer this package cannot
// classify is resolved by tmux's own listing, and ONLY by an authoritative one.
func TestTmuxProvedSessionAbsentCorroboratesWithTheSessionListing(t *testing.T) {
	const name = "af_corroborated"

	for _, tc := range []struct {
		label    string
		failure  error
		listing  []byte
		listErr  error
		expected bool
	}{
		{
			label:    "unclassified exit 1, listing omits the session",
			failure:  tmuxExitOneError(t, "no current target"),
			listing:  []byte(""),
			expected: true,
		},
		{
			label:    "unclassified exit 1, listing still holds the session",
			failure:  tmuxExitOneError(t, "no current target"),
			listing:  []byte("af_other\n" + name + "\n"),
			expected: false,
		},
		{
			label:    "unclassified exit 1, listing names other sessions only",
			failure:  tmuxExitOneError(t, "server exited unexpectedly"),
			listing:  []byte("af_other\naf_" + name + "\n" + name + "_suffix\n"),
			expected: true,
		},
		{
			label:   "unclassified exit 1, listing did not answer",
			failure: tmuxExitOneError(t, "no current target"),
			listErr: errors.New("injected listing failure"),
		},
		{
			label:    "tmux's own absence diagnostic still needs no listing",
			failure:  tmuxExitOneError(t, "can't find session: "+name),
			listErr:  errors.New("the listing must not be consulted"),
			expected: true,
		},
		{
			label:   "a status tmux documents no diagnostic for is never absence",
			failure: tmuxExitTwoError(t, "no current target"),
			listing: []byte(""),
		},
		{
			label:   "not an exit error at all",
			failure: errors.New("fork/exec tmux: resource temporarily unavailable"),
			listing: []byte(""),
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			cmdExec := cmd_test.MockCmdExec{
				RunFunc: func(*exec.Cmd) error { return nil },
				OutputFunc: func(command *exec.Cmd) ([]byte, error) {
					require.Equal(t, "ls", command.Args[1],
						"only the session listing may be issued to corroborate a failed read")
					return tc.listing, tc.listErr
				},
			}
			require.Equal(t, tc.expected, tmuxProvedSessionAbsent(cmdExec, tc.failure, name, ""))
		})
	}
}

// The #2875 orphan state itself, end to end: a live server whose socket was
// removed answers ENOENT to every client, but SIGUSR1 — tmux's documented
// recreate-socket signal — is the recovery, so the strict probe signals the
// live server, watches the socket reappear, and gets the REAL answer rather
// than reporting unknown forever. The server is bound to the unlinked path, so
// it is the one candidate signalled (#7533). The pin keeps the probe's reads to
// the test's own private server rather than every same-uid server on the host.
func TestProbeSessionStrictRevivesAnOrphanedSocket(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af_orphaned_socket_probe"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)

	out, err = exec.Command("tmux", "display-message", "-p", "#{pid}").CombinedOutput()
	require.NoError(t, err, "tmux display-message: %s", out)
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err, "the private server's pid must be parseable: %q", out)
	t.Cleanup(PinServerProbeForTest(pid))

	socket := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	require.NoError(t, os.Remove(socket), "stage the unlinked socket")

	exists, known, err := probeSessionStrict(cmd.MakeExecutor(), name)
	require.NoError(t, err, "a revived server answers for real, not with an unclassifiable error")
	require.True(t, known, "the orphaned server was found by revival, so its answer is determinate")
	require.True(t, exists, "the session is alive on the orphaned server and must be reported")
}

// The other half of the same mechanism: a socket NO live server claims is a
// determinate empty, even while unrelated tmux servers run for this uid — the
// `tmux -L`-user case that used to report unknown forever. The neighbour
// server is real: its listening socket is read and found bound to its own named
// socket inside the same private dir, so it is ruled out without a signal
// (#7533). A neighbour whose sockets could not be read would honestly leave the
// probe unknown.
func TestProbeSessionStrictConfirmsAbsenceWhenNoServerClaimsTheSocket(t *testing.T) {
	testguard.IsolateTmux(t)

	out, err := exec.Command("tmux", "-L", "af_neighbour", "new-session", "-d", "-s", "x", "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux -L new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", "af_neighbour", "kill-server").Run() })
	out, err = exec.Command("tmux", "-L", "af_neighbour", "display-message", "-p", "#{pid}").CombinedOutput()
	require.NoError(t, err, "tmux display-message: %s", out)
	neighbour, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	t.Cleanup(PinServerProbeForTest(neighbour))

	// The default socket inside the private world has never had a server: the
	// only live server owns the named socket next to it.
	exists, known, err := probeSessionStrict(cmd.MakeExecutor(), "af_never_existed")
	require.NoError(t, err, "an unclaimed socket is proved absent, not left unknown")
	require.True(t, known)
	require.False(t, exists)
}

// The partial-signaling case the doc comment describes and the code got wrong
// before this fix: a pid list mixing a signallable server (which recreates only
// ITS socket) with a prefilter-passing candidate proctree.IsTmuxServer rejects
// as not a daemon shape. On the unlinked default socket the function concluded
// "unclaimed" because the signalled neighbour recreated the wrong socket and
// the skipped candidate was never disproved — licensing the destructive `af
// reset` path (worktree os.RemoveAll, branch delete) the file's own invariant
// ("a failed read is not an empty result") exists to prevent.
//
// The unsignallable candidate is a real process whose comm and argv[0] are both
// "tmux" — exactly a darwin tmux client or foreground `tmux -D` shape — so
// isTmuxArgv passes and tmuxServerIdentity's `case "tmux"` rejects it for not
// having the daemon(3) shape (SID != PID || PPID != 1). This is the precise
// rejection branch 373e07b4's tightening of IsTmuxServer exposed.
func TestProbeSessionStrictStaysUnknownWhenAnUnsignallableCandidateRemains(t *testing.T) {
	testguard.IsolateTmux(t)

	// A signallable neighbour on a named socket inside the private world.
	out, err := exec.Command("tmux", "-L", "af_mix_nbr", "new-session", "-d", "-s", "x", "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux -L new-session: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", "af_mix_nbr", "kill-server").Run() })
	out, err = exec.Command("tmux", "-L", "af_mix_nbr", "display-message", "-p", "#{pid}").CombinedOutput()
	require.NoError(t, err, "tmux display-message: %s", out)
	neighbour, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)

	// An unsignallable candidate: a copy of sleep renamed "tmux", so the
	// kernel task name and argv[0] basename are both "tmux" — exactly what
	// liveTmuxServerPIDs' comm prefilter admits and isTmuxArgv accepts —
	// but it is not a daemonised server (SID != PID, PPID != 1), so
	// IsTmuxServer rejects it at tmuxServerIdentity's `case "tmux"`.
	fakeTmux := sleeper.Named(t, filepath.Join(t.TempDir(), "tmux"))
	fakeCmd := exec.Command(fakeTmux, "60")
	require.NoError(t, fakeCmd.Start())
	t.Cleanup(func() {
		_ = fakeCmd.Process.Kill()
		_, _ = fakeCmd.Process.Wait()
	})
	// Wait for the process to become readable as "tmux" so the fixture
	// exercises the `case "tmux"` rejection (isTmuxArgv passes, daemon shape
	// fails) rather than the argv-unreadable early return.
	fakePid := fakeCmd.Process.Pid
	require.Eventually(t, func() bool {
		p, lookupErr := proctree.Lookup(fakePid)
		return lookupErr == nil && p.Comm == "tmux" && len(proctree.Argv(fakePid)) > 0
	}, 5*time.Second, 10*time.Millisecond,
		"the tmux-named fixture must become readable as `tmux`")

	// Pin both pids to isolate the probe's universe from the developer's
	// own tmux processes. liveTmuxServerPIDs is bypassed; tmuxSocketClaimed
	// is the unit under test.
	t.Cleanup(PinServerProbeForTest(neighbour, fakePid))

	// The default socket inside the private world never had a server, so it
	// stays absent after the neighbour recreates only its named socket.
	exists, known, err := probeSessionStrict(cmd.MakeExecutor(), "af_never_existed")
	require.False(t, known, "a prefilter-passing pid that IsTmuxServer rejected as not "+
		"a daemon shape was never disproved, so absence must NOT be proved — reset must "+
		"refuse rather than destroy a possible live owner")
	require.False(t, exists)
	require.Error(t, err, "the probe must refuse (surface the socket-absent refusal) when "+
		"an unprobed possible owner remains, not return a clean 'proved absent'")
}

// The unclassifiable answer must still surface WHAT tmux said. An
// (*exec.ExitError).Error() is only "exit status 1", and #3469 spent a
// round trip on a refusal that reported exactly that and nothing else.
func TestProbeSessionStrictNamesTheDiagnosticItCouldNotClassify(t *testing.T) {
	const name = "af_unclassifiable"
	failure := tmuxExitOneError(t, "error connecting to /run/tmux/x (Permission denied)")
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, failure },
	}

	exists, known, err := probeSessionStrict(cmdExec, name)
	require.False(t, exists)
	require.False(t, known, "a listing that did not answer either leaves the session unknown")
	require.ErrorContains(t, err, "Permission denied",
		"the refusal must name what tmux said, not only its exit status")
}

// TestProbeStrictRefusesWhenARenamedLiveServerOwnsTheUnlinkedSocket is the
// end-to-end regression for the liveTmuxServerPIDs prefilter (#2875/#7242).
//
// Linux tmux builds its kernel task name from getprogname(), which is argv[0],
// so a server started as `exec -a foo tmux` — or a copy/symlink named
// otherwise — is titled `<name>: server` (measured on tmux 3.4, documented in
// internal/proctree/tmuxidentity.go, #7242). The old exact-match prefilter
// (`comm == "tmux: server" || comm == "tmux"`) dropped that shape at the
// entry point, so the owner never became a candidate, the socket-absent probe
// answered a determinate "unclaimed" (NoServerRunning == true) while the
// server and its panes were alive, and `af reset`'s destructive cleanup
// proceeded against a still-running server — the exact destruction class the
// socket-absent probe exists to prevent (#2875).
//
// The fix admits the `: server` suffix and its truncations: the kernel keeps
// 15 bytes of task name, so the title is whole only for a basename of seven
// characters or fewer — at eight to fourteen a partial suffix survives, and
// at fifteen or more the comm is only the name's prefix. The truncated form
// is admitted when it equals the truncation of "<basename(argv[0])>: server"
// — the one conclusive test a prefilter can run, since a server's argv is
// its client's verbatim and no tail spelling can rule a server out. Once
// admitted, the renamed server fails proctree.IsTmuxServer (its argv[0]
// basename is not "tmux") and is remembered as an unprobed possible owner,
// so tmuxSocketClaimed answers "claimed" (unknown) and NoServerRunning
// returns false — reset refuses.
//
// Linux-only: tmux cannot retitle on darwin, so a server started under a
// renamed binary there is titled after the exec'd file name (no `: server`
// suffix), a separate shape this fix does not address.
func TestProbeStrictRefusesWhenARenamedLiveServerOwnsTheUnlinkedSocket(t *testing.T) {
	for _, serverName := range []string{
		"bx",               // the suffix survives whole
		"mediumtmux",       // truncation keeps "mediumtmux: ser"
		"verylongtmuxname", // truncation keeps only the name prefix
	} {
		t.Run(serverName, func(t *testing.T) {
			renamedServerBlocksSocketAbsentProbe(t, serverName)
		})
	}
}

// renamedServerBlocksSocketAbsentProbe stages a tmux server under a copy of
// the binary named serverName on a private -S socket, unlinks the socket, and
// requires the whole probe chain to refuse the determinate empty.
func renamedServerBlocksSocketAbsentProbe(t *testing.T, serverName string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		// The `<name>: server` task name is a Linux prctl measurement; tmux
		// cannot retitle on darwin, where this test's premise does not hold.
		t.Skip("renamed-server task name is a Linux prctl shape")
	}
	testguard.IsolateTmux(t)

	// A copy of the tmux binary named otherwise. `exec -a <name> tmux` would
	// title the server it forks the same way, but a copy is what a user
	// actually has on disk.
	tmuxPath, err := exec.LookPath("tmux")
	require.NoError(t, err)
	src, err := os.ReadFile(tmuxPath)
	require.NoError(t, err)
	renamed := filepath.Join(t.TempDir(), serverName)
	require.NoError(t, os.WriteFile(renamed, src, 0o755))

	// Start a server on a private -S socket with a long-lived pane. The server
	// daemonises and retitles to `<serverName>: server`, truncated at the
	// kernel's 15-byte comm ceiling.
	socket := testguard.SocketPath(t, "repro.sock")
	out, err := exec.Command(renamed, "-S", socket, "new-session", "-d",
		"-s", "af_renamed_owner", "sleep 300").CombinedOutput()
	require.NoError(t, err, "renamed tmux new-session: %s", out)
	// Kill through the socket while it still exists, registered before
	// anything that can fail below so a failing assertion cannot leak the
	// server or its sleep pane. The pid cleanup stays for after the socket
	// is unlinked, where -S can no longer reach it.
	t.Cleanup(func() { _ = exec.Command(renamed, "-S", socket, "kill-server").Run() })

	// The server pid, from a plain `tmux` client on the same socket: a client
	// reaches whichever server owns the socket, regardless of the client
	// binary's name.
	out, err = exec.Command("tmux", "-S", socket, "display-message",
		"-p", "#{pid}").CombinedOutput()
	require.NoError(t, err, "tmux display-message against the renamed server: %s", out)
	serverPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err, "renamed server pid must parse: %q", out)
	t.Cleanup(func() {
		// The socket is unlinked below; kill by pid, not `tmux -S kill-server`.
		_ = syscall.Kill(serverPID, syscall.SIGTERM)
	})

	// The outcome the fix owes, under the fail-safe property: the live
	// renamed server must be a live-server CANDIDATE under whatever comm the
	// kernel keeps of its retitle — the test does not pin the truncated
	// spelling, which differs across tmux builds and name lengths. Waiting
	// for candidacy rather than the comm itself also covers retitle timing:
	// the rename lands inside proc_start a moment after the daemonising
	// fork. liveTmuxServerPIDs reads the real process table by design — the
	// bug is in this function, so it is not pinned; membership, not
	// equality, so an unrelated same-uid server on a shared host cannot
	// change the result.
	require.Eventually(t, func() bool {
		return containsInt(liveTmuxServerPIDs(), serverPID)
	}, 15*time.Second, 20*time.Millisecond,
		"the renamed server (%s) must become a live-server candidate", serverName)

	// The socket-absent precondition the probe exists for (#2875): the server
	// keeps running, every client gets ENOENT.
	require.NoError(t, os.Remove(socket), "unlink the server's socket while it lives")

	_, err = exec.Command("tmux", "-S", socket, "ls").Output()
	require.Error(t, err, "tmux ls on the unlinked socket must fail")
	diag, ok := tmuxExitOneDiagnostic(err)
	require.True(t, ok, "tmux ls on the unlinked socket must exit 1 with a diagnostic")
	require.Equal(t, socketAbsent, classifyNoServerDiagnostic(diag),
		"the diagnostic must be the ENOENT/socket-absent one the probe disambiguates")

	// The whole point: a live renamed server forbids a determinate-empty
	// answer. NoServerRunning MUST be false so CleanupSessions (`af reset`)
	// refuses rather than proceeding to destroy worktrees, prune branches,
	// and erase session records out from under a live server.
	require.False(t, NoServerRunning(err),
		"a live tmux server whose socket was unlinked must NOT be reported as no-server "+
			"(a determinate empty would license reset's destructive cleanup against it)")
}

// tmuxExitOneError builds a genuine *exec.ExitError carrying diagnostic on
// stderr, which is the only place tmux's reason ever lives.
func tmuxExitOneError(t *testing.T, diagnostic string) error {
	t.Helper()
	return exitErrorWithStderr(t, 1, diagnostic)
}

func tmuxExitTwoError(t *testing.T, diagnostic string) error {
	t.Helper()
	return exitErrorWithStderr(t, 2, diagnostic)
}

func exitErrorWithStderr(t *testing.T, code int, diagnostic string) error {
	t.Helper()
	command := exec.Command("sh", "-c", `printf '%s\n' "$1" >&2; exit "$2"`,
		"sh", diagnostic, strconv.Itoa(code))
	_, err := command.Output()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, code, exitErr.ExitCode())
	require.Equal(t, diagnostic, strings.TrimSpace(string(exitErr.Stderr)),
		"the fixture must reproduce tmux's stderr exactly")
	return err
}
