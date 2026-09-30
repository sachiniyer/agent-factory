package daemon

import (
	"errors"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
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

// TestWaitForShutdownCompletion_DrainingToExited_PIDDies: with the stopped daemon's PID
// in hand, the wait must return only once that process has exited. There is no
// control socket at all here, so the pre-#5007 socket-only wait returned on its
// first ping while the daemon was still alive — the exact window in which a
// respawn's startup ping mistook the dying daemon for a live one.
func TestWaitForShutdownCompletion_DrainingToExited_PIDDies(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv classification needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pid, exited := startFakeAFDaemon(t, home, "sleep 0.5; exit 0")

	if err := WaitForShutdownCompletion(ShutdownPID{PID: pid, Confirmed: true}); err != nil {
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

// TestWaitForShutdownCompletion_Draining_BoundNeverSignals: a daemon still alive at
// shutdownCompleteGrace may be joining durable work in drainDaemon (root-agent
// creates, admitted mutations — #3721) with its control socket already closed,
// so from outside it cannot be told apart from a wedged one, and a kill there
// can corrupt session state. The bound must end in an error with the process
// untouched — whether it serves this home or another.
func TestWaitForShutdownCompletion_Draining_BoundNeverSignals(t *testing.T) {
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

			err := WaitForShutdownCompletion(ShutdownPID{PID: pid, Confirmed: true})
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

// TestShutdown_ServingToDraining_AckCarriesPIDAndQuiesces: the Shutdown ack names the process
// that received it and flips the lifecycle to quiescing at once, so every Ping
// answered during the teardown tail reads "leaving", never "ready".
func TestShutdown_ServingToDraining_AckCarriesPIDAndQuiesces(t *testing.T) {
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

// TestLifecycle_Draining_QuiescingIsTerminal: a Shutdown acked mid-warm-up must not be undone by
// a restore that completes in the ack's grace window.
func TestLifecycle_Draining_QuiescingIsTerminal(t *testing.T) {
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

// startDrainingControlServer models a daemon that acked Shutdown: it holds the
// home lock and binds a control server whose lifecycle is quiescing, closes the
// server after linger, then holds the lock for the same again — drainDaemon's
// durable-join tail — before releasing it. The in-process server reports this
// test's own PID, which every wait refuses to watch, so the draining→exited
// proof here is the lock. The returned flag reports the release.
func startDrainingControlServer(t *testing.T, linger time.Duration) *atomic.Bool {
	t.Helper()
	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	lifecycle := readyLifecycle(t)
	lifecycle.markQuiescing()
	closeFn, err := startControlServer(&Manager{lifecycle: lifecycle}, nil, nil, nil)
	if err != nil {
		lock.release()
		t.Fatalf("startControlServer: %v", err)
	}
	var released atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(linger)
		_ = closeFn()
		time.Sleep(linger)
		released.Store(true)
		lock.release()
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(testSpawnReadyTimeout):
			t.Errorf("draining control server never closed")
		}
	})
	return &released
}

// TestEnsureDaemon_Draining_SpawnsOnExit: a responder reporting
// DaemonPhaseQuiescing is leaving, not serving. EnsureDaemon must wait it out
// and spawn, not return nil against it. Pre-#5007 the ping succeeded and
// EnsureDaemon returned with zero spawns, leaving no daemon once it exited.
func TestEnsureDaemon_Draining_SpawnsOnExit(t *testing.T) {
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

// TestWaitForDaemonReady_Draining_NotReady: readiness means a daemon that
// will stay; a quiescing responder is not one, and the expiry says why.
func TestWaitForDaemonReady_Draining_NotReady(t *testing.T) {
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

// TestDaemonAlreadyServing_ServingCountsDrainingWaits covers RunDaemon's startup
// guard: a live responder counts as already serving; a quiescing one does not,
// and the guard returns only once it has gone.
func TestDaemonAlreadyServing_ServingCountsDrainingWaits(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	closeLive, err := startControlServer(&Manager{lifecycle: readyLifecycle(t)}, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	if !daemonAlreadyServing() {
		t.Fatalf("a ready responder must count as already serving")
	}
	_ = closeLive()

	released := startDrainingControlServer(t, 300*time.Millisecond)
	if daemonAlreadyServing() {
		t.Fatalf("a quiescing responder must not count as already serving (#5007)")
	}
	if !released.Load() {
		t.Fatalf("daemonAlreadyServing returned while the draining daemon still held the home lock")
	}
}

// TestWaitForDaemonExit_DrainingToExited_PIDDiesOrBound: the best-effort wait returns once the process
// exits, and at the bound when it does not — it never hangs.
func TestWaitForDaemonExit_DrainingToExited_PIDDiesOrBound(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv classification needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pid, _ := startFakeAFDaemon(t, home, "sleep 0.4; exit 0")
	if !waitForDaemonExit(pid, true, time.Now().Add(testSpawnReadyTimeout)) {
		t.Fatalf("waitForDaemonExit reported pid %d still there after it exited", pid)
	}
	if pidLooksAlive(pid) {
		t.Fatalf("waitForDaemonExit returned while pid %d was still alive", pid)
	}

	wedged, _ := startFakeAFDaemon(t, home, "sleep 60; :")
	start := time.Now()
	if waitForDaemonExit(wedged, true, time.Now().Add(200*time.Millisecond)) {
		t.Fatalf("waitForDaemonExit reported a still-running pid %d as gone", wedged)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waitForDaemonExit overran its bound: %s", elapsed)
	}
	if !pidLooksAlive(wedged) {
		t.Fatalf("waitForDaemonExit must never signal the process it waits on")
	}
}

// TestWaitForShutdownCompletion_DrainingToExited_RenamedBinaryPIDTrusted: a PID the daemon
// reported for itself names the stopped daemon whatever its binary is called.
// The wait must not discard it because the basename is not `af`: falling back
// to socket polling reopens #5007, since the socket can vanish while teardown
// still holds the per-home lock and the replacement then fails to start.
func TestWaitForShutdownCompletion_DrainingToExited_RenamedBinaryPIDTrusted(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv rewrite needs /proc to observe")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pid, exited := startFakeDaemonNamed(t, home, "af-renamed", "sleep 0.5; exit 0")
	if isAgentFactoryDaemon(pid) {
		t.Fatalf("fixture invalid: a renamed binary must not pass the basename heuristic")
	}

	if err := WaitForShutdownCompletion(ShutdownPID{PID: pid, Confirmed: true}); err != nil {
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

// TestWaitForDaemonExit_DrainingToExited_RenamedBinaryPIDTrusted: PingResponse.PID is
// always self-reported, so the drain wait trusts it under any basename too.
func TestWaitForDaemonExit_DrainingToExited_RenamedBinaryPIDTrusted(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv rewrite needs /proc to observe")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pid, _ := startFakeDaemonNamed(t, home, "af-renamed", "sleep 0.4; exit 0")
	waitForDaemonExit(pid, true, time.Now().Add(testSpawnReadyTimeout))
	if pidLooksAlive(pid) {
		t.Fatalf("waitForDaemonExit returned while renamed daemon pid %d was still alive", pid)
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
	ackPID    int // 0 is the pre-field ack shape
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
	resp.OK = true
	resp.PID = c.ackPID
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

// TestRequestShutdown_PIDOrder_PrePIDDaemonUsesPingPID: a daemon predating
// ShutdownResponse.PID acks with PID 0. When it is a renamed install, the PID
// file fallback cannot name it (the basename check rejects `af-renamed`), so
// without the Ping self-report RequestShutdown returned 0 and the respawn fell
// back to the socket race #5007 closes. The Ping PID — captured before the
// Shutdown, while the daemon is provably up — must be returned.
func TestRequestShutdown_PIDOrder_PrePIDDaemonUsesPingPID(t *testing.T) {
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
	// Named for reporting, but advisory: Ping and Shutdown are separate
	// connections, so the Ping PID is not proof it was the acker.
	if want := (ShutdownPID{PID: fakePID}); result != ShutdownViaRPC || pid != want {
		t.Fatalf("RequestShutdown = (%v, %+v), want (ShutdownViaRPC, %+v) — the Ping self-report names a pre-PID daemon, unconfirmed", result, pid, want)
	}
}

// TestRequestShutdown_PIDOrder_VerifiedPIDFileWhenPingFails: the Ping capture
// adds a source, it does not replace one. A failed Ping leaves the verified
// PID-file fallback exactly as it was.
func TestRequestShutdown_PIDOrder_VerifiedPIDFileWhenPingFails(t *testing.T) {
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
	if want := (ShutdownPID{PID: fakePID}); result != ShutdownViaRPC || pid != want {
		t.Fatalf("RequestShutdown = (%v, %+v), want (ShutdownViaRPC, %+v) from the verified PID file, unconfirmed", result, pid, want)
	}
}

// TestEnsureDaemon_Draining_WaitsThenReportsStillDraining: a responder still reporting
// DaemonPhaseQuiescing at the drain-wait deadline is finishing durable work
// (#3721). EnsureDaemon must report ErrDaemonStillDraining and leave it alone —
// not fall through to its launch path, whose stale-daemon stop SIGTERMs the PID
// file's daemon and escalates to SIGKILL. The PID file here names the draining
// fake, so the old fall-through killed it.
func TestEnsureDaemon_Draining_WaitsThenReportsStillDraining(t *testing.T) {
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

// TestDrainDaemon_Draining_UnlinksPIDFileAtTeardownStart: drainDaemon's joins can outlast any
// bound with the control socket already closed, and for that tail EnsureDaemon's
// stale-daemon stop reads daemon.pid and SIGTERMs, then SIGKILLs, whatever it
// names. The file must therefore be gone as soon as teardown begins — before
// the control plane closes, let alone the joins — not at process exit.
func TestDrainDaemon_Draining_UnlinksPIDFileAtTeardownStart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	writeTestPIDFile(t, 999999)
	path, err := daemonPIDFilePath()
	if err != nil {
		t.Fatalf("daemonPIDFilePath: %v", err)
	}

	pidFileAtControlClose := true
	closeControl := func() error {
		_, statErr := os.Stat(path)
		pidFileAtControlClose = statErr == nil
		return nil
	}
	manager := &Manager{lifecycle: readyLifecycle(t)}
	stopCh := make(chan struct{})
	var workers sync.WaitGroup
	httpClosed, controlClosed := false, false
	drainDaemon(manager, nil, closeControl, &httpClosed, &controlClosed, stopCh, &workers)

	if pidFileAtControlClose {
		t.Fatalf("daemon.pid still present when the control plane closed; it must be unlinked when teardown begins")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("daemon.pid after drainDaemon: stat err = %v, want not-exist", err)
	}
}

// scriptShutdownWaitBoundary shrinks the wait to a grace of a few polls and
// returns the instant the scripted daemon leaves: the grace's end, measured from
// just before the wait starts. The wait's own deadline is taken a moment later,
// so every in-bounds probe sees the daemon still there; only a probe made after
// the loop's last sleep can see it gone.
func scriptShutdownWaitBoundary(t *testing.T) time.Time {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevGrace, prevPoll := shutdownCompleteGrace, shutdownCompletePoll
	prevAlive, prevLock := shutdownWaitPIDAliveFn, shutdownWaitHomeLockFn
	t.Cleanup(func() {
		shutdownCompleteGrace, shutdownCompletePoll = prevGrace, prevPoll
		shutdownWaitPIDAliveFn, shutdownWaitHomeLockFn = prevAlive, prevLock
	})
	shutdownCompleteGrace = 120 * time.Millisecond
	shutdownCompletePoll = 40 * time.Millisecond
	return time.Now().Add(shutdownCompleteGrace)
}

// TestWaitForShutdownCompletion_DrainingToExited_PIDBoundaryRecheck: a daemon that exits
// during the loop's last sleep wakes the wait past its deadline. Classifying
// that as ErrShutdownIncomplete would suppress the respawn and report a live
// daemon that is already gone; the post-loop recheck must observe the exit.
func TestWaitForShutdownCompletion_DrainingToExited_PIDBoundaryRecheck(t *testing.T) {
	leaves := scriptShutdownWaitBoundary(t)
	probes := 0
	shutdownWaitPIDAliveFn = func(int) bool {
		probes++
		return time.Now().Before(leaves)
	}

	if err := WaitForShutdownCompletion(ShutdownPID{PID: 4242, Confirmed: true}); err != nil {
		t.Fatalf("WaitForShutdownCompletion = %v, want nil: the daemon exited during the final poll", err)
	}
	if probes < 2 {
		t.Fatalf("liveness probes = %d; the script never exercised the in-bounds loop", probes)
	}
}

// TestWaitForShutdownCompletion_DrainingToExited_LockBoundaryRecheck: the PID-less
// loop has the same boundary — a home lock released during the last sleep.
func TestWaitForShutdownCompletion_DrainingToExited_LockBoundaryRecheck(t *testing.T) {
	leaves := scriptShutdownWaitBoundary(t)
	probes := 0
	shutdownWaitHomeLockFn = func(string) daemonState {
		probes++
		if time.Now().Before(leaves) {
			return daemonDraining // lock still held
		}
		return daemonExited
	}

	if err := WaitForShutdownCompletion(ShutdownPID{}); err != nil {
		t.Fatalf("WaitForShutdownCompletion = %v, want nil: the lock was released during the final poll", err)
	}
	if probes < 2 {
		t.Fatalf("lock probes = %d; the script never exercised the in-bounds loop", probes)
	}
}

// TestWaitForShutdownCompletion_Unknown_UnprovableLockIsNotExit: a lock probe that
// cannot answer is not proof of exit, however long it goes on — the wait ends
// in ErrShutdownIncomplete, never in a respawn beside a daemon it could not see
// leave.
func TestWaitForShutdownCompletion_Unknown_UnprovableLockIsNotExit(t *testing.T) {
	scriptShutdownWaitBoundary(t)
	shutdownWaitHomeLockFn = func(string) daemonState {
		return daemonUnknown // e.g. flock: input/output error
	}

	if err := WaitForShutdownCompletion(ShutdownPID{}); !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("WaitForShutdownCompletion = %v, want ErrShutdownIncomplete while the lock is unprovable", err)
	}
}

// TestWaitForShutdownCompletion_Draining_LockReleaseNotSocketQuiet (#5007 finding A): with
// no trustworthy PID the wait must not treat a quiet control socket as exit —
// drainDaemon closes the socket BEFORE its durable joins, while the process
// still holds the home lock. Here there is no socket at all and a real flock is
// held, then released: the wait must return only after the release. The old
// socket-quiet wait returned on its first ping.
func TestWaitForShutdownCompletion_Draining_LockReleaseNotSocketQuiet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	var released atomic.Bool
	go func() {
		time.Sleep(300 * time.Millisecond)
		released.Store(true)
		lock.release()
	}()

	if err := WaitForShutdownCompletion(ShutdownPID{}); err != nil {
		t.Fatalf("WaitForShutdownCompletion(ShutdownPID{}): %v", err)
	}
	if !released.Load() {
		t.Fatalf("WaitForShutdownCompletion(ShutdownPID{}) returned while the home lock was still held")
	}
}

// TestRecoveryStop_Draining_ConfirmsOnLockRelease (#5007 finding B):
// drainDaemon unlinks daemon.pid and closes the socket when teardown begins, so
// mid-drain the recovery actor's StopDaemon finds nothing to stop and the socket
// is already quiet. StopConfirmed must still wait for the draining daemon to
// release its home lock — confirming earlier lets the supervisor start a
// candidate that loses the singleton lock.
func TestRecoveryStop_Draining_ConfirmsOnLockRelease(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)
	stopDaemonFn = func() (bool, error) { return false, nil } // no pid file: nothing to stop
	waitForShutdownFn = WaitForShutdownCompletion

	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	var released atomic.Bool
	go func() {
		time.Sleep(300 * time.Millisecond)
		released.Store(true)
		lock.release()
	}()

	outcome, err := stopDaemonForRecovery(journal, "previous")
	if err != nil {
		t.Fatalf("stopDaemonForRecovery: %v", err)
	}
	if outcome != upgradetxn.StopConfirmed {
		t.Fatalf("outcome = %v, want StopConfirmed once the lock is released", outcome)
	}
	if !released.Load() {
		t.Fatalf("StopConfirmed while the draining daemon still held the home lock")
	}
}

// TestRecoveryStop_Draining_StillRunning: a drain that
// outlives the bound is StopStillRunning, never a fabricated confirmation.
func TestRecoveryStop_Draining_StillRunning(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)
	stopDaemonFn = func() (bool, error) { return false, nil }
	waitForShutdownFn = WaitForShutdownCompletion
	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 200 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })

	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	t.Cleanup(lock.release)

	outcome, err := stopDaemonForRecovery(journal, "previous")
	if err != nil {
		t.Fatalf("stopDaemonForRecovery: %v", err)
	}
	if outcome != upgradetxn.StopStillRunning {
		t.Fatalf("outcome = %v, want StopStillRunning while the home lock is held", outcome)
	}
}

// TestEnsureDaemon_Draining_LockHeldWithoutAnswerReportsStillDraining: past its
// socket close a draining daemon answers nothing and has unlinked daemon.pid,
// but still holds the home lock. EnsureDaemon must classify that as draining
// and report ErrDaemonStillDraining at the bound — not spawn a child that can
// only lose the lock and exit. The old ping-only check went straight to spawn.
func TestEnsureDaemon_Draining_LockHeldWithoutAnswerReportsStillDraining(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	t.Cleanup(lock.release)

	launches := 0
	launch := func() error {
		launches++
		return errors.New("test launcher: must not be reached")
	}
	err = ensureDaemonWithLauncherUntil(launch, time.Now().Add(1500*time.Millisecond))
	if !errors.Is(err, ErrDaemonStillDraining) {
		t.Fatalf("ensure against a lock-holding drainer = %v, want ErrDaemonStillDraining", err)
	}
	if launches != 0 {
		t.Fatalf("launches = %d, want 0 while the home lock is held", launches)
	}
}

// TestEnsureDaemon_Exited_Spawns: no answer, no daemon.pid, and a home lock that
// is takeable — the previous daemon has exited — so EnsureDaemon spawns.
func TestEnsureDaemon_Exited_Spawns(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	lock, err := acquireHomeLock() // leaves daemon.lock on disk, as a past daemon would
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	lock.release()

	spawns := 0
	var newDaemonClose func() error
	launch := func() error {
		spawns++
		var bindErr error
		newDaemonClose, bindErr = startControlServer(nil, nil, nil, nil)
		return bindErr
	}
	t.Cleanup(func() {
		if newDaemonClose != nil {
			_ = newDaemonClose()
		}
	})
	if err := ensureDaemonWithLauncherUntil(launch, time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("ensure with the previous daemon exited: %v", err)
	}
	if spawns != 1 {
		t.Fatalf("spawns = %d, want 1", spawns)
	}
}

// deadPID returns the PID of a process that has already exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

// livePID returns a PID that stays alive until the test ends.
func livePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// writePIDFileFor drops a daemon.pid naming pid into the test home, as a
// daemon predating drainDaemon's early unlink leaves behind mid-drain (#5007).
func writePIDFileFor(t *testing.T, pid int) {
	t.Helper()
	path, err := daemonPIDFilePath()
	if err != nil {
		t.Fatalf("daemonPIDFilePath: %v", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0600); err != nil {
		t.Fatalf("write daemon.pid: %v", err)
	}
}

// TestEnsureDaemon_Draining_PIDFileAndHeldLockNeverKilled (#5007 addendum 4):
// a daemon predating the early unlink keeps daemon.pid through its drain, so
// mid-drain the socket is dead but the pidfile still names a live process AND
// the lock stays held. EnsureDaemon must read that as draining — report
// ErrDaemonStillDraining at the bound and never reach the launch path, whose
// stale-daemon stop would SIGKILL the drainer. Its one SIGTERM reclaim is
// absorbed the way a real drainer's is (the fake's trap drops it like an
// unread sigChan), so the process surviving to the end proves the SIGKILL
// escalation never ran.
func TestEnsureDaemon_Draining_PIDFileAndHeldLockNeverKilled(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv classification needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	t.Cleanup(lock.release)
	pid, _ := startFakeAFDaemon(t, home, "trap '' TERM; sleep 60")
	writePIDFileFor(t, pid)

	launches := 0
	launch := func() error {
		launches++
		return errors.New("test launcher: must not be reached")
	}
	err = ensureDaemonWithLauncherUntil(launch, time.Now().Add(1500*time.Millisecond))
	if !errors.Is(err, ErrDaemonStillDraining) {
		t.Fatalf("ensure against a pidfile-keeping drainer = %v, want ErrDaemonStillDraining", err)
	}
	if launches != 0 {
		t.Fatalf("launches = %d, want 0 while a live daemon.pid PID names the drainer", launches)
	}
	if !pidLooksAlive(pid) {
		t.Fatalf("draining daemon pid %d died during the wait; the reclaim must never escalate to SIGKILL", pid)
	}
}

// TestEnsureDaemon_Draining_LivePIDFileWithoutLock: a drainer predating the
// home lock never takes daemon.lock and keeps daemon.pid until exit — dead
// socket, absent lock, live pidfile PID. The pid must still read draining:
// absent lock alone would spawn beside it.
func TestEnsureDaemon_Draining_LivePIDFileWithoutLock(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("fake-daemon argv classification needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	pid, _ := startFakeAFDaemon(t, home, "trap '' TERM; sleep 60")
	writePIDFileFor(t, pid)

	launches := 0
	launch := func() error {
		launches++
		return errors.New("test launcher: must not be reached")
	}
	err := ensureDaemonWithLauncherUntil(launch, time.Now().Add(1500*time.Millisecond))
	if !errors.Is(err, ErrDaemonStillDraining) {
		t.Fatalf("ensure against a pre-lock drainer = %v, want ErrDaemonStillDraining", err)
	}
	if launches != 0 {
		t.Fatalf("launches = %d, want 0 while a live daemon.pid PID names the drainer", launches)
	}
	if !pidLooksAlive(pid) {
		t.Fatalf("draining daemon pid %d died during the wait; the reclaim must never escalate to SIGKILL", pid)
	}
}

// TestWaitForShutdownCompletion_Draining_LivePIDFileKeepsWaiting (#5007
// addendum 4): quiet socket, takeable leftover lock, but daemon.pid still
// names a live process — a drainer that predates the early unlink. The wait
// must not read exit until that PID is gone.
func TestWaitForShutdownCompletion_Draining_LivePIDFileKeepsWaiting(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	lock, err := acquireHomeLock() // leftover daemon.lock, as a past daemon leaves
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	lock.release()
	writePIDFileFor(t, livePID(t))

	go func() {
		time.Sleep(300 * time.Millisecond)
		path, _ := daemonPIDFilePath()
		_ = os.Remove(path)
	}()

	if err := WaitForShutdownCompletion(ShutdownPID{}); err != nil {
		t.Fatalf("WaitForShutdownCompletion = %v, want nil once the pidfile drainer is gone", err)
	}
}

// hangingControl answers connects but its Ping never replies, so the client
// read blocks until the conn deadline — the wedged-responder case the bounded
// exit-wait ping exists for (#5007 addendum 4).
type hangingControl struct{}

func (h *hangingControl) Ping(_ PingRequest, _ *PingResponse) error {
	select {} // never replies
}

// TestWaitForShutdownCompletion_Unknown_HungResponderBoundedByDeadline: a
// responder that accepts the unix dial but never replies must cost one bounded
// probe, not the wait — the wait still ends in ErrShutdownIncomplete at the
// grace.
func TestWaitForShutdownCompletion_Unknown_HungResponderBoundedByDeadline(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 800 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })
	startFakeControlService(t, &hangingControl{})
	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	t.Cleanup(lock.release)

	start := time.Now()
	err = WaitForShutdownCompletion(ShutdownPID{})
	if !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("hung responder: WaitForShutdownCompletion = %v, want ErrShutdownIncomplete", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("hung responder stalled the wait %s, past the %s grace", elapsed, shutdownCompleteGrace)
	}
}

// TestRequestShutdown_PIDOrder_AckPIDIsConfirmed: only the PID the acker names
// in its own Shutdown reply is confirmed, even when a Ping reported another.
func TestRequestShutdown_PIDOrder_AckPIDIsConfirmed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	startFakeControlService(t, &preShutdownPIDControl{pingPID: 1111, ackPID: 2222})

	result, pid, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if want := (ShutdownPID{PID: 2222, Confirmed: true}); result != ShutdownViaRPC || pid != want {
		t.Fatalf("RequestShutdown = (%v, %+v), want (ShutdownViaRPC, %+v)", result, pid, want)
	}
}

// TestWaitForShutdownCompletion_Draining_UnconfirmedPIDWaitsOnLock (#5007
// amendment §1): Ping and Shutdown are separate connections, so a pre-ack PID
// can name a daemon that exited and was replaced by the one that acked. Here
// that advisory PID is dead while the acker still holds the home lock: the
// wait must keep waiting on the lock, not report exit on the wrong process's
// death. A confirmed PID, by contrast, is waited on directly.
func TestWaitForShutdownCompletion_Draining_UnconfirmedPIDWaitsOnLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 300 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })
	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	t.Cleanup(lock.release)
	gone := deadPID(t)

	if err := WaitForShutdownCompletion(ShutdownPID{PID: gone}); !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("unconfirmed dead PID with the lock held = %v, want ErrShutdownIncomplete", err)
	}
	if err := WaitForShutdownCompletion(ShutdownPID{PID: gone, Confirmed: true}); err != nil {
		t.Fatalf("confirmed dead PID = %v, want nil: the acker itself has exited", err)
	}
}

