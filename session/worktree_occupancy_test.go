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

// #2998 acceptance 1: a markerless process left in the workspace is seen. No
// AF_SESSION, no tmux, no ancestry — the case every marker-based check reports
// empty for, whether or not anything survived.
func TestCheckWorktreeOccupants_ReportsAMarkerlessProcess(t *testing.T) {
	worktree := t.TempDir()
	occupyDir(t, worktree)

	err := CheckWorktreeOccupants(worktree)
	require.Error(t, err)
	require.Contains(t, err.Error(), "still working inside")
	require.Contains(t, err.Error(), worktree)
	require.Contains(t, err.Error(), "reported rather than killed",
		"the message must say what was NOT done, so nobody assumes the process was dealt with")
}

// #2998 acceptance 2: an empty workspace is collectable. If this ever refused,
// every blind teardown would stall.
func TestCheckWorktreeOccupants_AllowsAnEmptyWorkspace(t *testing.T) {
	require.NoError(t, CheckWorktreeOccupants(t.TempDir()))
}

// A sibling sharing a path prefix is not inside the workspace.
func TestCheckWorktreeOccupants_IgnoresASiblingSharingAPathPrefix(t *testing.T) {
	parent := t.TempDir()
	worktree := filepath.Join(parent, "wt")
	sibling := filepath.Join(parent, "wt-backup")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	occupyDir(t, sibling)

	require.NoError(t, CheckWorktreeOccupants(worktree),
		"a neighbour's process must not block this teardown")
}

// No workspace in play is the one silent nil.
func TestCheckWorktreeOccupants_NoWorkspaceIsNotAGate(t *testing.T) {
	require.NoError(t, CheckWorktreeOccupants(""))
}
