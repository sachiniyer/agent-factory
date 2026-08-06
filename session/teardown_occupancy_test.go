package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

func occupyDir(t *testing.T, dir string) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.Dir = dir
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := proctree.WorkingDir(cmd.Process.Pid); ok {
			return cmd.Process.Pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Skip("this platform does not disclose a child's working directory")
	return 0
}

// #2998's acceptance criterion 1: a markerless vanished session with a
// descendant still inside the worktree must not have that worktree deleted.
//
// Every gate above this one reasons through the pane and the AF_SESSION marker.
// This process has neither — no marker, no tmux, no ancestry — which is exactly
// what a session from a pre-marker build or tmux < 3.2 leaves behind. The gate
// must still see it.
func TestWorktreeOccupancyGate_RefusesWhenSomethingIsStillInsideTheWorktree(t *testing.T) {
	worktree := t.TempDir()
	pid := occupyDir(t, worktree)

	state, err := worktreeOccupancyGate(worktree, "kill", "session-title", "agent")

	require.Equal(t, stateUnknown, state,
		"an occupied worktree must not be reported as safe to mutate: the caller deletes on stateKnown")
	require.Error(t, err)
	require.Contains(t, err.Error(), "still working inside")
	require.Contains(t, err.Error(), worktree, "the operator needs to know WHICH workspace")
	require.Contains(t, err.Error(), "reported rather than killed",
		"the message must say what was NOT done, so nobody assumes the process was dealt with")
	require.Contains(t, err.Error(), "retry", "the record stays retryable; the message should say so")
	_ = pid
}

// #2998's acceptance criterion 2: an ordinary exited agent is still collectable.
// If an empty worktree ever refused, every kill and archive on the box would
// stall — a constant breakage traded for a rare one, which the issue rejects by
// name.
func TestWorktreeOccupancyGate_AllowsAnEmptyWorktree(t *testing.T) {
	state, err := worktreeOccupancyGate(t.TempDir(), "archive", "session-title", "agent")
	require.Equal(t, stateKnown, state)
	require.NoError(t, err)
}

// A process in a SIBLING directory must not block this worktree's teardown.
// Path containment, not string prefixes: /x/wt-backup is not inside /x/wt.
func TestWorktreeOccupancyGate_IgnoresASiblingSharingAPathPrefix(t *testing.T) {
	parent := t.TempDir()
	worktree := filepath.Join(parent, "wt")
	sibling := filepath.Join(parent, "wt-backup")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	occupyDir(t, sibling)

	state, err := worktreeOccupancyGate(worktree, "kill", "session-title", "agent")
	require.Equal(t, stateKnown, state, "a neighbour's process must not block this teardown")
	require.NoError(t, err)
}

// A mode with no worktree in play is unaffected. release-PTY passes "" here, and
// tabs without a workspace must not be gated on one.
func TestWorktreeOccupancyGate_NoWorktreeIsNotAGate(t *testing.T) {
	state, err := worktreeOccupancyGate("", "kill", "session-title", "agent")
	require.Equal(t, stateKnown, state)
	require.NoError(t, err)
}
