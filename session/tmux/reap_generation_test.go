package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
//
// The replacement is created while the sweep is HELD at its grace observation,
// not 50ms into a 200ms grace and hoping. This direction of the assertion never
// went red — a replacement born after the sweep finished still passes — and that
// is exactly the problem: it would pass for the wrong reason, having tested a
// sweep that had nothing to adopt (#3766). requireMarkedPaneProcess below turns
// "the sweep probably saw it" into a fact established before the sweep is
// released to look.
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

	barrier, sweepDone := startSweepAtGraceBarrier(t, name, func() error {
		return reapVanishedSessionProcesses(name, home, []proctree.Process{old}, nil, false)
	})
	barrier.holdFirstObservation(t, func() {
		created, createErr := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", t.TempDir(),
			"-e", EnvMarkerSession+"="+name,
			"-e", EnvMarkerHome+"="+home,
			"-e", EnvMarkerGeneration+"="+replacementGeneration,
			"sleep 300").CombinedOutput()
		require.NoError(t, createErr, "re-create same-named tmux session: %s", created)
		t.Cleanup(func() {
			_, _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
		})
		requireMarkedPaneProcess(t, name, replacementGeneration)
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

	err := reapVanishedSessionProcesses(name, home, nil, nil, false)
	require.True(t, proctree.AliveSame(replacement),
		"a blind vanished-session sweep reaped a same-named replacement")
	require.ErrorContains(t, err, "generation",
		"blind recovery must refuse when it cannot identify the predecessor generation")
}

// TestTrustedBlindVanishedSessionSweepReapsNewerGeneration pins the #3413 fix.
// Archive/kill hold this exact session's exclusive lifecycle lock for their
// entire call (daemon/archive.go's op-lock + killsInFlight), which structurally
// rules out the actual risk #3338/#3309 protect against — a same-name
// REPLACEMENT appearing mid-sweep. With that risk excluded, a trusted sweep can
// reap a survivor whose generation is simply NEWER than anything captured,
// restoring the non-destructive escape hatch for a session that flapped
// through a restore between capture and teardown.
func TestTrustedBlindVanishedSessionSweepReapsNewerGeneration(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_trusted_blind_newer_generation"
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	survivor := spawnMarkedSessionWithEscapee(t, name, home, "newer-generation")

	err := reapVanishedSessionProcesses(name, home, nil, nil, true)
	require.NoError(t, err, "a trusted sweep must reap its own session's survivor, not refuse it")
	require.False(t, proctree.AliveSame(survivor),
		"the newer-generation survivor must be reaped once the caller vouches for exclusivity")
}

// TestTrustedBlindVanishedSessionSweepStillRejectsForeignHome pins the boundary
// of the #3413 trust: it rules out a same-name REPLACEMENT, nothing more. A
// process from a DIFFERENT agent-factory home is the #1122 case
// markedOrphanProcesses already silently skips regardless of generation, and
// trusting the live generation must not change that.
func TestTrustedBlindVanishedSessionSweepStillRejectsForeignHome(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_trusted_blind_foreign_home"
	ourHome := t.TempDir()
	foreignHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", ourHome)
	foreign := spawnMarkedSessionWithEscapee(t, name, foreignHome, "some-generation")

	err := reapVanishedSessionProcesses(name, ourHome, nil, nil, true)
	require.NoError(t, err, "a foreign-home process is silently skipped, not an error")
	require.True(t, proctree.AliveSame(foreign),
		"trusting the live generation must not waive the AF_HOME ownership boundary")
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
	// The parent outlives the sweep (#3439). The changed-generation child is
	// reachable only as a ppid-descendant of a LIVE parent: it calls setsid, so it
	// shares no session id, and the moment the parent exits it reparents to init
	// and no refresh can attribute it again.
	//
	// What #3439 did NOT close is the other half — the child being BORN too late
	// for any pass to see it. The fixture used to write the trigger 50ms into a
	// 200ms grace, leaving `sh` ~150ms to wake from its poll and get through two
	// fork/execs, and that is a race against reapGraceWait, not a precondition:
	// on a loaded arm64 runner the child arrived after the last pass, nothing
	// mismatched, the sweep returned nil, and the assertion below read that as
	// "no refusal" — master red at b690a205 with session/tmux untouched (#3766).
	//
	// So the fork is now HELD against, not timed against. graceObservationBarrier
	// stops the sweep at the top of a pass, before it takes its snapshot; the
	// fixture forks the child, proves the process table shows it as a descendant
	// of the still-live parent, and only then releases the pass that has to see
	// it. No wall clock enters the verdict.
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

	barrier, sweepDone := startSweepAtGraceBarrier(t, name, func() error {
		return reapVanishedSessionProcesses(name, home, []proctree.Process{parent}, nil, false)
	})
	var child proctree.Process
	barrier.holdFirstObservation(t, func() {
		require.NoError(t, os.WriteFile(trigger, []byte("go"), 0o600))
		waitForPIDFile(t, pidFile)
		child = processFromPIDFile(t, pidFile)
		t.Cleanup(func() { _ = proctree.Signal(child, syscall.SIGKILL) })
		requireObservableDescendant(t, parent, child)
	})

	err = <-sweepDone
	require.ErrorContains(t, err, "generation",
		"a generation mismatch on captured ancestry must remain a refusal")
	require.True(t, proctree.AliveSame(child),
		"an ambiguously re-marked descendant must not be signalled")
}