// TestWaitForShutdownCompletion_Draining_PreLockAnsweringSocketKeepsWaiting
// (#5007 amendment §2): a daemon predating the home lock acks Shutdown and
// never creates daemon.lock. A missing lock file is not exit — with the socket
// still answering quiescing, it is draining, and the bound reports
// ErrShutdownIncomplete.
func TestWaitForShutdownCompletion_Draining_PreLockAnsweringSocketKeepsWaiting(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 300 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })
	startQuiescingControlServer(t)

	if err := WaitForShutdownCompletion(ShutdownPID{}); !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("pre-lock daemon still answering = %v, want ErrShutdownIncomplete", err)
	}
}

// TestWaitForShutdownCompletion_DrainingToExited_PreLockSocketQuiet: for a
// pre-lock daemon the control socket going quiet is the only exit proxy left,
// so the wait returns once it stops answering, and not before.
func TestWaitForShutdownCompletion_DrainingToExited_PreLockSocketQuiet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	closeFn := startQuiescingControlServer(t)
	var closed atomic.Bool
	go func() {
		time.Sleep(300 * time.Millisecond)
		closed.Store(true)
		_ = closeFn()
	}()

	if err := WaitForShutdownCompletion(ShutdownPID{}); err != nil {
		t.Fatalf("pre-lock daemon whose socket went quiet = %v, want nil", err)
	}
	if !closed.Load() {
		t.Fatalf("the wait returned while the pre-lock daemon's socket still answered")
	}
}

