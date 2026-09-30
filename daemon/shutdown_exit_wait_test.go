package daemon

import (
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"strconv"
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
	return startFakeDaemonNamed(t, home, "af", script)
}

// startFakeDaemonNamed is startFakeAFDaemon with the binary's basename chosen by
// the caller, for a daemon installed under a name other than `af` — which
// InstallAutostart permits, and which isAgentFactoryDaemon does not recognize.
func startFakeDaemonNamed(t *testing.T, home, basename, script string) (int, <-chan *os.ProcessState) {
	t.Helper()
	argv0 := filepath.Join(fakeBinDir(t), basename)
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

// TestWaitForShutdownCompletionNeverSignalsAtBound: a daemon still alive at
// shutdownCompleteGrace may be joining durable work in drainDaemon (root-agent
// creates, admitted mutations — #3721) with its control socket already closed,
// so from outside it cannot be told apart from a wedged one, and a kill there
// can corrupt session state. The bound must end in an error with the process
// untouched — whether it serves this home or another.
func TestWaitForShutdownCompletionNeverSignalsAtBound(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv rewrite needs /proc to observe")
	}
	for _, tc := range []struct {
		name        string
		foreignHome bool
	}{
		{name: "this home's daemon"},
		{name: "another home's daemon", foreignHome: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := testguard.SocketTempDir(t)
			t.Setenv("AGENT_FACTORY_HOME", home)

			prevGrace := shutdownCompleteGrace
			shutdownCompleteGrace = 300 * time.Millisecond
			t.Cleanup(func() { shutdownCompleteGrace = prevGrace })

			daemonHome := home
			if tc.foreignHome {
				daemonHome = t.TempDir()
			}
			pid, exited := startFakeAFDaemon(t, daemonHome, "sleep 60; :")

			err := WaitForShutdownCompletion(pid)
			if !errors.Is(err, ErrShutdownIncomplete) {
				t.Fatalf("WaitForShutdownCompletion(%d) = %v, want an error wrapping ErrShutdownIncomplete while the daemon is still running", pid, err)
			}
			if !pidLooksAlive(pid) {
				t.Fatalf("daemon pid %d is gone after the wait; the bound must never signal it", pid)
			}
			// Give a signal the wait might have sent time to land before
			// concluding none did.
			select {
			case state := <-exited:
				t.Fatalf("daemon pid %d exited (%v) after the wait; the bound must never signal it", pid, state)
			case <-time.After(500 * time.Millisecond):
			}
		})
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
	if !waitForDrainingDaemonExit(pid, time.Now().Add(testSpawnReadyTimeout)) {
		t.Fatalf("waitForDrainingDaemonExit reported pid %d still there after it exited", pid)
	}
	if pidLooksAlive(pid) {
		t.Fatalf("waitForDrainingDaemonExit returned while pid %d was still alive", pid)
	}

	wedged, _ := startFakeAFDaemon(t, home, "sleep 60; :")
	start := time.Now()
	if waitForDrainingDaemonExit(wedged, time.Now().Add(200*time.Millisecond)) {
		t.Fatalf("waitForDrainingDaemonExit reported a still-running pid %d as gone", wedged)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waitForDrainingDaemonExit overran its bound: %s", elapsed)
	}
	if !pidLooksAlive(wedged) {
		t.Fatalf("waitForDrainingDaemonExit must never signal the process it waits on")
	}
}

// TestWaitForShutdownCompletionWaitsOnRenamedDaemonBinary: a PID the daemon
// reported for itself names the stopped daemon whatever its binary is called.
// The wait must not discard it because the basename is not `af`: falling back
// to socket polling reopens #5007, since the socket can vanish while teardown
// still holds the per-home lock and the replacement then fails to start.
func TestWaitForShutdownCompletionWaitsOnRenamedDaemonBinary(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv rewrite needs /proc to observe")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pid, exited := startFakeDaemonNamed(t, home, "af-renamed", "sleep 0.5; exit 0")
	if isAgentFactoryDaemon(pid) {
		t.Fatalf("fixture invalid: a renamed binary must not pass the basename heuristic")
	}

	if err := WaitForShutdownCompletion(pid); err != nil {
		t.Fatalf("WaitForShutdownCompletion(%d): %v", pid, err)
	}
	if pidLooksAlive(pid) {
		t.Fatalf("WaitForShutdownCompletion returned while renamed daemon pid %d was still alive", pid)
	}
	select {
	case state := <-exited:
		if state == nil || !state.Exited() || state.ExitCode() != 0 {
			t.Fatalf("renamed daemon exit state = %v, want a clean exit, not a signal", state)
		}
	case <-time.After(testSpawnReadyTimeout):
		t.Fatalf("renamed fake daemon was never reaped")
	}
}

// TestWaitForDrainingDaemonExitWaitsOnRenamedDaemonBinary: PingResponse.PID is
// always self-reported, so the drain wait trusts it under any basename too.
func TestWaitForDrainingDaemonExitWaitsOnRenamedDaemonBinary(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv rewrite needs /proc to observe")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pid, _ := startFakeDaemonNamed(t, home, "af-renamed", "sleep 0.4; exit 0")
	waitForDrainingDaemonExit(pid, time.Now().Add(testSpawnReadyTimeout))
	if pidLooksAlive(pid) {
		t.Fatalf("waitForDrainingDaemonExit returned while renamed daemon pid %d was still alive", pid)
	}
}