// graceBarrier holds a vanished-session sweep at its bounded grace observation
// so a fixture can BUILD the state its assertion is about, instead of racing
// reapGraceWait to build it (#3766). See graceObservationBarrier in reap.go for
// why the hold sits before the pass takes its snapshot.
//
// The fixture work runs on the TEST goroutine rather than inside the hook:
// require's FailNow is only legal there, and a failure raised from the sweep's
// goroutine would surface as "test executed panic(nil) or runtime.Goexit"
// instead of the assertion that actually failed.
type graceBarrier struct {
	reached  chan struct{}
	release  chan struct{}
	returned chan struct{}
	arrived  sync.Once
	freed    sync.Once
}

// graceBarrierBound releases a barrier whose counterpart is never coming. It is
// reached only once the sweep or the fixture has already failed, so it costs
// nothing on a passing run and only keeps a failing one from hanging until the
// package deadline.
const graceBarrierBound = 10 * time.Second

// startSweepAtGraceBarrier installs the barrier for match and runs sweep on its
// own goroutine, returning the channel carrying its verdict.
//
// It owns that goroutine so it can also own the teardown. graceObservationBarrier
// is a package global: a cleanup that cleared it while the sweep was still
// reading it would be a data race, and -race would report it on precisely the
// runs where the test had already failed for another reason. Cleanup therefore
// releases the barrier, waits for the sweep to return, and only then clears the
// hook.
func startSweepAtGraceBarrier(t *testing.T, match string, sweep func() error) (*graceBarrier, <-chan error) {
	t.Helper()
	require.Nil(t, graceObservationBarrier, "a grace observation barrier is already installed")
	barrier := &graceBarrier{
		reached:  make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}
	graceObservationBarrier = func(observed string) {
		if observed != match {
			return
		}
		barrier.arrived.Do(func() { close(barrier.reached) })
		select {
		case <-barrier.release:
		case <-time.After(graceBarrierBound):
		}
	}
	verdict := make(chan error, 1)
	go func() {
		defer close(barrier.returned)
		verdict <- sweep()
	}()
	t.Cleanup(func() {
		barrier.releaseObservation()
		select {
		case <-barrier.returned:
		case <-time.After(graceBarrierBound):
		}
		graceObservationBarrier = nil
	})
	return barrier, verdict
}

// holdFirstObservation blocks until the sweep has reached an observation pass
// and has NOT yet taken that pass's snapshot, runs build, then releases the pass
// to observe what build left behind. The pass that follows always happens,
// however long build took — that is the guarantee documented at
// graceObservationBarrier.
func (b *graceBarrier) holdFirstObservation(t *testing.T, build func()) {
	t.Helper()
	select {
	case <-b.reached:
	case <-time.After(graceBarrierBound):
		require.FailNow(t, "the sweep never reached its bounded grace observation")
	}
	build()
	b.releaseObservation()
}

func (b *graceBarrier) releaseObservation() {
	b.freed.Do(func() { close(b.release) })
}

// requireObservableDescendant proves the precondition a generation refusal is
// about: the child is alive and is a ppid-descendant of a LIVE parent, so
// refreshCapturedAncestry's next pass reaches it through that parent's TreeOf.
// It is also the guard on the fixture itself — a setsid(1) that decided to fork
// would leave the recorded pid dead, and this says so instead of letting the
// sweep return an unexplained nil.
func requireObservableDescendant(t *testing.T, parent, child proctree.Process) {
	t.Helper()
	require.Eventually(t, func() bool {
		snap, err := proctree.Snapshot()
		if err != nil {
			return false
		}
		current, alive := snap[parent.PID]
		if !alive || current.StartID != parent.StartID {
			return false
		}
		for _, descendant := range proctree.TreeOf(snap, parent.PID) {
			if descendant.PID == child.PID && descendant.StartID == child.StartID {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond,
		"child %d never became a descendant of live parent %d", child.PID, parent.PID)
}

// requireMarkedPaneProcess proves a replacement session is visible to the marker
// scan the sweep is about to run: its pane process exists and its AF_SESSION and
// generation markers are readable. Without it, "the sweep did not reap the
// replacement" could hold because there was no replacement yet.
func requireMarkedPaneProcess(t *testing.T, name, generation string) {
	t.Helper()
	require.Eventually(t, func() bool {
		out, err := exec.Command("tmux", "list-panes", "-s", "-t", exactTarget(name), "-F", "#{pane_pid}").Output()
		if err != nil {
			return false
		}
		for _, field := range strings.Fields(string(out)) {
			pid, convErr := strconv.Atoi(field)
			if convErr != nil {
				continue
			}
			environ, envErr := proctree.Environ(pid)
			if envErr != nil {
				continue
			}
			session, hasSession := processEnvValue(environ, EnvMarkerSession)
			marker, hasGeneration := processEnvValue(environ, EnvMarkerGeneration)
			if hasSession && session == name && hasGeneration && marker == generation {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond,
		"no pane process of %s ever carried %s=%s", name, EnvMarkerGeneration, generation)
}
