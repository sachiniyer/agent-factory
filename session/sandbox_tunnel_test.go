package session

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/watchlifeline"
)

// The sleeper command these tests substitute for ssh: `sleep N #` turns the
// tunnel script's flags into a trailing shell comment, so the spawned group is
// sh→sleep with the same shape the real tunnel has without needing an sshd.
const tunnelTestSleep = "sleep 60 #"

// forceTunnelLifelineExe points the lifeline-exe seam at a binary for one test
// and restores both the seam and the unavailable latch on cleanup.
func forceTunnelLifelineExe(t *testing.T, exe string) {
	t.Helper()
	old := sandboxTunnelLifelineExe
	sandboxTunnelLifelineExe = func() string { return exe }
	sandboxTunnelLifelineUnavailable.Store(false)
	t.Cleanup(func() {
		sandboxTunnelLifelineExe = old
		sandboxTunnelLifelineUnavailable.Store(false)
	})
}

// spawnTestTunnel spawns a tunnel-shaped process group for p exactly as
// startTunnel does — register, then spawn+publish through
// provisionTunnelChild — and registers the failure-free cleanup. The caller
// drives whatever assertion it is testing, including none.
func spawnTestTunnel(t *testing.T, p *sandboxProvisioner) *tunnelChild {
	t.Helper()
	registerSandboxTunnel(p)
	if p.forward == "" {
		p.forward = "127.0.0.1:40000:127.0.0.1:40001"
	}
	child, err := p.provisionTunnelChild()
	require.NoError(t, err)
	require.NotNil(t, child)
	t.Cleanup(func() {
		p.reapMu.Lock()
		p.stopTunnel()
		p.reapMu.Unlock()
	})
	return child
}

// requireGroupGone polls until no member of the process group named by pgid
// answers kill(2) — the lifeline exits its group on death even before it is
// reaped, so an empty group is the whole-group-died fact.
func requireGroupGone(t *testing.T, pgid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process group %d still has members after %s", pgid, timeout)
}

func alive(t *testing.T, pid int) bool {
	t.Helper()
	return syscall.Kill(pid, 0) == nil
}

// TestSandboxTunnelLifelineKillsGroupWhenDaemonPipeCloses is the #5564
// regression: the tunnel's process group must die when the daemon's write end
// of the lifeline pipe closes — the kernel's version of "the daemon died by
// any cause, SIGKILL included". A sleep stands in for ssh; the group is
// lifeline+sh+sleep.
func TestSandboxTunnelLifelineKillsGroupWhenDaemonPipeCloses(t *testing.T) {
	// os.Args[0] is the test binary; TestMain's watchlifeline.HandleExec makes
	// it a real lifeline, so the lifeline path is exercised end to end.
	forceTunnelLifelineExe(t, os.Args[0])
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child := spawnTestTunnel(t, p)
	require.NotNil(t, child.ll, "the tunnel must run under the lifeline when the binary speaks it")

	// The inner tunnel command reaches the lifeline verbatim: sh -c '<sshCmd>
	// -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -N -L "$1"'
	// af-sandbox <forward> — argv[6:] after '<exe> __watch-lifeline v1 <ms> --'.
	// ExitOnForwardFailure is the failed-bind pin of
	// TestSandboxTunnelRefusesToOutliveAFailedForward; the ServerAlive pair is
	// the #5571 keepalive that bounds how long a dead peer goes undetected.
	require.Len(t, child.cmd.Args, 10, "lifeline argv: exe __watch-lifeline v1 <ms> -- sh -c <script> af-sandbox <forward>: %v", child.cmd.Args)
	assert.Equal(t, watchlifeline.Arg, child.cmd.Args[1])
	assert.Equal(t, watchlifeline.Protocol, child.cmd.Args[2])
	assert.Equal(t, "sh", child.cmd.Args[5])
	assert.Equal(t, "-c", child.cmd.Args[6])
	assert.Equal(t, tunnelTestSleep+` -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -N -L "$1"`, child.cmd.Args[7])
	assert.Equal(t, "af-sandbox", child.cmd.Args[8])
	assert.Equal(t, "127.0.0.1:40000:127.0.0.1:40001", child.cmd.Args[9])

	pgid := child.cmd.Process.Pid
	// The daemon's half of the death pact is closing this write end — exactly
	// what the kernel does for a dead daemon's descriptors.
	require.NoError(t, child.ll.keepW.Close())

	// The lifeline ends by its own group SIGKILL — the reaper's Wait both proves
	// that and reaps it, and only a reaped leader stops counting for
	// kill(-pgid, 0): a zombie still answers a group probe on this kernel.
	<-child.done
	require.Error(t, child.err, "the lifeline ends by its own group SIGKILL")
	requireGroupGone(t, pgid, 10*time.Second)
}

