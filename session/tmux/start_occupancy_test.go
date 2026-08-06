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

func occupyDirForStart(t *testing.T, dir string) {
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
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Skip("this platform does not disclose a child's working directory")
}

// Blindness alone must NOT make a workspace unsafe. A start timeout whose
// session never came up is blind by construction — there was never a pane to
// observe — so gating on it withheld ErrSessionNotStarted from every ordinary
// failed launch and left callers unable to tell "definitely did not start" from
// "could not determine". Only a positive occupant may do that.
func TestMarkerlessOccupants_EmptyWorkspaceIsSafe(t *testing.T) {
	require.NoError(t, markerlessOccupants(t.TempDir()),
		"an empty workspace must stay collectable: this is the ordinary failed launch")
}

// A markerless process actually inside the workspace is what makes it unsafe.
func TestMarkerlessOccupants_ReportsAPositiveOccupant(t *testing.T) {
	workDir := t.TempDir()
	occupyDirForStart(t, workDir)

	err := markerlessOccupants(workDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "still working inside")
	require.Contains(t, err.Error(), "reported rather than killed")
}

// A neighbour sharing a path prefix is not inside the workspace.
func TestMarkerlessOccupants_IgnoresASiblingSharingAPathPrefix(t *testing.T) {
	parent := t.TempDir()
	workDir := filepath.Join(parent, "wt")
	sibling := filepath.Join(parent, "wt-backup")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	occupyDirForStart(t, sibling)

	require.NoError(t, markerlessOccupants(workDir))
}

func TestMarkerlessOccupants_NoWorkspaceIsNotAGate(t *testing.T) {
	require.NoError(t, markerlessOccupants(""))
}
