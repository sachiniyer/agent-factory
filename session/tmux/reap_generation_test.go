package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
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

// With no captured predecessor identity, a post-absence marker scan cannot
// distinguish an escaped process from a replacement that reused the name. It
// must refuse cleanup without signalling either one.
func TestBlindVanishedSessionSweepDoesNotAdoptReplacementGeneration(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_blind_vanished_recreated"
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	replacement := spawnMarkedSessionWithEscapee(t, name, home, "replacement")

	err := reapVanishedSessionProcesses(name, home, nil, nil)
	require.True(t, proctree.AliveSame(replacement),
		"a blind vanished-session sweep reaped a same-named replacement")
	require.ErrorContains(t, err, "generation",
		"blind recovery must refuse when it cannot identify the predecessor generation")
}

// Captured ancestry remains authoritative even if a descendant execs with a
// different generation marker. Silently treating that process as a replacement
// would lose the last proof that it came from the vanished pane tree.
func TestVanishedSessionSweepRefusesDescendantThatChangesGeneration(t *testing.T) {
	shrinkReapWaits(t)
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("exact detached-child fixture requires setsid")
	}

	const name = "af_vanished_changed_generation"
	home := t.TempDir()
	dir := t.TempDir()
	trigger := filepath.Join(dir, "fork")
	pidFile := filepath.Join(dir, "child.pid")
	// The parent outlives the sweep instead of exiting 150ms after it forks, and
	// that is what makes this test deterministic (#3439). The changed-generation
	// child is reachable only as a ppid-descendant of a LIVE parent: it calls
	// setsid, so it shares no session id, and the moment the parent exits it
	// reparents to init and no refresh can attribute it again. The old fixture
	// gave that window 150ms against a 200ms grace, so the sweep's refresh landed
	// ~10ms before the parent died — on a loaded box the two reorder and the
	// descendant is simply never seen, which reads as "no refusal" and fails the
	// assertion below. Measured 3 failures in 15 runs at load 75.
	//
	// Nothing here depends on the parent exiting: it carries the cohort's own
	// generation marker, so the sweep classifies it as a marked escapee and
	// signals it, and t.Cleanup collects whatever is left. `exec` so the shell
	// BECOMES that process — a trailing `sleep 300` as a child would outlive the
	// Kill below.
	script := fmt.Sprintf("while [ ! -f %s ]; do sleep 0.01; done; "+
		"env %s=changed setsid sleep 300 >/dev/null 2>&1 & %s; exec sleep 300",
		trigger, EnvMarkerGeneration, recordPIDShell("$!", pidFile))
	parentCmd := exec.Command("sh", "-c", script)
	parentCmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		EnvMarkerSession + "=" + name,
		EnvMarkerHome + "=" + home,
		EnvMarkerGeneration + "=vanished",
	}
	parentCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, parentCmd.Start())
	t.Cleanup(func() {
		_ = parentCmd.Process.Kill()
		_, _ = parentCmd.Process.Wait()
	})

	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	parent, ok := snap[parentCmd.Process.Pid]
	require.True(t, ok, "parent %d not in process snapshot", parentCmd.Process.Pid)

	sweepDone := make(chan error, 1)
	go func() {
		sweepDone <- reapVanishedSessionProcesses(name, home, []proctree.Process{parent}, nil)
	}()
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, os.WriteFile(trigger, []byte("go"), 0o600))
	waitForPIDFile(t, pidFile)
	child := processFromPIDFile(t, pidFile)
	t.Cleanup(func() { _ = proctree.Signal(child, syscall.SIGKILL) })

	err = <-sweepDone
	require.ErrorContains(t, err, "generation",
		"a generation mismatch on captured ancestry must remain a refusal")
	require.True(t, proctree.AliveSame(child),
		"an ambiguously re-marked descendant must not be signalled")
}