// startQuiescingControlServer binds a control server that answers as a daemon
// that has acked Shutdown — DaemonPhaseQuiescing — WITHOUT taking the home
// lock: the pre-lock vintage. The returned close is idempotent.
func startQuiescingControlServer(t *testing.T) func() error {
	t.Helper()
	lifecycle := readyLifecycle(t)
	lifecycle.markQuiescing()
	closeFn, err := startControlServer(&Manager{lifecycle: lifecycle}, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	return closeFn
}

// serveAsResponder binds a control server that answers serving, as a daemon
// that has not acked Shutdown does. It is in-process, so its Ping reports this
// test's own PID — the responder PID the cells below compare targets against.
func serveAsResponder(t *testing.T) func() error {
	t.Helper()
	closeFn, err := startControlServer(&Manager{lifecycle: readyLifecycle(t)}, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	return closeFn
}

// staleLockFile leaves daemon.lock on disk and free, as a lock-era daemon that
// ran in this home earlier does: the file persists and nothing unlinks it.
func staleLockFile(t *testing.T) {
	t.Helper()
	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	lock.release()
}

// shortShutdownGrace shrinks the post-ack bound for cells that expect it to run out.
func shortShutdownGrace(t *testing.T) {
	t.Helper()
	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 300 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })
}

// TestWaitForShutdownCompletion_ServingAnswer_ExitOnlyForDifferentPID (#5007
// addendum 2): in the post-ack wait a serving answer proves exit only from a
// provably different process — responder PID known and unequal to a known
// advisory target. The target itself answers serving while it predates
// quiescing-at-ack (its whole ack grace), and a daemon never asked to stop
// answers serving indefinitely; reading either as exited respawns beside a live
// daemon. Pinned for every lock reading Ping is consulted on: held, absent (a
// pre-lock daemon), and takeable — a stale file a pre-lock daemon never flocks
// (addendum 3).
func TestWaitForShutdownCompletion_ServingAnswer_ExitOnlyForDifferentPID(t *testing.T) {
	for _, lockMode := range []string{"held", "absent", "stale-takeable"} {
		for _, tc := range []struct {
			name       string
			target     func(t *testing.T) int
			wantExited bool
		}{
			{name: "different responder PID is exited", target: deadPID, wantExited: true},
			{name: "responder is the target: keeps waiting", target: func(*testing.T) int { return os.Getpid() }},
			{name: "target unknown: keeps waiting", target: func(*testing.T) int { return 0 }},
		} {
			t.Run(lockMode+"/"+tc.name, func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
				shortShutdownGrace(t)
				switch lockMode {
				case "held":
					lock, err := acquireHomeLock()
					if err != nil {
						t.Fatalf("acquireHomeLock: %v", err)
					}
					t.Cleanup(lock.release)
				case "stale-takeable":
					staleLockFile(t)
				}
				serveAsResponder(t)

				err := WaitForShutdownCompletion(ShutdownPID{PID: tc.target(t)})
				switch {
				case tc.wantExited && err != nil:
					t.Fatalf("WaitForShutdownCompletion = %v, want nil: a different process serves", err)
				case !tc.wantExited && !errors.Is(err, ErrShutdownIncomplete):
					t.Fatalf("WaitForShutdownCompletion = %v, want ErrShutdownIncomplete: the responder may be the target", err)
				}
			})
		}
	}
}

