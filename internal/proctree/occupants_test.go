package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// parkedProcessIn starts a real process whose cwd is dir and returns its pid.
// A real process, not a fake: the whole mechanism under test is what the kernel
// reports for /proc/<pid>/cwd, and a stub would test the stub.
func parkedProcessIn(t *testing.T, dir string) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.Dir = dir
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	// The cwd link is not necessarily readable the instant Start returns.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := WorkingDir(cmd.Process.Pid); ok {
			return cmd.Process.Pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Skipf("this platform does not disclose a child's working directory, so cwd occupancy cannot be observed here")
	return 0
}

func pidsOf(occupants []Occupant) map[int]bool {
	out := make(map[int]bool, len(occupants))
	for _, o := range occupants {
		out[o.Process.PID] = true
	}
	return out
}

// THE POINT OF #2998. A process inside the worktree is found with no marker
// anywhere in the picture — no AF_SESSION, no tmux, no ancestry. That is the
// case every marker-based check goes blind on: a session from a pre-marker build
// or under tmux < 3.2 never had a marker to find, so a marker scan reports the
// same empty result whether a descendant escaped or not.
func TestOccupantsOfDir_FindsAMarkerlessProcessByItsWorkingDirectory(t *testing.T) {
	worktree := t.TempDir()
	pid := parkedProcessIn(t, worktree)

	occupants, err := OccupantsOfDir(worktree)
	require.NoError(t, err)
	require.True(t, pidsOf(occupants)[pid],
		"a process working inside the worktree must be found without any marker: this is the signal that still works where the AF_SESSION chain is blind")

	for _, o := range occupants {
		if o.Process.PID == pid {
			require.NotEmpty(t, o.WorkingDir, "the report must say WHY the process was attributed")
		}
	}
}

// A subdirectory counts. An escaped agent child is far more likely to be sitting
// in .claude/ or a build directory than at the worktree root.
func TestOccupantsOfDir_FindsAProcessInASubdirectory(t *testing.T) {
	worktree := t.TempDir()
	nested := filepath.Join(worktree, "deep", "nested")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	pid := parkedProcessIn(t, nested)

	occupants, err := OccupantsOfDir(worktree)
	require.NoError(t, err)
	require.True(t, pidsOf(occupants)[pid], "a process below the worktree root is inside it")
}

// The negative that keeps this from being a rubber stamp. A process outside the
// worktree must NOT be attributed to it — otherwise the gate would refuse every
// teardown on any busy box and be turned off within a day.
func TestOccupantsOfDir_IgnoresAProcessOutsideTheWorktree(t *testing.T) {
	worktree := t.TempDir()
	elsewhere := t.TempDir()
	outside := parkedProcessIn(t, elsewhere)

	occupants, err := OccupantsOfDir(worktree)
	require.NoError(t, err)
	require.False(t, pidsOf(occupants)[outside],
		"attributing a process that is not in the worktree would make the gate refuse teardowns that are perfectly safe")
	require.Empty(t, occupants, "an empty worktree has no occupants")
}

// A sibling directory whose name merely PREFIXES the worktree's is not inside
// it: /tmp/wt and /tmp/wt-backup share a string prefix and nothing else. This is
// the classic path-containment bug, and here it would attribute an unrelated
// process and block a teardown.
func TestOccupantsOfDir_DoesNotMatchASiblingSharingAPathPrefix(t *testing.T) {
	parent := t.TempDir()
	worktree := filepath.Join(parent, "wt")
	sibling := filepath.Join(parent, "wt-backup")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	outside := parkedProcessIn(t, sibling)

	occupants, err := OccupantsOfDir(worktree)
	require.NoError(t, err)
	require.False(t, pidsOf(occupants)[outside],
		"%q merely starts with %q; string prefixes are not path containment", sibling, worktree)
}

// An empty path is an error, not an empty result. Silently returning "no
// occupants" for a caller that failed to supply a worktree would report a clean
// bill of health for a directory nobody looked at.
func TestOccupantsOfDir_RefusesAnEmptyPath(t *testing.T) {
	occupants, err := OccupantsOfDir("")
	require.Error(t, err)
	require.Nil(t, occupants)
}

// A path that does not exist still resolves and simply has no occupants — the
// ordinary case for a worktree that was already removed. It must not error, or
// every post-removal call would look like a failure.
func TestOccupantsOfDir_MissingWorktreeHasNoOccupants(t *testing.T) {
	occupants, err := OccupantsOfDir(filepath.Join(t.TempDir(), "never-created"))
	require.NoError(t, err)
	require.Empty(t, occupants)
}

func TestDescribeOccupants_NamesPidsAndCaps(t *testing.T) {
	var many []Occupant
	for i := 1; i <= 8; i++ {
		o := Occupant{WorkingDir: "/w"}
		o.Process.PID = i
		many = append(many, o)
	}
	out := DescribeOccupants(many)
	require.Contains(t, out, "pid 1 (cwd /w)")
	require.Contains(t, out, "and 3 more", "a long list must be capped rather than flooding an operator message")
	require.False(t, strings.Contains(out, "pid 8"), "capped entries must not be rendered")
}

// A deep tree that all inherited one cwd must be walked ONCE, not once per
// member. TreeOf rebuilds its child map from the full snapshot on every call, so
// re-walking is O(k*n) on a synchronous teardown gate — the daemon's kill/archive
// RPC blocks on this.
//
// The observable contract is that each pid appears exactly once; the skip that
// makes it cheap is what also makes that true without deduping after the fact.
func TestOccupantsOfDir_ReportsEachProcessOnce(t *testing.T) {
	worktree := t.TempDir()
	parkedProcessIn(t, worktree)
	parkedProcessIn(t, worktree)

	occupants, err := OccupantsOfDir(worktree)
	require.NoError(t, err)
	require.NotEmpty(t, occupants)

	seen := map[int]bool{}
	for _, o := range occupants {
		require.False(t, seen[o.Process.PID], "pid %d reported twice", o.Process.PID)
		seen[o.Process.PID] = true
	}
}

// The tmux SERVER must never be reported. One server backs every session on the
// box and outlives all of them, inheriting its cwd from whichever client first
// started it — so matching it refuses that workspace on every retry until the
// whole server exits, taking unrelated sessions with it.
//
// Driven through the real predicate rather than a stub: identification is from
// the process's own argv, so a stub would test the stub.
func TestIsTmuxServer_IdentifiesPositivelyFromArgv(t *testing.T) {
	require.False(t, isTmuxServer(os.Getpid()),
		"the test binary is not a tmux server and must not be excluded")

	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	require.False(t, isTmuxServer(cmd.Process.Pid),
		"an ordinary process must stay a candidate: excluding on anything but a positive match would blind the gate")

	// An unreadable/absent pid reports false rather than excluding silently.
	require.False(t, isTmuxServer(-1))
}