// TestSandboxTunnelFallsBackOncePerDaemon covers the non-acking lifeline: the
// tunnel still spawns — as the pre-#5564 direct sh -c child — the degradation
// latches, and the warning is logged once per daemon, not once per provision.
func TestSandboxTunnelFallsBackOncePerDaemon(t *testing.T) {
	warnings := captureWarningLog(t)
	// /bin/false execs and exits without an ack, exactly like an af binary that
	// predates the protocol.
	forceTunnelLifelineExe(t, "/bin/false")
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}

	child := spawnTestTunnel(t, p)
	require.Nil(t, child.ll, "a non-acking lifeline must fall back to the direct spawn")
	require.Equal(t, "sh", child.cmd.Args[0], "the fallback is the direct sh -c spawn: %v", child.cmd.Args)
	require.Equal(t, "-c", child.cmd.Args[1])
	require.Equal(t, tunnelTestSleep+` -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -N -L "$1"`, child.cmd.Args[2])
	assert.True(t, sandboxTunnelLifelineUnavailable.Load(), "the failed probe must latch the fallback")

	// A second provision goes straight to the direct spawn and does NOT warn
	// again — the warning is once per daemon.
	warnings.Reset()
	child2 := spawnTestTunnel(t, &sandboxProvisioner{sshCmd: tunnelTestSleep})
	require.Nil(t, child2.ll)
	require.Equal(t, "sh", child2.cmd.Args[0])
	assert.NotContains(t, warnings.String(), "does not run the watch lifeline",
		"the degradation warning must be logged once per daemon, not once per provision")
}

// TestSandboxTunnelStampsHomeMarker pins the AF_SANDBOX_TUNNEL_HOME ownership
// stamp on BOTH spawn shapes: the startup sweep's kill decision reads exactly
// this variable, canonicalized the same way.
func TestSandboxTunnelStampsHomeMarker(t *testing.T) {
	home, err := sandboxTunnelHome()
	require.NoError(t, err)
	want := sandboxTunnelHomeEnv + "=" + home

	// Lifeline path: the marker lands on the lifeline and is inherited by the
	// whole group.
	forceTunnelLifelineExe(t, os.Args[0])
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child := spawnTestTunnel(t, p)
	require.NotNil(t, child.ll)
	assert.Contains(t, child.cmd.Env, want, "the lifeline child must carry the home marker")

	// Direct path: same stamp.
	forceTunnelLifelineExe(t, "")
	p2 := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child2 := spawnTestTunnel(t, p2)
	require.Nil(t, child2.ll)
	assert.Contains(t, child2.cmd.Env, want, "the direct child must carry the home marker")
}