// TestWaitForShutdownCompletion_Draining_TargetServingInAckGraceWaitsForLock:
// a pre-quiescing acker answers serving under its own PID through its ack
// grace. The wait keeps going on that answer and is resolved by what does
// prove exit — the daemon leaving: its socket closes and its lock is released.
func TestWaitForShutdownCompletion_Draining_TargetServingInAckGraceWaitsForLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	closeFn := serveAsResponder(t)
	var released atomic.Bool
	go func() {
		time.Sleep(300 * time.Millisecond)
		released.Store(true)
		_ = closeFn()
		lock.release()
	}()

	if err := WaitForShutdownCompletion(ShutdownPID{PID: os.Getpid()}); err != nil {
		t.Fatalf("WaitForShutdownCompletion: %v", err)
	}
	if !released.Load() {
		t.Fatalf("the wait read the target's own serving answer as exit before the lock was released")
	}
}

// TestEnsureDaemon_Draining_StarterAnswersServingMidWait (#5007 addendum): a
// new daemon holds the home lock from before it binds its socket or writes
// daemon.pid, so in that window EnsureDaemon sees a held lock and no answer —
// indistinguishable from a drainer — and waits. A drainer's socket never
// re-opens, so a serving answer inside that wait can only be the new holder:
// the state-probe poll must end early and EnsureDaemon return nil without
// spawning (addendum 2 keeps this cell off the post-ack wait's proof). A
// lock-only wait stalled until the drain deadline.
func TestEnsureDaemon_Draining_StarterAnswersServingMidWait(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	lock, err := acquireHomeLock() // the starter's lock, held for its whole life
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	t.Cleanup(lock.release)
	bound := make(chan func() error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		closeFn, err := startControlServer(&Manager{lifecycle: readyLifecycle(t)}, nil, nil, nil)
		if err != nil {
			t.Errorf("starter bind: %v", err)
			bound <- func() error { return nil }
			return
		}
		bound <- closeFn
	}()
	t.Cleanup(func() { _ = (<-bound)() })

	launches := 0
	launch := func() error {
		launches++
		return errors.New("test launcher: must not be reached")
	}
	start := time.Now()
	if err := ensureDaemonWithLauncherUntil(launch, time.Now().Add(10*time.Second)); err != nil {
		t.Fatalf("ensure against a starter that comes up serving: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ensure took %s; a serving answer must end the drain wait, not the ~9.5s deadline", elapsed)
	}
	if launches != 0 {
		t.Fatalf("launches = %d, want 0: the starter serves this home", launches)
	}
}

