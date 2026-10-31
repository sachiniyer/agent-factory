package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// tmuxSocketClaimed disambiguates an ENOENT tmux socket by signalling every
// live same-uid tmux server with SIGUSR1 — tmux's documented recreate-socket
// signal — and watching the path. A server that recreates it owns it; a path
// that stays absent is unclaimed, UNLESS a candidate we could not probe may
// still be the owner. The function's own contract, in server_presence.go,
// says so plainly: "a table with live servers that could not all be signaled
// answers 'claimed' (unknown), because a live owner was not disproved."
//
// Two branches of the signal loop could leave a candidate unprobed:
//
//   - the IsTmuxServer skip: a prefilter-passing pid that could not be
//     positively identified as a server. bb7949c4 (#4878) fixed this one by
//     setting unprobedOwnerPossible.
//   - the kill-ERROR branch: a pid that IS positively identified as a live
//     server but whose SIGUSR1 kill() FAILS — EPERM or EACCES from a
//     sandbox/LSM signal policy, the denied-signal state cleanup.go and
//     reap_test.go both call out as real ("a D-state sleeper or a process
//     this uid may not signal"). Before this fix the error was swallowed:
//     the pid was neither counted as signalled nor remembered as a possible
//     owner, so a MIXED table (one kill succeeding, one failing) with the
//     socket staying absent answered "unclaimed" — manufacturing a
//     determinate empty for a live owner that was never disproved and feeding
//     it to CleanupSessions / `af reset` on a path its own comments call "a
//     licence to destroy".
//
// The bug is invisible in an unconstrained kernel, where a same-uid kill()
// never fails EPERM, and a test "cannot conjure on demand" the unsignallable
// state (reap_test.go). These tests model it the same way proctree.Kill is a
// package var for exactly this reason: the SIGUSR1 delivery is injected via
// tmuxServerSignal (overridden by pinServerProbeSignalForTest) so a positively
// identified live tmux server can be made to look unsignallable, without a
// real seccomp filter or a process owned by another uid.
//
// Each candidate is a REAL tmux server on a private named socket (-L), so
// proctree.IsTmuxServer positively identifies it (the kernel task name
// "tmux: server" on Linux, the daemon(3) shape on darwin) and the signal loop
// reaches the kill() call — the branch the bug lives in, which a fixture
// that merely fails IsTmuxServer (the #4878 test) never enters.

// pinServerProbeSignalForTest overrides the SIGUSR1 delivery used by
// tmuxSocketClaimed for the duration of t, mirroring pinSocketClaimProof's
// self-registering, self-restoring shape. Test-only by contract: only
// tmuxSocketClaimed consumes tmuxServerSignal, so the override scopes the
// ownership probe and cannot change how a teardown signals anything.
func pinServerProbeSignalForTest(t *testing.T, sig func(int, syscall.Signal) error) {
	t.Helper()
	prev := tmuxServerSignal
	tmuxServerSignal = sig
	t.Cleanup(func() { tmuxServerSignal = prev })
}

// startNamedTmuxNeighbour launches a private tmux server on a -L named socket
// and returns its server pid. Keep the label SHORT: the socket path is
// TMUX_TMPDIR/tmux-<uid>/<label>, and TMUX_TMPDIR already runs ~57 chars on a
// macOS runner (/private/var/folders/…), so a label past ~17 chars pushes the
// path over the ~104-byte unix socket limit and new-session fails "File name
// too long" (#6335's macOS regression). The server is a real, signallable tmux server,
// so proctree.IsTmuxServer positively identifies it: this is what reaches the
// kill() branch of tmuxSocketClaimed. Isolated by -L (never the default
// socket, never tmux kill-server on the ambient server — the rule in
// CLAUDE.md/#2175); cleaned up with its own -L kill-server.
func startNamedTmuxNeighbour(t *testing.T, socketLabel string) int {
	t.Helper()
	out, err := exec.Command("tmux", "-L", socketLabel, "new-session", "-d", "-s", "x", "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux -L %s new-session: %s", socketLabel, out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socketLabel, "kill-server").Run() })
	out, err = exec.Command("tmux", "-L", socketLabel, "display-message", "-p", "#{pid}").CombinedOutput()
	require.NoError(t, err, "tmux -L %s display-message: %s", socketLabel, out)
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	// Wait until proctree positively identifies the server. On Linux tmux
	// retitles the task to "tmux: server" during startup; on darwin the
	// daemon(3) shape is present once setsid + reparent land. The kill-error
	// branch is only reached for pids IsTmuxServer accepts, so pinning before
	// that is a race this suite cannot tolerate.
	require.Eventually(t, func() bool { return proctree.IsTmuxServer(pid) },
		5*time.Second, 25*time.Millisecond,
		"the %q neighbour must become positively identifiable as a tmux server", socketLabel)
	return pid
}

