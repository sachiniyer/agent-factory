package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// Tests for #5007: the post-upgrade respawn must not start a new daemon while
// the old one can still be mistaken for live. The wait watches the stopped
// daemon's PID exit (a positive signal, not a wall-clock budget), a Shutdown
// ack flips the responder to DaemonPhaseQuiescing at once, and every liveness
// check on the respawn path treats a quiescing responder as leaving, not
// serving. Every test points AGENT_FACTORY_HOME at a private temp dir and every
// process it waits on or signals is a fake it spawned itself — the host's real
// daemon is never pinged, signaled, or spawned.

// startFakeAFDaemon starts a fake `af --daemon` running script, whose environ
// carries AGENT_FACTORY_HOME=home, and waits until its argv is the crafted one
// (so isAgentFactoryDaemon classifies it). The returned channel receives the
// process's exit status: it is reaped concurrently, so an exited fake never
// lingers as a zombie a liveness check could misread.
func startFakeAFDaemon(t *testing.T, home, script string) (int, <-chan *os.ProcessState) {
	t.Helper()
	argv0 := filepath.Join(fakeBinDir(t), "af")
	cmd := fakeDaemonCmd(t, argv0, script, "--daemon")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "AGENT_FACTORY_HOME=" + home}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan *os.ProcessState, 1)
	go func() {
		state, _ := cmd.Process.Wait()
		exited <- state
	}()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	waitForArgv(t, pid, argv0)
	return pid, exited
}

// TestWaitForShutdownCompletionWaitsForPIDExit: with the stopped daemon's PID
// in hand, the wait must return only once that process has exited. There is no
// control socket at all here, so the pre-#5007 socket-only wait returned on its
// first ping while the daemon was still alive — the exact window in which a
// respawn's startup ping mistook the dying daemon for a live one.
func TestWaitForShutdownCompletionWaitsForPIDExit(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv classification needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pid, exited := startFakeAFDaemon(t, home, "sleep 0.5; exit 0")

	if err := WaitForShutdownCompletion(pid); err != nil {
		t.Fatalf("WaitForShutdownCompletion(%d): %v", pid, err)
	}
	if pidLooksAlive(pid) {
		t.Fatalf("WaitForShutdownCompletion returned while daemon pid %d was still alive (#5007)", pid)
	}
	select {
	case state := <-exited:
		if state == nil || !state.Exited() || state.ExitCode() != 0 {
			t.Fatalf("fake daemon exit state = %v, want a clean exit (the wait must not signal a daemon that leaves on its own)", state)
		}
	case <-time.After(testSpawnReadyTimeout):
		t.Fatalf("fake daemon was never reaped")
	}
}

// TestWaitForShutdownCompletionEscalatesWedgedDaemon: a daemon that acked
// Shutdown but is still alive at the bound is wedged. It must be SIGKILLed
// (after re-verifying it is this home's af daemon) and the wait must return nil
// once it is confirmed gone — never a blind respawn beside a live daemon.
func TestWaitForShutdownCompletionEscalatesWedgedDaemon(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("AF-home verification needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 300 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })

	pid, exited := startFakeAFDaemon(t, home, "sleep 60; :")

	if err := WaitForShutdownCompletion(pid); err != nil {
		t.Fatalf("WaitForShutdownCompletion(%d) on a wedged daemon: %v", pid, err)
	}
	select {
	case state := <-exited:
		ws, ok := state.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("wedged daemon exit state = %v, want death by SIGKILL", state)
		}
	case <-time.After(testSpawnReadyTimeout):
		t.Fatalf("wedged daemon pid %d was never killed", pid)
	}
}

// TestWaitForShutdownCompletionRefusesToKillForeignHomeDaemon: the escalation
// re-verifies the PID before signaling. An af daemon serving a DIFFERENT home
// (a recycled PID, or a misreported handle) is never ours to kill: the wait
// must return an error and leave it running.
func TestWaitForShutdownCompletionRefusesToKillForeignHomeDaemon(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("AF-home verification needs /proc")
	}
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 300 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })

	pid, _ := startFakeAFDaemon(t, t.TempDir(), "sleep 60; :")

	err := WaitForShutdownCompletion(pid)
	if err == nil {
		t.Fatalf("WaitForShutdownCompletion must refuse to escalate against another home's daemon")
	}
	if !pidLooksAlive(pid) {
		t.Fatalf("a foreign-home daemon (pid %d) was killed; it must be left running", pid)
	}
}

// TestShutdownAckReportsPIDAndQuiescing: the Shutdown ack names the process
// that received it and flips the lifecycle to quiescing at once, so every Ping
// answered during the teardown tail reads "leaving", never "ready".
func TestShutdownAckReportsPIDAndQuiescing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	manager := &Manager{lifecycle: readyLifecycle(t)}
	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	if resp, err := pingDaemonResponse(); err != nil || resp.Phase != DaemonPhaseReady {
		t.Fatalf("before Shutdown: ping = %+v, %v; want phase %q", resp, err, DaemonPhaseReady)
	}

	var resp ShutdownResponse
	if err := callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp); err != nil {
		t.Fatalf("rpc Shutdown: %v", err)
	}
	if !resp.OK || resp.PID != os.Getpid() {
		t.Fatalf("Shutdown ack = %+v, want OK with PID %d", resp, os.Getpid())
	}
	ping, err := pingDaemonResponse()
	if err != nil {
		t.Fatalf("ping after Shutdown ack: %v", err)
	}
	if ping.Phase != DaemonPhaseQuiescing {
		t.Fatalf("phase after Shutdown ack = %q, want %q", ping.Phase, DaemonPhaseQuiescing)
	}
}