// TestStopAllSandboxTunnelsStopsRegistered is the graceful-exit layer: every
// registered provisioner's tunnel dies — including one no instance row points
// at — while a provisioner mid-reap is left to its own stopTunnel.
func TestStopAllSandboxTunnelsStopsRegistered(t *testing.T) {
	forceTunnelLifelineExe(t, "")
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child := spawnTestTunnel(t, p)

	StopAllSandboxTunnels()
	requireGroupGone(t, child.cmd.Process.Pid, 5*time.Second)
	_, registered := liveSandboxTunnels.Load(p)
	assert.False(t, registered, "stopTunnel must unregister the provisioner")
	assert.Nil(t, p.tunnel)

	// A provisioner being reaped owns its own stop; StopAll skips it rather
	// than park behind the remote teardown.
	reaping := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	reaping.reaping.Store(true)
	child2 := spawnTestTunnel(t, reaping)
	StopAllSandboxTunnels()
	assert.True(t, alive(t, child2.cmd.Process.Pid),
		"a mid-reap provisioner must be skipped, not double-stopped")
}

// TestTunnelSpawnAfterStopKillsItself pins the race-closing half of
// provisionTunnelChild (#5564): a spawn that completes after the
// provisioner's stop already ran — a daemon exit that overtook the provision —
// must kill its own newborn and fail, never publish a tunnel no remaining
// stop can reach.
func TestTunnelSpawnAfterStopKillsItself(t *testing.T) {
	forceTunnelLifelineExe(t, "")
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	registerSandboxTunnel(p)

	// The stop lands first — the shape a daemon-exit StopAllSandboxTunnels
	// takes when it wins the race against this provision.
	p.reapMu.Lock()
	p.stopTunnel()
	p.reapMu.Unlock()

	p.forward = "127.0.0.1:45555:127.0.0.1:45556"
	child, err := p.provisionTunnelChild()
	require.ErrorIs(t, err, errTunnelOwnerStopped, "a spent provisioner must refuse the spawn")
	assert.Nil(t, child)

	// The newborn it briefly started is already dead and reaped — not even a
	// zombie of this test's children may carry the forward.
	for pid, proc := range mustSnapshot(t) {
		if proc.PPID != os.Getpid() {
			continue
		}
		for _, arg := range childArgv(pid) {
			assert.NotContains(t, arg, p.forward, "the refused tunnel's child must not survive: %v", arg)
		}
	}
}

// mustSnapshot reads the process table for the assertion above.
func mustSnapshot(t *testing.T) map[int]proctree.Process {
	t.Helper()
	procs, err := proctree.Snapshot()
	require.NoError(t, err)
	return procs
}

// childArgv reads one process's argv for the assertion above.
func childArgv(pid int) []string { return proctree.Argv(pid) }

// TestReapStopsTunnel pins the session-side path: reap's stopTunnel kills the
// group and unregisters the provisioner, whether or not a remote workspace
// exists to tear down.
func TestReapStopsTunnel(t *testing.T) {
	forceTunnelLifelineExe(t, "")
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child := spawnTestTunnel(t, p)

	require.NoError(t, p.reap())
	requireGroupGone(t, child.cmd.Process.Pid, 5*time.Second)
	_, registered := liveSandboxTunnels.Load(p)
	assert.False(t, registered)
}

// spawnSweepSleeper starts one sleeper with the given extra argv and env
// marker (nil = unmarked) for the sweep tests.
func spawnSweepSleeper(t *testing.T, home string, argv ...string) *exec.Cmd {
	t.Helper()
	var cmd *exec.Cmd
	if len(argv) == 0 {
		cmd = exec.Command("sleep", "60")
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
	}
	if home != "" {
		cmd.Env = append(os.Environ(), sandboxTunnelHomeEnv+"="+home)
	}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// spawnSshArgvSleeper starts a sleeper presenting the argv a leaked pre-#5564
// tunnel actually has once its `sh -c` wrapper exec's away — ssh-named with
// af's flag signature and NO af-sandbox element. That is the shape EVERY
// pre-fix leftover carries on darwin, where /bin/sh is a shell that always
// tail-optimizes; the & wait positional-command keeps this sh alive holding
// the original argv so the fixture is identical on every platform.
func spawnSshArgvSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 60 & wait",
		"-o", "ExitOnForwardFailure=yes", "-N", "-L", "127.0.0.1:40010:127.0.0.1:40011")
	cmd.Args[0] = "ssh"
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// requireReaped waits for OUR CHILD to exit — the honest "the sweep killed it"
// for a spawned sleeper, since a child killed but not yet waited on still
// answers kill(pid, 0) as a zombie.
func requireReaped(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("pid %d still running after %s — the sweep did not kill it", cmd.Process.Pid, timeout)
	}
}

