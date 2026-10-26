package session

import (
	"errors"
	"os"
	"os/exec"
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
func spawnTestTunnel(t *testing.T, p *sandboxProvisioner) (*exec.Cmd, *tunnelLifeline) {
	t.Helper()
	registerSandboxTunnel(p)
	cmd, ll, err := p.provisionTunnelChild("127.0.0.1:40000:127.0.0.1:40001")
	require.NoError(t, err)
	require.NotNil(t, cmd)
	t.Cleanup(func() {
		p.reapMu.Lock()
		p.stopTunnel()
		p.reapMu.Unlock()
	})
	return cmd, ll
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
	cmd, ll := spawnTestTunnel(t, p)
	require.NotNil(t, ll, "the tunnel must run under the lifeline when the binary speaks it")

	// The inner tunnel command reaches the lifeline verbatim: sh -c '<sshCmd>
	// -o ExitOnForwardFailure=yes -N -L "$1"' af-sandbox <forward> — argv[6:]
	// after '<exe> __watch-lifeline v1 <ms> --'. The flag is the
	// failed-bind-does-not-look-healthy pin of
	// TestSandboxTunnelRefusesToOutliveAFailedForward.
	require.Len(t, cmd.Args, 10, "lifeline argv: exe __watch-lifeline v1 <ms> -- sh -c <script> af-sandbox <forward>: %v", cmd.Args)
	assert.Equal(t, watchlifeline.Arg, cmd.Args[1])
	assert.Equal(t, watchlifeline.Protocol, cmd.Args[2])
	assert.Equal(t, "sh", cmd.Args[5])
	assert.Equal(t, "-c", cmd.Args[6])
	assert.Equal(t, tunnelTestSleep+` -o ExitOnForwardFailure=yes -N -L "$1"`, cmd.Args[7])
	assert.Equal(t, "af-sandbox", cmd.Args[8])
	assert.Equal(t, "127.0.0.1:40000:127.0.0.1:40001", cmd.Args[9])

	pgid := cmd.Process.Pid
	// The daemon's half of the death pact is closing this write end — exactly
	// what the kernel does for a dead daemon's descriptors.
	require.NoError(t, ll.keepW.Close())

	// The lifeline ends by its own group SIGKILL — Wait both proves that and
	// reaps it, and only a reaped leader stops counting for kill(-pgid, 0): a
	// zombie still answers a group probe on this kernel.
	waitErr := cmd.Wait()
	require.Error(t, waitErr, "the lifeline ends by its own group SIGKILL")
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

	cmd, ll := spawnTestTunnel(t, p)
	require.Nil(t, ll, "a non-acking lifeline must fall back to the direct spawn")
	require.Equal(t, "sh", cmd.Args[0], "the fallback is the direct sh -c spawn: %v", cmd.Args)
	require.Equal(t, "-c", cmd.Args[1])
	require.Equal(t, tunnelTestSleep+` -o ExitOnForwardFailure=yes -N -L "$1"`, cmd.Args[2])
	assert.True(t, sandboxTunnelLifelineUnavailable.Load(), "the failed probe must latch the fallback")

	// A second provision goes straight to the direct spawn and does NOT warn
	// again — the warning is once per daemon.
	warnings.Reset()
	cmd2, ll2 := spawnTestTunnel(t, &sandboxProvisioner{sshCmd: tunnelTestSleep})
	require.Nil(t, ll2)
	require.Equal(t, "sh", cmd2.Args[0])
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
	cmd, ll := spawnTestTunnel(t, p)
	require.NotNil(t, ll)
	assert.Contains(t, cmd.Env, want, "the lifeline child must carry the home marker")

	// Direct path: same stamp.
	forceTunnelLifelineExe(t, "")
	p2 := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	cmd2, ll2 := spawnTestTunnel(t, p2)
	require.Nil(t, ll2)
	assert.Contains(t, cmd2.Env, want, "the direct child must carry the home marker")
}

// TestStopAllSandboxTunnelsStopsRegistered is the graceful-exit layer: every
// registered provisioner's tunnel dies — including one no instance row points
// at — while a provisioner mid-reap is left to its own stopTunnel.
func TestStopAllSandboxTunnelsStopsRegistered(t *testing.T) {
	forceTunnelLifelineExe(t, "")
	p := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	cmd, _ := spawnTestTunnel(t, p)

	StopAllSandboxTunnels()
	requireGroupGone(t, cmd.Process.Pid, 5*time.Second)
	_, registered := liveSandboxTunnels.Load(p)
	assert.False(t, registered, "stopTunnel must unregister the provisioner")
	assert.Nil(t, p.tunnel)

	// A provisioner being reaped owns its own stop; StopAll skips it rather
	// than park behind the remote teardown.
	reaping := &sandboxProvisioner{sshCmd: tunnelTestSleep}
	reaping.reaping.Store(true)
	cmd2, _ := spawnTestTunnel(t, reaping)
	StopAllSandboxTunnels()
	assert.True(t, alive(t, cmd2.Process.Pid),
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

	forward := "127.0.0.1:45555:127.0.0.1:45556"
	cmd, ll, err := p.provisionTunnelChild(forward)
	require.Error(t, err, "a spent provisioner must refuse the spawn")
	assert.Contains(t, err.Error(), "already stopping")
	assert.Nil(t, cmd)
	assert.Nil(t, ll)

	// The newborn it briefly started is already dead and reaped — not even a
	// zombie of this test's children may carry the forward.
	for pid, proc := range mustSnapshot(t) {
		if proc.PPID != os.Getpid() {
			continue
		}
		for _, arg := range childArgv(pid) {
			assert.NotContains(t, arg, forward, "the refused tunnel's child must not survive: %v", arg)
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
	cmd, _ := spawnTestTunnel(t, p)

	require.NoError(t, p.reap())
	requireGroupGone(t, cmd.Process.Pid, 5*time.Second)
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

// TestSweepOrphanSandboxTunnels is the startup-sweep contract: kill exactly
// the processes PROVEN to be this home's leftovers — same uid AND the
// AF_SANDBOX_TUNNEL_HOME marker — and only warn about unmarked af-sandbox
// shapes, never kill on argv alone.
func TestSweepOrphanSandboxTunnels(t *testing.T) {
	home, err := sandboxTunnelHome()
	require.NoError(t, err)

	ours := spawnSweepSleeper(t, home)
	foreign := spawnSweepSleeper(t, "/nonexistent/foreign-af-home")
	unmarkedTunnelShape := spawnSweepSleeper(t, "", "sh", "-c", "sleep 60", "af-sandbox")
	unrelated := spawnSweepSleeper(t, "")

	warnings := captureWarningLog(t)
	SweepOrphanSandboxTunnels(home)

	requireReaped(t, ours, 5*time.Second)
	assert.True(t, alive(t, foreign.Process.Pid),
		"a tunnel stamped with a different AF home is never touched")
	assert.True(t, alive(t, unmarkedTunnelShape.Process.Pid),
		"an unmarked af-sandbox argv must only be reported, never killed")
	assert.True(t, alive(t, unrelated.Process.Pid))

	out := warnings.String()
	assert.Contains(t, out, "AF_SANDBOX_TUNNEL_HOME")
	assert.Contains(t, out, "no "+sandboxTunnelHomeEnv+" ownership stamp",
		"the report must say why af did not act")
	assert.Contains(t, out, "kill",
		"the report must tell the operator how to stop proven-stale leftovers by hand")
}

// TestSweepOrphanSandboxTunnelsForeignOnly warns nothing when no unmarked
// candidate exists — a foreign home's live tunnels are not our report either.
func TestSweepOrphanSandboxTunnelsForeignOnly(t *testing.T) {
	home, err := sandboxTunnelHome()
	require.NoError(t, err)
	spawnSweepSleeper(t, "/nonexistent/foreign-af-home", "sh", "-c", "sleep 60", "af-sandbox")

	warnings := captureWarningLog(t)
	SweepOrphanSandboxTunnels(home)
	assert.NotContains(t, warnings.String(), "ownership stamp",
		"a foreign-marked process must not even be reported — it is not ours to classify")
}