// TestQuiescingIsTerminal: a Shutdown acked mid-warm-up must not be undone by
// a restore that completes in the ack's grace window.
func TestQuiescingIsTerminal(t *testing.T) {
	l, err := newDaemonLifecycle("", "", "")
	if err != nil {
		t.Fatalf("newDaemonLifecycle: %v", err)
	}
	l.markQuiescing()
	l.markRestoreComplete()
	if err := l.markReady(); err != nil {
		t.Fatalf("markReady after quiescing: %v", err)
	}
	if got := l.snapshot().phase; got != DaemonPhaseQuiescing {
		t.Fatalf("phase = %q after markReady on a quiescing daemon, want %q", got, DaemonPhaseQuiescing)
	}
}

// startDrainingControlServer binds a control server whose lifecycle is
// quiescing — a daemon that acked Shutdown — and closes it after linger,
// modeling the teardown tail.
func startDrainingControlServer(t *testing.T, linger time.Duration) {
	t.Helper()
	lifecycle := readyLifecycle(t)
	lifecycle.markQuiescing()
	closeFn, err := startControlServer(&Manager{lifecycle: lifecycle}, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(linger)
		_ = closeFn()
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(testSpawnReadyTimeout):
			t.Errorf("draining control server never closed")
		}
	})
}

// TestEnsureDaemonSpawnsWhenResponderIsDraining: a responder reporting
// DaemonPhaseQuiescing is leaving, not serving. EnsureDaemon must wait it out
// and spawn, not return nil against it. Pre-#5007 the ping succeeded and
// EnsureDaemon returned with zero spawns, leaving no daemon once it exited.
func TestEnsureDaemonSpawnsWhenResponderIsDraining(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	startDrainingControlServer(t, 300*time.Millisecond)

	spawns := 0
	var newDaemonClose func() error
	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error {
		spawns++
		var bindErr error
		newDaemonClose, bindErr = startControlServer(nil, nil, nil, nil)
		return bindErr
	}
	t.Cleanup(func() {
		launchDaemonProcessFn = prevLaunch
		if newDaemonClose != nil {
			_ = newDaemonClose()
		}
	})

	if err := EnsureDaemon(); err != nil {
		t.Fatalf("EnsureDaemon against a draining responder: %v", err)
	}
	if spawns != 1 {
		t.Fatalf("daemon spawns = %d, want 1 — a quiescing responder must not count as a live daemon (#5007)", spawns)
	}
}

// TestWaitForDaemonReadyIgnoresDrainingResponder: readiness means a daemon that
// will stay; a quiescing responder is not one, and the expiry says why.
func TestWaitForDaemonReadyIgnoresDrainingResponder(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	startDrainingControlServer(t, time.Second)

	err := waitForDaemonReady(time.Now().Add(300 * time.Millisecond))
	if err == nil {
		t.Fatalf("waitForDaemonReady accepted a quiescing responder as ready")
	}
	if !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("waitForDaemonReady error = %v, want it to say the daemon is shutting down", err)
	}
}

// TestDaemonAlreadyServingWaitsOutDrainingResponder covers RunDaemon's startup
// guard: a live responder counts as already serving; a quiescing one does not,
// and the guard returns only once it has gone.
func TestDaemonAlreadyServingWaitsOutDrainingResponder(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	closeLive, err := startControlServer(&Manager{lifecycle: readyLifecycle(t)}, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	if !daemonAlreadyServing() {
		t.Fatalf("a ready responder must count as already serving")
	}
	_ = closeLive()

	startDrainingControlServer(t, 300*time.Millisecond)
	if daemonAlreadyServing() {
		t.Fatalf("a quiescing responder must not count as already serving (#5007)")
	}
	if pingDaemon() == nil {
		t.Fatalf("daemonAlreadyServing returned while the draining responder still answered")
	}
}

// TestWaitForDrainingDaemonExit: the best-effort wait returns once the process
// exits, and at the bound when it does not — it never hangs.
func TestWaitForDrainingDaemonExit(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv classification needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pid, _ := startFakeAFDaemon(t, home, "sleep 0.4; exit 0")
	waitForDrainingDaemonExit(pid, time.Now().Add(testSpawnReadyTimeout))
	if pidLooksAlive(pid) {
		t.Fatalf("waitForDrainingDaemonExit returned while pid %d was still alive", pid)
	}

	wedged, _ := startFakeAFDaemon(t, home, "sleep 60; :")
	start := time.Now()
	waitForDrainingDaemonExit(wedged, time.Now().Add(200*time.Millisecond))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waitForDrainingDaemonExit overran its bound: %s", elapsed)
	}
	if !pidLooksAlive(wedged) {
		t.Fatalf("waitForDrainingDaemonExit must never signal the process it waits on")
	}
}