// sweepReportedPIDs returns the PIDs the sweep's unmarked-candidate warning
// names (the comma list after "-p "). The sweep scans the runner's WHOLE process
// table, so a test may only assert about the PIDs of fixtures it spawned: the
// warning text and counts also carry whatever else on the host looks like a
// transport (a sibling test's dying sleeper, another package's test binary).
func sweepReportedPIDs(out string) map[int]bool {
	reported := map[int]bool{}
	_, rest, ok := strings.Cut(out, "ps -o pid,ppid,command -p ")
	if !ok {
		return reported
	}
	list, _, _ := strings.Cut(rest, "`")
	for _, f := range strings.Split(list, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			reported[n] = true
		}
	}
	return reported
}

// TestSweepOrphanSandboxTunnels is the startup-sweep contract: kill exactly
// the processes PROVEN to be this home's leftovers — same uid AND the
// AF_SANDBOX_TUNNEL_HOME marker — and only warn about unmarked af-sandbox
// shapes, never kill on argv alone.
func TestSweepOrphanSandboxTunnels(t *testing.T) {
	home, err := sandboxTunnelHome()
	require.NoError(t, err)

	ours := spawnSweepSleeper(t, home)
	foreign := spawnSweepSleeper(t, "/nonexistent/foreign-af-home")
	// `sleep 60 & wait`, not `sleep 60`: a bare last command is exec'd away by
	// shells that tail-optimize (darwin's /bin/sh is one), which sheds the
	// af-sandbox element from argv — the very thing this fixture exists to
	// carry. The & wait keeps sh alive holding its original argv on every
	// shell, so the element survives to be matched on linux and darwin alike.
	unmarkedTunnelShape := spawnSweepSleeper(t, "", "sh", "-c", "sleep 60 & wait", "af-sandbox")
	// The darwin shape of the same leftover: once a tail-optimizing sh exec's
	// the tunnel command away, only the ssh argv remains — no af-sandbox
	// element at all. It must still be reported, and still never killed.
	unmarkedSshShape := spawnSshArgvSleeper(t)
	unrelated := spawnSweepSleeper(t, "")

	warnings := captureWarningLog(t)
	SweepOrphanSandboxTunnels(home)

	requireReaped(t, ours, 5*time.Second)
	assert.True(t, alive(t, foreign.Process.Pid),
		"a tunnel stamped with a different AF home is never touched")
	assert.True(t, alive(t, unmarkedTunnelShape.Process.Pid),
		"an unmarked af-sandbox argv must only be reported, never killed")
	assert.True(t, alive(t, unmarkedSshShape.Process.Pid),
		"an unmarked ssh-argv leftover must only be reported, never killed")
	assert.True(t, alive(t, unrelated.Process.Pid))

	// Assert only about the fixtures this test spawned, by PID.
	reported := sweepReportedPIDs(warnings.String())
	assert.True(t, reported[unmarkedTunnelShape.Process.Pid],
		"an unmarked af-sandbox argv must be reported so the operator can inspect it")
	assert.True(t, reported[unmarkedSshShape.Process.Pid],
		"an unmarked ssh-argv leftover must be reported so the operator can inspect it")
	assert.False(t, reported[ours.Process.Pid], "a proven leftover is killed, not reported")
	assert.False(t, reported[foreign.Process.Pid], "a foreign-stamped tunnel is not ours to classify")
	assert.False(t, reported[unrelated.Process.Pid], "an unrelated process is not a candidate")
	assert.Contains(t, warnings.String(), "stop them by hand with `kill ",
		"the report must tell the operator how to stop proven-stale leftovers by hand")
}

