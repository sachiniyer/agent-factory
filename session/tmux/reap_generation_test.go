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
	"github.com/sachiniyer/agent-factory/internal/shellquote"
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
	vanishSessionWithObservableEscapee(t, name, old)
	require.True(t, proctree.AliveSame(old), "original marked helper must outlive its tmux session")

	barrier, sweepDone := startSweepAtGraceBarrier(t, name, func() error {
		return reapVanishedSessionProcessCohort(name, home, []proctree.Process{old}, nil,
			fixtureCohort(vanishedGeneration), cohortMembership{}, false)
	})
	barrier.holdFirstObservation(t, func() {
		created, createErr := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", t.TempDir(),
			"-e", EnvMarkerSession+"="+name,
			"-e", EnvMarkerHome+"="+home,
			"-e", EnvMarkerGeneration+"="+replacementGeneration,
			"sleep 300").CombinedOutput()
		require.NoError(t, createErr, "re-create same-named tmux session: %s", created)
		t.Cleanup(func() {
			_, _ = exec.Command("tmux", "kill-session", "-t", ExactTarget(name)).CombinedOutput()
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
	sweepDescendantThatChangesGeneration(t, "")
}

// TestVanishedSessionSweepRefusesDescendantThatChangesGenerationLate is the same
// sweep with the child's re-mark held back a full second after its fork — five
// times reapGraceWait — instead of left to the scheduler. A fixture that
// releases the sweep before the child carries its new generation hands the pass
// a cohort member to reap, and this turns that from a once-a-day CI loss into a
// deterministic one (#3766).
func TestVanishedSessionSweepRefusesDescendantThatChangesGenerationLate(t *testing.T) {
	sweepDescendantThatChangesGeneration(t, slowEnvDir(t, time.Second))
}

// sweepDescendantThatChangesGeneration runs the changed-generation fixture.
// envDir, when set, is put first on the fixture's PATH (see slowEnvDir). It is
// deliberately not a t.Helper: a failure should name its own assertion line,
// which is what #4217's census keys a sighting on.
func sweepDescendantThatChangesGeneration(t *testing.T, envDir string) {
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
	// What #3439 did NOT close is the child being BORN too late for any pass to
	// see it: a trigger written 50ms into a 200ms grace raced reapGraceWait, and on
	// a loaded runner the child arrived after the last pass (#3766). So the fork is
	// HELD against, not timed against. graceObservationBarrier stops the sweep at
	// the top of a pass, before it takes its snapshot; the fixture forks the child
	// and only then releases the pass that has to see it.
	//
	// What that did NOT close is the child being born but not yet RE-MARKED.
	// `$!` names the fork, and until the fork has exec'd env and env has exec'd
	// setsid, the child's environment is still the parent's copy — carrying the
	// cohort's own generation. Released there, a pass that reads it sees a cohort
	// member, reaps it, and returns nil, which the assertion below reads as "no
	// refusal" with no defect behind it (#3766, reopened). requireReMarkedDescendant
	// is the release condition: the child carries the changed marker, as a live
	// descendant of the live parent, before the pass looks. No wall clock enters
	// the verdict.
	//
	// And what that did NOT close is the sweep never reaching the observation at
	// all. reapVanishedSessionProcesses derives the generation cohort by reading
	// each captured process's environment at sweep entry; a single transient
	// /proc read failure there empties the cohort, and the blind early return
	// exits before any pass exists — the fixture then burned the full bound on
	// `reached` while the sweep's own verdict was already delivered (#3766,
	// reopened). So the cohort is supplied, not re-derived: the fixture knows
	// what it launched, and the sweep is given it the same way
	// recoverVanishedSessionProcesses is given the pre-marker capture's
	// generations. holdFirstObservation also watches the verdict channel, so a
	// no-observation return that still happens fails on the sweep's answer.
	//
	// Nothing here depends on the parent exiting: it carries the cohort's own
	// generation marker, so the sweep classifies it as a marked escapee and
	// signals it, and t.Cleanup collects whatever is left. `exec` so the shell
	// BECOMES that process — a trailing `sleep 300` as a child would outlive the
	// Kill below.
	const (
		changedGeneration  = "changed"
		vanishedGeneration = "vanished"
	)
	script := fmt.Sprintf("%s; "+
		"env %s=%s setsid sleep 300 >/dev/null 2>&1 & %s; exec sleep 300",
		testguard.BoundedGateWait(trigger, 10*time.Millisecond, 5*time.Minute),
		EnvMarkerGeneration, changedGeneration, recordPIDShell("$!", pidFile))
	path := os.Getenv("PATH")
	if envDir != "" {
		path = envDir + string(os.PathListSeparator) + path
	}
	parentCmd := exec.Command("sh", "-c", script)
	parentCmd.Env = []string{
		"PATH=" + path,
		EnvMarkerSession + "=" + name,
		EnvMarkerHome + "=" + home,
		EnvMarkerGeneration + "=" + vanishedGeneration,
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
		return reapVanishedSessionProcessCohort(name, home, []proctree.Process{parent}, nil,
			fixtureCohort(vanishedGeneration), cohortMembership{}, false)
	})
	var child proctree.Process
	barrier.holdFirstObservation(t, func() {
		require.NoError(t, os.WriteFile(trigger, []byte("go"), 0o600))
		waitForPIDFile(t, pidFile)
		child = processFromPIDFile(t, pidFile)
		t.Cleanup(func() { _ = proctree.Signal(child, syscall.SIGKILL) })
		requireReMarkedDescendant(t, parent, child, changedGeneration)
	})

	err = <-sweepDone
	require.ErrorContains(t, err, "generation",
		"a generation mismatch on captured ancestry must remain a refusal")
	require.True(t, proctree.AliveSame(child),
		"an ambiguously re-marked descendant must not be signalled")
}

// slowEnvDir returns a directory holding an `env` that waits delay before
// exec'ing the real one. First on the fixture's PATH, it is exec'd exactly where
// env would be, still carrying the generation the fork inherited, so the child
// re-marks itself delay after its fork rather than whenever the scheduler gets
// to it — the window a loaded runner opened by accident, held open on purpose.
func slowEnvDir(t *testing.T, delay time.Duration) string {
	t.Helper()
	realEnv, err := exec.LookPath("env")
	require.NoError(t, err)
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nsleep %s\nexec %s \"$@\"\n",
		strconv.FormatFloat(delay.Seconds(), 'f', -1, 64), shellquote.Quote(realEnv))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "env"), []byte(script), 0o755))
	return dir
}