// TestDaemonAlreadyServing_Draining_StarterAnswersServingCounts: RunDaemon's
// guard sees the same lock-then-bind window. When the wait ends because a new
// daemon answers serving, that daemon already serves this home — the guard must
// say so (a clean exit) rather than proceed to the lock it cannot take.
func TestDaemonAlreadyServing_Draining_StarterAnswersServingCounts(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	t.Cleanup(lock.release)
	bound := make(chan func() error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		closeFn, err := startControlServer(&Manager{lifecycle: readyLifecycle(t)}, nil, nil, nil)
		if err != nil {
			t.Errorf("starter bind: %v", err)
			bound <- func() error { return nil }
			return
		}
		bound <- closeFn
	}()
	t.Cleanup(func() { _ = (<-bound)() })

	if !daemonAlreadyServing() {
		t.Fatalf("daemonAlreadyServing = false, want true once the starter answers serving")
	}
}

// TestWaitForShutdownCompletion_StaleLockFile_PreLockDaemonAnswerDecides (#5007
// addendum 3): daemon.lock persists once created, and a daemon predating the
// lock never flocks it, so on a home that once ran a lock-era daemon the file
// can read takeable while a pre-lock daemon still drains. A takeable lock is
// therefore exit only once Ping rules out a responder: a quiescing answer is
// draining, a quiet socket is exited.
func TestWaitForShutdownCompletion_StaleLockFile_PreLockDaemonAnswerDecides(t *testing.T) {
	t.Run("quiescing answer keeps waiting", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		shortShutdownGrace(t)
		staleLockFile(t)
		startQuiescingControlServer(t)

		if err := WaitForShutdownCompletion(ShutdownPID{}); !errors.Is(err, ErrShutdownIncomplete) {
			t.Fatalf("takeable lock beside a quiescing pre-lock daemon = %v, want ErrShutdownIncomplete", err)
		}
	})
	t.Run("quiet socket is exited", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		staleLockFile(t)

		if err := WaitForShutdownCompletion(ShutdownPID{}); err != nil {
			t.Fatalf("takeable lock, nothing answering = %v, want nil", err)
		}
	})
}