// TestSweepOrphanSandboxTunnelsForeignOnly warns nothing when no unmarked
// candidate exists — a foreign home's live tunnels are not our report either.
func TestSweepOrphanSandboxTunnelsForeignOnly(t *testing.T) {
	home, err := sandboxTunnelHome()
	require.NoError(t, err)
	foreign := spawnSweepSleeper(t, "/nonexistent/foreign-af-home", "sh", "-c", "sleep 60", "af-sandbox")

	warnings := captureWarningLog(t)
	SweepOrphanSandboxTunnels(home)
	// By PID, not by the global warning text: the sweep scans the whole process
	// table, so another process may legitimately be reported.
	assert.False(t, sweepReportedPIDs(warnings.String())[foreign.Process.Pid],
		"a foreign-marked process must not even be reported — it is not ours to classify")
	assert.True(t, alive(t, foreign.Process.Pid), "a foreign-marked process is never killed")
}

// --- supervised connection (#5571 W1.2) ---

// startSupervisedTestTunnel spawns a tunnel AND its supervisor for the
// supervision tests below: the shared backoff ladder is shrunk so a re-dial
// lands inside a poll window (the same vars the ptybroker recovery tests
// shrink), the readiness probe is replaced by probeFn so the fake tunnel never
// needs a real listener, and cleanup stops everything via spawnTestTunnel's.
func startSupervisedTestTunnel(t *testing.T, p *sandboxProvisioner, probeFn func(label, localAddr string, timeout, poll time.Duration) error) (*tunnelChild, <-chan struct{}) {
	t.Helper()
	forceTunnelLifelineExe(t, "")
	prevInitial, prevMax, prevHealthy := redialInitialBackoff, redialMaxBackoff, redialHealthySpan
	redialInitialBackoff, redialMaxBackoff, redialHealthySpan = 20*time.Millisecond, 80*time.Millisecond, 120*time.Millisecond
	t.Cleanup(func() { redialInitialBackoff, redialMaxBackoff, redialHealthySpan = prevInitial, prevMax, prevHealthy })
	prevProbe := waitForSandboxTunnelFn
	waitForSandboxTunnelFn = probeFn
	t.Cleanup(func() { waitForSandboxTunnelFn = prevProbe })
	if p.localAddr == "" {
		p.localAddr = "127.0.0.1:40000"
	}
	child := spawnTestTunnel(t, p)
	done := make(chan struct{})
	go func() { p.superviseTunnel(); close(done) }()
	return child, done
}

// killTunnelGeneration fakes an unexpected transport death: the child's whole
// process group dies and its reaper reaps it. It waits on the kernel fact — a
// reaped leader stops answering kill(-pgid, 0) — rather than child.done or
// cmd.ProcessState, both of which the reaper owns (done is safe to read but
// proves nothing the group-gone probe doesn't; ProcessState is a data race
// under -race). The generation poll that follows is what proves the
// supervisor noticed.
func killTunnelGeneration(t *testing.T, child *tunnelChild) {
	t.Helper()
	require.NoError(t, syscall.Kill(-child.cmd.Process.Pid, syscall.SIGKILL))
	requireGroupGone(t, child.cmd.Process.Pid, 10*time.Second)
}

// requireTunnelGeneration polls until the provisioner publishes a generation
// other than prev (or the first one at all when prev is nil) — a re-dial, not
// just the death being noticed.
func requireTunnelGeneration(t *testing.T, p *sandboxProvisioner, prev *tunnelChild, timeout time.Duration) *tunnelChild {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.reapMu.Lock()
		got := p.tunnel
		p.reapMu.Unlock()
		if got != nil && got != prev {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no new tunnel generation after %s", timeout)
	return nil
}

// requireSupervisorExit polls for the monitor goroutine's return.
func requireSupervisorExit(t *testing.T, done <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("the transport supervisor did not exit within " + timeout.String())
	}
}

