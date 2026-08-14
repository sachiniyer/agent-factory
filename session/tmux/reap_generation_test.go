package tmux

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// A vanished generation's grace window must not adopt a same-named replacement.
// AF_SESSION proves only the reusable tmux name; the replacement is a different
// generation even though it belongs to the same Agent Factory home.
func TestVanishedSessionSweepDoesNotReapSameNameReplacement(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const (
		name                  = "af_vanished_recreated"
		vanishedGeneration    = "vanished"
		replacementGeneration = "replacement"
	)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	old := spawnMarkedSessionWithEscapee(t, name, home, vanishedGeneration)
	out, err := exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
	require.NoError(t, err, "vanish original tmux session: %s", out)
	require.True(t, proctree.AliveSame(old), "original marked helper must outlive its tmux session")

	sweepStarted := make(chan struct{})
	sweepDone := make(chan error, 1)
	go func() {
		close(sweepStarted)
		sweepDone <- reapVanishedSessionProcesses(name, home, []proctree.Process{old}, nil)
	}()
	<-sweepStarted
	// Keep the replacement comfortably inside the old generation's 200ms grace
	// observation window while allowing the sweep goroutine to enter it.
	time.Sleep(50 * time.Millisecond)

	out, err = exec.Command("tmux", "new-session", "-d", "-s", name, "-c", t.TempDir(),
		"-e", EnvMarkerSession+"="+name,
		"-e", EnvMarkerHome+"="+home,
		"-e", EnvMarkerGeneration+"="+replacementGeneration,
		"sleep 300").CombinedOutput()
	require.NoError(t, err, "re-create same-named tmux session: %s", out)
	t.Cleanup(func() {
		_, _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
	})

	require.NoError(t, <-sweepDone)
	require.False(t, proctree.AliveSame(old),
		"the vanished generation's owned survivor must still be reaped")
	exists, known, probeErr := probeSessionStrict(cmd.MakeExecutor(), name)
	require.NoError(t, probeErr)
	require.True(t, known && exists,
		"the vanished generation's sweep reaped the fresh same-named session")
}
