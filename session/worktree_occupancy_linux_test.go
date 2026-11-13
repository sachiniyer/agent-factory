//go:build linux

package session

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// A refusal that cites an occupant the gate could not classify tells the
// operator what to do (#7242 review R8). The occupant is a live process whose
// command line reads as nothing to classify: it is exec'd with an EMPTY argv,
// the reading a process gives while it is starting or exiting.
func TestCheckWorktreeOccupants_AdvisesOnAnUnclassifiableOccupant(t *testing.T) {
	worktree := t.TempDir()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	require.NoError(t, err)
	occupant, err := os.StartProcess(sleeper.Path(t), []string{}, &os.ProcAttr{
		Dir:   worktree,
		Files: []*os.File{devNull, devNull, devNull},
	})
	_ = devNull.Close()
	require.NoError(t, err)
	t.Cleanup(func() {
		// The process this test started and recorded, and nothing else.
		_ = occupant.Kill()
		_, _ = occupant.Wait()
	})
	require.Eventually(t, func() bool {
		p, lookupErr := proctree.Lookup(occupant.Pid)
		_, cwdOK := proctree.WorkingDir(occupant.Pid)
		return lookupErr == nil && p.Comm == "sleeper" && cwdOK
	}, 5*time.Second, 10*time.Millisecond, "fixture: the argv-less sleeper never finished exec")

	err = CheckWorktreeOccupants(worktree)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unclassified")
	require.Contains(t, err.Error(), "retry",
		"a refusal over a process that is usually just starting or exiting must say that retrying is the fix")
	require.Contains(t, err.Error(), "ps -o pid,stat,lstart,args -p",
		"and how to inspect one that persists")
}