// TestSuperviseTunnelRedialsAfterDeath is the W1.2 core: a tunnel child that
// dies for any reason other than teardown is re-established — a NEW generation
// under the same provisioner, re-bound to the same localAddr spec.
func TestSuperviseTunnelRedialsAfterDeath(t *testing.T) {
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child, done := startSupervisedTestTunnel(t, p, func(string, string, time.Duration, time.Duration) error { return nil })
	// LIFO: this cleanup runs before spawnTestTunnel's, so it must stop the
	// provisioner itself before expecting the supervisor to exit (stopTunnel
	// is idempotent — spawnTestTunnel's teardown will harmlessly re-run it).
	t.Cleanup(func() {
		p.reapMu.Lock()
		p.stopTunnel()
		p.reapMu.Unlock()
		requireSupervisorExit(t, done, 5*time.Second)
	})

	killTunnelGeneration(t, child)
	next := requireTunnelGeneration(t, p, child, 10*time.Second)
	require.NotSame(t, child, next)
	require.True(t, alive(t, next.cmd.Process.Pid), "the re-dialed generation must be running")
	// The respawn re-reads the pinned spec: same script, same forward arg.
	assert.Equal(t, child.cmd.Args, next.cmd.Args)
}

// TestSuperviseTunnelStopsWithOwner covers both teardown signals: a parked-in-
// backoff supervisor and a parked-on-exit supervisor both return once stopped,
// and the daemon-exit path reaps the live generation.
func TestSuperviseTunnelStopsWithOwner(t *testing.T) {
	// Parked on a live generation's exit chan.
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child, done := startSupervisedTestTunnel(t, p, func(string, string, time.Duration, time.Duration) error { return nil })
	p.reapMu.Lock()
	p.stopTunnel()
	p.reapMu.Unlock()
	requireSupervisorExit(t, done, 5*time.Second)
	requireGroupGone(t, child.cmd.Process.Pid, 5*time.Second)

	// Parked in a re-dial backoff: the child died, the dial is delayed, and
	// stoppedCh must wake the sleep rather than let it finish.
	p2 := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child2, done2 := startSupervisedTestTunnel(t, p2, func(string, string, time.Duration, time.Duration) error {
		return errors.New("the forward never answers")
	})
	killTunnelGeneration(t, child2)
	// The probe fails every time, so the supervisor is inside the
	// fail→backoff→spawn cycle continuously.
	p2.reapMu.Lock()
	p2.stopTunnel()
	p2.reapMu.Unlock()
	requireSupervisorExit(t, done2, 5*time.Second)
}

// TestSuperviseTunnelNoRedialOnTeardown pins the other half of "any exit other
// than teardown": a child killed BY the stop path is never re-established —
// the supervisor sees the latch, not an outage.
func TestSuperviseTunnelNoRedialOnTeardown(t *testing.T) {
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child, done := startSupervisedTestTunnel(t, p, func(string, string, time.Duration, time.Duration) error { return nil })
	t.Cleanup(func() { requireSupervisorExit(t, done, 5*time.Second) })

	p.reapMu.Lock()
	p.stopTunnel()
	p.reapMu.Unlock()

	// Give the supervisor every chance to mis-fire: several shrunken backoff
	// intervals plus the probe's own poll would have produced a new generation
	// if teardown were read as an outage.
	time.Sleep(400 * time.Millisecond)
	p.reapMu.Lock()
	assert.Nil(t, p.tunnel, "teardown must never be re-dialed")
	p.reapMu.Unlock()
	requireGroupGone(t, child.cmd.Process.Pid, 5*time.Second)
}

