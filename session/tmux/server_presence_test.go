package tmux

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #4349: the PID set behind BOTH socket-absent arms — the `kill -USR1`
// recovery advice and tmuxSocketClaimed's direct signalling — must answer
// "which server owns this socket", not "which pids look like tmux". On darwin
// p_comm is "tmux" for the server AND every attached client (setproctitle
// rewrites argv, never p_comm), so a comm-only list swept clients in — and
// SIGUSR1, whose default disposition is terminate, killed a build's own
// `tmux show-options` client mid-flight (master Build 35043934992).

// tmuxArgvProc starts a sleeper whose argv[0] is retitle-shaped, the way
// setproctitle leaves a real tmux process's command line — "tmux: server" or
// "tmux: client". Its comm stays the binary's own name; for the darwin shape
// where comm itself is "tmux" see tmuxNamedProc.
func tmuxArgvProc(t *testing.T, argv0 string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.Args[0] = argv0
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// tmuxNamedProc stages the exact process shape #4349 lives in on darwin: a
// file literally named tmux, so the kernel reports comm "tmux" — the
// executable basename setproctitle never updates — while argv carries the
// retitle that separates a server from a client.
func tmuxNamedProc(t *testing.T, argv0 string) *exec.Cmd {
	t.Helper()
	sleeper, err := exec.LookPath("sleep")
	require.NoError(t, err)
	fakePath := filepath.Join(t.TempDir(), "tmux")
	binary, err := os.ReadFile(sleeper)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(fakePath, binary, 0o755))
	cmd := exec.Command(fakePath, "300")
	cmd.Args[0] = argv0
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// TestLiveTmuxServerPIDsExcludesClients pins the construction side: a process
// shaped like a tmux CLIENT — comm "tmux" with argv "tmux: client", which is
// what every attached client looks like on darwin — must not land in the set
// that gets signalled or rendered into `kill -USR1` advice, while the
// same-shaped server stays.
func TestLiveTmuxServerPIDsExcludesClients(t *testing.T) {
	client := tmuxNamedProc(t, "tmux: client")
	server := tmuxNamedProc(t, "tmux: server")

	pids := liveTmuxServerPIDs(filepath.Join(t.TempDir(), "no-listener-here"))

	if slices.Contains(pids, client.Process.Pid) {
		t.Errorf("a client-shaped pid made the server set — it would be signalled or named " +
			"in kill advice, and SIGUSR1's default disposition is terminate (#4349)")
	}
	if !slices.Contains(pids, server.Process.Pid) {
		t.Errorf("a server-shaped pid is missing from the server set it belongs in")
	}
}

// TestTmuxSocketClaimedNeverSignalsAClient pins the spray arm: handed a set
// that holds a client-shaped pid next to the socket's real server, only the
// server may be signalled. The client must still be alive when the probe
// returns — a signal that reached it is the CI kill this issue reports.
func TestTmuxSocketClaimedNeverSignalsAClient(t *testing.T) {
	testguard.IsolateTmux(t)

	out, err := exec.Command("tmux", "new-session", "-d", "-s", "af_claim_probe", "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	out, err = exec.Command("tmux", "display-message", "-p", "#{pid}").CombinedOutput()
	require.NoError(t, err, "tmux display-message: %s", out)
	serverPid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err, "the private server's pid must parse: %q", out)

	client := tmuxArgvProc(t, "tmux: client")

	socket := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	require.NoError(t, os.Remove(socket), "stage the unlinked socket")

	require.True(t, tmuxSocketClaimed(socket, []int{client.Process.Pid, serverPid}),
		"the live same-socket server should have been signalled and reclaimed its socket")

	if _, err := proctree.Lookup(client.Process.Pid); err != nil {
		t.Errorf("the client-shaped pid is gone (%v) — it was signalled alongside the server, "+
			"which is how SIGUSR1 reached a tmux client it did not own (#4349)", err)
	}
}

// TestLiveTmuxServerPIDsNamesTheSocketOwner pins the ownership half of the
// fix: where the kernel table can attest it, the set narrows to the server
// actually bound at THIS socket — the ownership answer — rather than every
// server that happens to run for this uid. Where ownership cannot be
// attested, the set is every verified server: still the conservative answer.
func TestLiveTmuxServerPIDsNamesTheSocketOwner(t *testing.T) {
	testguard.IsolateTmux(t)

	out, err := exec.Command("tmux", "new-session", "-d", "-s", "af_owner_probe", "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	out, err = exec.Command("tmux", "display-message", "-p", "#{pid}").CombinedOutput()
	require.NoError(t, err, "tmux display-message: %s", out)
	serverPid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err, "the private server's pid must parse: %q", out)

	// A second same-uid server-shaped process that does NOT own this socket.
	// Where ownership is attested it must drop out; elsewhere it may stay.
	pretender := tmuxNamedProc(t, "tmux: server")

	socket := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d", os.Getuid()), "default")

	pids := liveTmuxServerPIDs(socket)
	require.Contains(t, pids, serverPid,
		"the server bound at this socket must be named — that is the whole point of the set")
	if runtime.GOOS == "linux" {
		require.Equal(t, []int{serverPid}, pids,
			"the kernel table attests exactly one owner for this socket; the set must narrow to it")
		require.NotContains(t, pids, pretender.Process.Pid)
	}
}

// TestIsLiveTmuxServerRefusesClients pins the signal-time re-verify: the same
// argv evidence that excludes a client at construction must exclude it at the
// kill syscall, because proctree.IsTmuxServer's "tmux:" prefix match counts a
// client as a server.
func TestIsLiveTmuxServerRefusesClients(t *testing.T) {
	client := tmuxArgvProc(t, "tmux: client")
	server := tmuxArgvProc(t, "tmux: server")
	plain := tmuxArgvProc(t, "tmux")
	ordinary := exec.Command("sleep", "300")
	require.NoError(t, ordinary.Start())
	t.Cleanup(func() {
		_ = ordinary.Process.Kill()
		_, _ = ordinary.Process.Wait()
	})

	require.False(t, isLiveTmuxServer(client.Process.Pid),
		"a client must fail the signal gate — SIGUSR1 terminates it")
	require.True(t, isLiveTmuxServer(server.Process.Pid))
	require.True(t, isLiveTmuxServer(plain.Process.Pid),
		"an unretitled tmux stays a candidate: argv cannot disprove it is a server")
	require.False(t, isLiveTmuxServer(ordinary.Process.Pid))
	require.False(t, isLiveTmuxServer(-1), "a pid that does not exist is not a server")

	// The full set of title shapes, and the near misses. #4432: filepath.Base
	// was wrong here in BOTH directions, and the second one is the dangerous
	// one — it mangles a suffixed server OUT of the gate (a withheld signal),
	// and it lifts a client whose truncated title happens to end in "/tmux"
	// straight INTO it, which is a SIGUSR1 at a process whose default
	// disposition for it is terminate.
	cases := []struct {
		argv0 string
		want  bool
		why   string
	}{
		{"tmux: server", true, "retitled, cut back to the last space"},
		{"tmux: server (/private/tmp/tmux-501/default)", true, "the full retitle: role plus socket path"},
		{"tmux: server (/tmp/tmux-1000/def", true, "truncated mid-suffix is still positively a server"},
		{"tmux", true, "unretitled: argv[0] is the binary and cannot disprove server-hood"},
		{"/opt/homebrew/bin/tmux", true, "unretitled and pathed — Base is justified on a real path"},
		{"tmux: client (/private/tmp/tmux-501/default)", false, "a suffixed client must stay out of the signal set"},
		{"tmux: client (/private/tmp/tmux", false, "a client truncated so its tail reads /tmux must not pass as an unretitled tmux"},
		{"tmux: serverfoo", false, "the role has to end at the title or at \" (\""},
		{"tmux: ", false, "a retitle that names no role is not a CONFIRMED server"},
		{"vim", false, "not tmux at all"},
	}
	// Spawn every stand-in BEFORE asserting on any of them. argv only becomes
	// readable once the child has exec'd, and asserting immediately after
	// Start() reads an empty /proc/<pid>/cmdline as "no argv" on a loaded box.
	pids := make([]int, len(cases))
	for i, tc := range cases {
		pids[i] = tmuxArgvProc(t, tc.argv0).Process.Pid
	}
	for i, tc := range cases {
		require.NotEmpty(t, proctree.Argv(pids[i]),
			"stand-in for %q never became readable — the case below would pass vacuously", tc.argv0)
		require.Equal(t, tc.want, isLiveTmuxServer(pids[i]),
			"isLiveTmuxServer(%q): %s", tc.argv0, tc.why)
	}
}

// TestTmuxArgvNamesClient covers the positive-sighting rule the construction
// loop applies: only an argv that actually says "tmux: client" excludes — an
// unreadable or unretitled argv cannot disprove server-hood, so it stays.
func TestTmuxArgvNamesClient(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{"tmux: client"}, true},
		{[]string{"tmux: client", "attach-session"}, true},
		// The retitle tmux actually writes. proc.c has ONE setproctitle call
		// site — setproctitle("%s (%s)", name, socket_path) in proc_start() —
		// so the full shape carries the socket PATH in parentheses, and
		// filepath.Base ate exactly that: Base("tmux: client
		// (/private/tmp/tmux-501/default)") is "default)". A normally
		// retitled client therefore failed to match, stayed in the fallback
		// set, and was signalled — #4349 reintroduced through an incomplete
		// match (#4432).
		{[]string{"tmux: client (/private/tmp/tmux-501/default)"}, true},
		{[]string{"tmux: client (/dev/ttys001)"}, true},
		{[]string{"tmux: client (/tmp/tmux-1000/default)", "attach-session"}, true},
		// Truncated mid-suffix, the way tmux's own compat setproctitle leaves
		// a title that overflows its 16-byte buffer. Still positively a client.
		{[]string{"tmux: client (/private/tmp/tm"}, true},
		// Near misses. The role has to END — at the title, or at the " ("
		// that opens the socket path — so a longer word is a different
		// process, not a suffixed client.
		{[]string{"tmux: clientfoo"}, false},
		{[]string{"tmux: clientx"}, false},
		{[]string{"tmux: clients (/tmp/tmux-1000/default)"}, false},
		{[]string{"tmux: client(/dev/ttys001)"}, false}, // tmux always writes the space
		{[]string{"tmux: server"}, false},
		{[]string{"tmux: server (/private/tmp/tmux-501/default)"}, false},
		{[]string{"tmux"}, false},                   // unretitled — cannot disprove server
		{[]string{"/opt/homebrew/bin/tmux"}, false}, // unretitled, pathed — same
		{nil, false}, // unreadable is not a client sighting
		{[]string{"vim"}, false},
	} {
		if got := tmuxArgvNamesClient(tc.argv); got != tc.want {
			t.Errorf("tmuxArgvNamesClient(%v) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

// TestLiveTmuxServerPIDsReadsUnnameableOwnerAsClaimed pins the ambiguity
// contract the other way: a listener bound at the socket by something that is
// NOT a signallable server means the socket is owned — claimed — by nobody we
// can name. That is the unnamed sentinel, never an empty "no server": a
// confident unclaimed here lets af sweep under a live owner.
func TestLiveTmuxServerPIDsReadsUnnameableOwnerAsClaimed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("socket-ownership attestation reads /proc/net/unix")
	}
	sock := filepath.Join(testguard.SocketTempDir(t), "held.sock")
	listener, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	require.Equal(t, []int{0}, liveTmuxServerPIDs(sock),
		"a bound socket whose holder is not a server we can signal is owned but unnameable")
}