func defaultSocketInPrivateTmuxWorld(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d", os.Getuid()), "default")
}

// pinClaimedNeighbours isolates the probe's universe to exactly two live,
// positively-identified tmux servers on named sockets, and asserts the
// default socket inside the private world is absent (no server owns it, so
// it never reappears to short-circuit the 500ms watch). n1 is the signallable
// server; n2 is the one whose kill() the test injects an error for.
func pinClaimedNeighbours(t *testing.T, labelA, labelB string) (n1, n2 int, socket string) {
	t.Helper()
	testguard.IsolateTmux(t)
	n1 = startNamedTmuxNeighbour(t, labelA)
	n2 = startNamedTmuxNeighbour(t, labelB)
	t.Cleanup(PinServerProbeForTest(n1, n2))
	socket = defaultSocketInPrivateTmuxWorld(t)
	require.Eventually(t, func() bool {
		_, err := os.Stat(socket)
		return err != nil && os.IsNotExist(err)
	}, 2*time.Second, 25*time.Millisecond,
		"the default socket must stay absent so the 500ms watch reaches its deadline")
	return n1, n2, socket
}

// TestTmuxSocketClaimedKillOutcomes pins the boolean tmuxSocketClaimed answers
// for every kill() outcome of a positively-identified server, against a mixed
// table where one server's signal succeeds and the other's is the case under
// test. The EPERM/EACCES rows are the regression: they MUST stay "claimed"
// (true) because the unsignallable live owner was never disproved. ESRCH is
// the exemption a future change must not lose: a pid that vanished before it
// could be signalled IS disproved, so it does NOT poison the table into
// "claimed". The nil row is the unchanged happy path: every server signalled
// and the socket still absent is unclaimed.
//
// On the unfixed tree the EPERM/EACCES rows return false (the bug) instead of
// true; the ESRCH and nil rows already pass there, which is why the defect
// lived only in the mixed-kill-error branch.
func TestTmuxSocketClaimedKillOutcomes(t *testing.T) {
	n1, n2, socket := pinClaimedNeighbours(t, "af_ck_a", "af_ck_b")

	for _, tc := range []struct {
		name        string
		n2Err       error
		wantClaimed bool
	}{
		{"EPERMFromALiveServerStaysClaimed", syscall.EPERM, true},
		{"EACCESFromALiveServerStaysClaimed", syscall.EACCES, true},
		{"ESRCHVanishedBeforeSignalIsDisprovedUnclaimed", syscall.ESRCH, false},
		{"BothSignalledAndSocketAbsentIsUnclaimed", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// n1 is always genuinely signalled (sets signaled=true, so the
			// deadline arm decides — the mixed-table path the bug lives in).
			// n2 gets the case's injected errno; a nil n2Err means signal n2
			// for real, the all-succeed happy path.
			n2Err := tc.n2Err
			pinServerProbeSignalForTest(t, func(pid int, sig syscall.Signal) error {
				if pid == n2 && n2Err != nil {
					return n2Err
				}
				return syscall.Kill(pid, sig)
			})
			require.Equal(t, tc.wantClaimed, tmuxSocketClaimed(socket, []int{n1, n2}),
				"kill() outcome classification for a positively-identified live server")
		})
	}
}