// TestSuperviseTunnelDialFailureKeepsRetrying: a re-dial whose spawn never
// answers the readiness probe is killed and retried — the backoff ladder
// keeps climbing instead of giving up or spinning.
func TestSuperviseTunnelDialFailureKeepsRetrying(t *testing.T) {
	var probes atomic.Int32
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	child, done := startSupervisedTestTunnel(t, p, func(string, string, time.Duration, time.Duration) error {
		probes.Add(1)
		return errors.New("the forward never answers")
	})
	t.Cleanup(func() {
		p.reapMu.Lock()
		p.stopTunnel()
		p.reapMu.Unlock()
		requireSupervisorExit(t, done, 5*time.Second)
	})

	killTunnelGeneration(t, child)
	// Each failed re-dial costs one probe; several probes mean the loop is
	// retrying, not parked.
	deadline := time.Now().Add(10 * time.Second)
	for probes.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d re-dial probes after a dead tunnel — the supervisor stopped retrying", probes.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSuperviseTunnelBackoffBounds pins the jittered ladder: doubling, capped,
// and smeared ±25% around redialDelay — the "capped, jittered backoff" half of
// the property.
func TestSuperviseTunnelBackoffBounds(t *testing.T) {
	prevInitial, prevMax := redialInitialBackoff, redialMaxBackoff
	redialInitialBackoff, redialMaxBackoff = 100*time.Millisecond, 800*time.Millisecond
	t.Cleanup(func() { redialInitialBackoff, redialMaxBackoff = prevInitial, prevMax })
	// Deterministic jitter: rand=0.5 gives the neutral factor.
	prevRand := tunnelRedialRand
	tunnelRedialRand = func() float64 { return 0.5 }
	t.Cleanup(func() { tunnelRedialRand = prevRand })

	for attempt, want := range []time.Duration{100, 200, 400, 800, 800, 800} {
		if got := tunnelRedialDelay(attempt); got != want*time.Millisecond {
			t.Errorf("tunnelRedialDelay(%d) = %s, want %s", attempt, got, want*time.Millisecond)
		}
	}
	// The smear: extreme jitter stays inside ±25% of the ladder — including
	// slightly over the nominal cap at the top, which is the point of jitter.
	for _, r := range []float64{0, 1} {
		tunnelRedialRand = func() float64 { return r }
		for attempt := 0; attempt < 8; attempt++ {
			base := float64(redialDelay(attempt))
			got := float64(tunnelRedialDelay(attempt))
			if got < base*0.75-1 || got > base*1.25+1 {
				t.Fatalf("tunnelRedialDelay(%d) with rand=%v = %v, outside ±25%% of %v", attempt, r, got, base)
			}
		}
	}
}

// TestTunnelSpawnsReverseForwardSlot is W3.4's slot: reverse forward specs set
// on the provisioner are composed into the spawn as `-R "$N"` after the -L —
// the same ExitOnForwardFailure fatality and the same supervision, today with
// the slot empty.
func TestTunnelSpawnsReverseForwardSlot(t *testing.T) {
	forceTunnelLifelineExe(t, "")
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	p.forward = "127.0.0.1:40000:127.0.0.1:40001"
	p.reverseFwds = []string{"127.0.0.1:9000:127.0.0.1:7070", "0:127.0.0.1:8080"}
	registerSandboxTunnel(p)
	child, err := p.provisionTunnelChild()
	require.NoError(t, err)
	t.Cleanup(func() {
		p.reapMu.Lock()
		p.stopTunnel()
		p.reapMu.Unlock()
	})

	want := tunnelTestSleep + ` -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -N -L "$1" -R "$2" -R "$3"`
	assert.Equal(t, want, child.cmd.Args[2], "the spawn script composes each -R as a positional arg: %v", child.cmd.Args)
	assert.Equal(t, "127.0.0.1:40000:127.0.0.1:40001", child.cmd.Args[4])
	assert.Equal(t, "127.0.0.1:9000:127.0.0.1:7070", child.cmd.Args[5])
	assert.Equal(t, "0:127.0.0.1:8080", child.cmd.Args[6])
}
