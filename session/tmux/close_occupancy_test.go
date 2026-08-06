package tmux

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

// #2998 acceptance 1: a vanished session that never exported a marker still sees
// a descendant left inside its workspace. No marker, no tmux, no ancestry — the
// case every marker-based check reports empty for.
func TestMarkerlessWorktreeOccupants_ReportsAProcessLeftInTheWorkspace(t *testing.T) {
	worktree := t.TempDir()
	pid := occupyDir(t, worktree)

	ts := NewTmuxSessionFromSanitizedName("gone-session", "")
	ts.SetWorktreePath(worktree)

	err := ts.markerlessWorktreeOccupants()
	require.Error(t, err, "an occupied workspace must not be reported as safe to mutate")
	require.Contains(t, err.Error(), "still working inside")
	require.Contains(t, err.Error(), worktree)
	require.Contains(t, err.Error(), "reported rather than killed",
		"the message must say what was NOT done, so nobody assumes the process was dealt with")
	_ = pid
}

// #2998 acceptance 2: an ordinary exited agent stays collectable. An empty
// workspace must never refuse, or every vanished-session teardown stalls.
func TestMarkerlessWorktreeOccupants_AllowsAnEmptyWorkspace(t *testing.T) {
	ts := NewTmuxSessionFromSanitizedName("gone-session", "")
	ts.SetWorktreePath(t.TempDir())
	require.NoError(t, ts.markerlessWorktreeOccupants())
}

// A sibling sharing a path prefix is not inside the workspace. Without this the
// check would refuse teardowns that are perfectly safe.
func TestMarkerlessWorktreeOccupants_IgnoresASiblingSharingAPathPrefix(t *testing.T) {
	parent := t.TempDir()
	worktree := filepath.Join(parent, "wt")
	sibling := filepath.Join(parent, "wt-backup")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	occupyDir(t, sibling)

	ts := NewTmuxSessionFromSanitizedName("gone-session", "")
	ts.SetWorktreePath(worktree)
	require.NoError(t, ts.markerlessWorktreeOccupants())
}

// No workspace supplied means the evidence is unavailable, never that the
// workspace is empty. It must not refuse — that is the pre-#2998 state, which is
// what the ghost-kill path (no worktree in hand) still gets.
func TestMarkerlessWorktreeOccupants_UnsuppliedWorkspaceIsNotARefusal(t *testing.T) {
	ts := NewTmuxSessionFromSanitizedName("gone-session", "")
	require.NoError(t, ts.markerlessWorktreeOccupants())
}