// TestNoServerRunningRefusesWhenASignallableServerKillFails is the direct
// reproduction of the reported bug at the classification boundary: the exact
// ENOENT diagnostic NoServerRunning reads, with one signallable server and
// one EPERM'd server, MUST report "no server is NOT determinate" (false). On
// the unfixed tree it returns true — the determinate empty that licenses
// CleanupSessions to delete worktrees and kill processes under a live,
// unsignallable owner.
func TestNoServerRunningRefusesWhenASignallableServerKillFails(t *testing.T) {
	n1, n2, socket := pinClaimedNeighbours(t, "af_ns_a", "af_ns_b")
	pinServerProbeSignalForTest(t, func(pid int, sig syscall.Signal) error {
		if pid == n2 {
			return syscall.EPERM
		}
		return syscall.Kill(pid, sig)
	})
	require.True(t, proctree.IsTmuxServer(n1) && proctree.IsTmuxServer(n2),
		"both pinned candidates must be positively identified as live servers")

	failure := tmuxExitOneError(t, "error connecting to "+socket+" (No such file or directory)")
	require.False(t, NoServerRunning(failure),
		"a live same-uid server we positively identified but could not signal (EPERM) was never "+
			"disproved; the missing socket is NOT a determinate empty, so NoServerRunning must refuse")
}

// TestProbeSessionStrictStaysUnknownWhenASignallableServerKillFails is the
// end-to-end gate CleanupSessions consults. With one signallable server and
// one EPERM'd server and the default socket absent, the existence probe must
// answer UNKNOWN and refuse — the mirror of the #4878 regression test, on
// the kill-error branch instead of the IsTmuxServer skip branch. On the
// unfixed tree `known` is wrongly `true` (the bug "proves absence") and
// `af reset` proceeds to delete worktrees and kill processes out from under
// a live, unsignallable tmux server.
func TestProbeSessionStrictStaysUnknownWhenASignallableServerKillFails(t *testing.T) {
	n1, n2, _ := pinClaimedNeighbours(t, "af_en_a", "af_en_b")
	pinServerProbeSignalForTest(t, func(pid int, sig syscall.Signal) error {
		if pid == n2 {
			return syscall.EPERM
		}
		return syscall.Kill(pid, sig)
	})
	require.True(t, proctree.IsTmuxServer(n1) && proctree.IsTmuxServer(n2),
		"both pinned candidates must be positively identified as live servers")

	exists, known, err := probeSessionStrict(cmd.MakeExecutor(), "af_never_existed")
	require.False(t, exists)
	require.False(t, known, "a live same-uid server we positively identified but could not signal "+
		"was never disproved; absence must NOT be proved — reset must refuse rather than destroy "+
		"a possible live owner")
	require.Error(t, err, "the probe must refuse (surface the socket-absent refusal) when an "+
		"unprobed possible owner remains, not return a clean 'proved absent'")
}

// TestTmuxSocketClaimedAllKillsFailStaysClaimed pins the !signaled arm of
// tmuxSocketClaimed for the kill-error case: when EVERY positively-identified
// server's kill() fails with a non-ESRCH errno, the function must NOT fall
// through to the deadline arm — it must answer "claimed" outright via the
// `return len(pids) != 0` path, because no candidate was disproved. This arm
// pre-dates the fix and already behaved safely, but it is the structurally
// obvious cousin of the mixed-table bug, so it is pinned here against a
// future change that "tidies" one branch and forgets the other.
func TestTmuxSocketClaimedAllKillsFailStaysClaimed(t *testing.T) {
	n1, n2, socket := pinClaimedNeighbours(t, "af_af_a", "af_af_b")
	pinServerProbeSignalForTest(t, func(int, syscall.Signal) error { return syscall.EPERM })

	require.True(t, tmuxSocketClaimed(socket, []int{n1, n2}),
		"a non-empty table where no positively-identified server could be signalled must answer "+
			"claimed (unknown); an empty answer would license destructive cleanup")
}