// preShutdownPIDControl is the control service of a daemon built before
// ShutdownResponse.PID existed: its Shutdown ack carries no PID, but its Ping
// self-reports one (PingResponse.PID predates the Shutdown field). pingFails
// models a Ping that errors, so RequestShutdown's fallbacks are exercised.
type preShutdownPIDControl struct {
	pingPID   int
	pingPhase DaemonPhase
	pingFails bool
}

func (c *preShutdownPIDControl) Ping(_ PingRequest, resp *PingResponse) error {
	if c.pingFails {
		return errors.New("ping unavailable")
	}
	resp.OK = true
	resp.PID = c.pingPID
	resp.Phase = c.pingPhase
	return nil
}

func (c *preShutdownPIDControl) Shutdown(_ ShutdownRequest, resp *ShutdownResponse) error {
	resp.OK = true // and no PID: the pre-field reply shape
	return nil
}

// startFakeControlService binds svc as the control service on this test home's
// socket, mirroring startControlServer's bind, and closes it on cleanup.
func startFakeControlService(t *testing.T, svc any) {
	t.Helper()
	socketPath, err := DaemonSocketPath()
	if err != nil {
		t.Fatalf("DaemonSocketPath: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen %s: %v", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		t.Fatalf("chmod socket: %v", err)
	}
	server := rpc.NewServer()
	if err := server.RegisterName(controlServiceName, svc); err != nil {
		_ = listener.Close()
		t.Fatalf("register fake control service: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.ServeConn(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
}

func writeTestPIDFile(t *testing.T, pid int) {
	t.Helper()
	path, err := daemonPIDFilePath()
	if err != nil {
		t.Fatalf("daemonPIDFilePath: %v", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
}

// TestRequestShutdownTakesPingPIDFromPrePIDDaemon: a daemon predating
// ShutdownResponse.PID acks with PID 0. When it is a renamed install, the PID
// file fallback cannot name it (the basename check rejects `af-renamed`), so
// without the Ping self-report RequestShutdown returned 0 and the respawn fell
// back to the socket race #5007 closes. The Ping PID — captured before the
// Shutdown, while the daemon is provably up — must be returned.
func TestRequestShutdownTakesPingPIDFromPrePIDDaemon(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv rewrite needs /proc to observe")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	fakePID, _ := startFakeDaemonNamed(t, home, "af-renamed", "sleep 60; :")
	if isAgentFactoryDaemon(fakePID) {
		t.Fatalf("fixture invalid: a renamed binary must not pass the basename heuristic")
	}
	writeTestPIDFile(t, fakePID)
	startFakeControlService(t, &preShutdownPIDControl{pingPID: fakePID})

	result, pid, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if result != ShutdownViaRPC || pid != fakePID {
		t.Fatalf("RequestShutdown = (%v, %d), want (ShutdownViaRPC, %d) — the Ping self-report must name a pre-PID daemon", result, pid, fakePID)
	}
}

// TestRequestShutdownFallsBackToVerifiedPIDFileWhenPingFails: the Ping capture
// adds a source, it does not replace one. A failed Ping leaves the verified
// PID-file fallback exactly as it was.
func TestRequestShutdownFallsBackToVerifiedPIDFileWhenPingFails(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv rewrite needs /proc to observe")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	fakePID, _ := startFakeAFDaemon(t, home, "sleep 60; :")
	writeTestPIDFile(t, fakePID)
	startFakeControlService(t, &preShutdownPIDControl{pingFails: true})

	result, pid, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if result != ShutdownViaRPC || pid != fakePID {
		t.Fatalf("RequestShutdown = (%v, %d), want (ShutdownViaRPC, %d) from the verified PID file", result, pid, fakePID)
	}
}

// TestEnsureDaemonRefusesToRaceStillDrainingDaemon: a responder still reporting
// DaemonPhaseQuiescing at the drain-wait deadline is finishing durable work
// (#3721). EnsureDaemon must report ErrDaemonStillDraining and leave it alone —
// not fall through to its launch path, whose stale-daemon stop SIGTERMs the PID
// file's daemon and escalates to SIGKILL. The PID file here names the draining
// fake, so the old fall-through killed it.
func TestEnsureDaemonRefusesToRaceStillDrainingDaemon(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv rewrite needs /proc to observe")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	fakePID, _ := startFakeAFDaemon(t, home, "sleep 60; :")
	writeTestPIDFile(t, fakePID)
	startFakeControlService(t, &preShutdownPIDControl{pingPID: fakePID, pingPhase: DaemonPhaseQuiescing})

	launches := 0
	launch := func() error {
		launches++
		return errors.New("test launcher: must not be reached")
	}
	err := ensureDaemonWithLauncherUntil(launch, time.Now().Add(1500*time.Millisecond))
	if !errors.Is(err, ErrDaemonStillDraining) {
		t.Fatalf("ensure against a still-draining daemon = %v, want ErrDaemonStillDraining", err)
	}
	if launches != 0 {
		t.Fatalf("launches = %d, want 0 while the old daemon is still draining", launches)
	}
	if !pidLooksAlive(fakePID) {
		t.Fatalf("the draining daemon (pid %d) was stopped; it must be left to finish", fakePID)
	}
}