// fixtureCohort builds the generation cohort a fixture sweep gets. Supplying
// it keeps "the cohort exists" out of the set of things the test can lose on:
// reapVanishedSessionProcesses re-derives it by reading each captured process's
// environment at sweep entry, and one transient /proc read failure there
// empties the cohort — the blind early return then exits before any observation
// pass exists, which is what left holdFirstObservation parked on `reached` for
// a mute 10s timeout on master (#3766). The fixture already knows what it
// launched, so it says so — the same shape recoverVanishedSessionProcesses
// uses, where the cohort comes from the pre-marker capture rather than a fresh
// read. An orphanGenerationSet with no legacy and the named generations.
func fixtureCohort(generations ...string) orphanGenerationSet {
	cohort := orphanGenerationSet{values: make(map[string]bool, len(generations))}
	for _, generation := range generations {
		cohort.values[generation] = true
	}
	return cohort
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
	verdict  <-chan error
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
	verdict := make(chan error, 1)
	barrier := &graceBarrier{
		reached:  make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
		verdict:  verdict,
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
//
// A sweep that RETURNS before reaching an observation — today the only such
// path is the empty-cohort blind return of reapVanishedSessionProcessCohort —
// has already delivered its verdict, and that verdict is the report. Waiting
// the bound on `reached` alone cannot tell "already returned" from "still
// coming": it produced a mute 10s timeout on master while the sweep's own
// answer — a blind refusal naming the fixture's parent — sat on the verdict
// channel (#3766). A sweep still working when the bound trips is a different
// finding, and the timeout message stays for exactly that case.
func (b *graceBarrier) holdFirstObservation(t *testing.T, build func()) {
	t.Helper()
	select {
	case <-b.reached:
	case err := <-b.verdict:
		require.FailNowf(t, "the sweep returned before reaching its bounded grace observation",
			"sweep verdict: %v", err)
	case <-time.After(graceBarrierBound):
		require.FailNow(t, "the sweep never reached its bounded grace observation")
	}
	build()
	b.releaseObservation()
}

func (b *graceBarrier) releaseObservation() {
	b.freed.Do(func() { close(b.release) })
}

// requireReMarkedDescendant proves the precondition a generation refusal is
// about, all of it in one observation: the child is still the process recorded,
// it is a ppid-descendant of a LIVE parent — so refreshCapturedAncestry's next
// pass reaches it through that parent's TreeOf — and its OWN environment
// carries generation.
//
// Descent alone is not that precondition: it holds the instant fork returns,
// while the child's environment is still the parent's (#3766). Comm reaching
// sleep is what says the exec chain is over, so nothing can change the marker
// again before the pass reads it. It is also the guard on the fixture itself —
// a setsid(1) that decided to fork would leave the recorded pid exiting rather
// than becoming sleep, and this says so instead of letting the sweep return an
// unexplained nil.
func requireReMarkedDescendant(t *testing.T, parent, child proctree.Process, generation string) {
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
			if descendant.PID != child.PID || descendant.StartID != child.StartID {
				continue
			}
			if descendant.Comm != "sleep" {
				return false
			}
			environ, envErr := proctree.Environ(child.PID)
			if envErr != nil {
				return false
			}
			marker, ok := processEnvValue(environ, EnvMarkerGeneration)
			return ok && marker == generation
		}
		return false
	}, 5*time.Second, 20*time.Millisecond,
		"child %d never became a live descendant of parent %d carrying %s=%s",
		child.PID, parent.PID, EnvMarkerGeneration, generation)
}

// requireMarkedPaneProcess proves a replacement session is visible to the marker
// scan the sweep is about to run: its pane process exists and its AF_SESSION and
// generation markers are readable. Without it, "the sweep did not reap the
// replacement" could hold because there was no replacement yet.
func requireMarkedPaneProcess(t *testing.T, name, generation string) {
	t.Helper()
	require.Eventually(t, func() bool {
		out, err := exec.Command("tmux", "list-panes", "-s", "-t", ExactTarget(name), "-F", "#{pane_pid}").Output()
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
