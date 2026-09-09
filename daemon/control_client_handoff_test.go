package daemon

import (
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
)

// Tests for the #2212 R2b upgrade hand-off dead window in callDaemon's retry
// loop. During a self-upgrade, a quiescing daemon answers the first mutation
// with errDaemonQuiescing and then exits, unlinking the control socket before
// the candidate binds. The next callDaemonNoEnsure re-dial lands in that gap
// and returns a bare dial error (ECONNREFUSED / fs.ErrNotExist) that
// IsDaemonAdmissionRetryable does not classify as retryable. callDaemon now
// treats that absent dial as retryable ONLY after it has already observed a
// quiescing admission (seenQuiescing), and re-runs EnsureDaemon on it so the
// gate can wait for the candidate to bind — riding the dead window to the new
// daemon instead of surfacing the bare dial error. These tests exercise the
// real callDaemon loop over a socket-lifecycle transition (up -> quiescing ->
// gone -> candidate bound) driven by fake Control RPC servers.

// startFakeControlListenerWithSrv resolves the daemon control socket, ensures
// its parent dir exists, removes any stale socket, binds, and serves srv on it.
// Returns the listener so a caller can close it to simulate the daemon exiting.
func startFakeControlListenerWithSrv(t *testing.T, srv *rpc.Server) (net.Listener, error) {
	t.Helper()
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go srv.ServeConn(conn)
		}
	}()
	return listener, nil
}

// startFakeControlListener binds the daemon control socket (resolved from
// AGENT_FACTORY_HOME) and serves srv on it, returning the listener so the
// caller can close it to simulate the daemon exiting and freeing the socket.
// Mirrors the low-level pattern in startPreShutdownFakeDaemon, with an
// arbitrary registered Control service.
func startFakeControlListener(t *testing.T, srv *rpc.Server) (net.Listener, func()) {
	t.Helper()
	listener, err := startFakeControlListenerWithSrv(t, srv)
	if err != nil {
		t.Fatalf("startFakeControlListener: %v", err)
	}
	socketPath, _ := DaemonSocketPath()
	cleanup := func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
	return listener, cleanup
}

// quiescingHandoffControl simulates the OLD side of a #2212 R2b upgrade
// hand-off: Ping always succeeds (so EnsureDaemon sees a live daemon and does
// not spawn), and every mutation is refused with errDaemonQuiescing. The first
// quiescing refusal triggers closeOld, which closes the listener and unlinks
// the socket — reproducing the quiescing daemon exiting and freeing the socket
// before the candidate binds (the hand-off dead window).
type quiescingHandoffControl struct {
	mu       sync.Mutex
	closed   bool
	closeOld func()
	quiesced *atomic.Bool
}

func (c *quiescingHandoffControl) Ping(_ PingRequest, _ *PingResponse) error {
	return nil
}

func (c *quiescingHandoffControl) QuiescingMutation(_ struct{}, _ *struct{}) error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		if c.quiesced != nil {
			c.quiesced.Store(true)
		}
		if c.closeOld != nil {
			c.closeOld()
		}
	}
	c.mu.Unlock()
	return errDaemonQuiescing()
}

// candidateAcceptControl simulates the upgrade candidate that wins the socket
// after the hand-off: Ping succeeds (so the re-run EnsureDaemon's readiness
// poll returns nil), and the mutation is admitted (so the retrying callDaemon
// reaches the new daemon and succeeds).
type candidateAcceptControl struct{}

func (candidateAcceptControl) Ping(_ PingRequest, _ *PingResponse) error { return nil }
func (candidateAcceptControl) QuiescingMutation(_ struct{}, _ *struct{}) error {
	return nil
}

// TestCallDaemon_RidesHandoffDeadWindowToCandidate is the regression test for
// the bare-dial-error bug. The pre-fix loop terminates on the first re-dial
// that hits the freed socket (the absent error is not admission-retryable) and
// surfaces a bare dial error instead of reaching the candidate — so this test
// FAILS before the fix (callDaemon returns the dial error, not nil). After the
// fix the loop treats that absent dial as retryable once it has seen a
// quiescing admission, re-runs EnsureDaemon to let the candidate bind, and
// reaches the new daemon.
func TestCallDaemon_RidesHandoffDeadWindowToCandidate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)

	var quiesced atomic.Bool

	oldSrv := rpc.NewServer()
	oldCtrl := &quiescingHandoffControl{quiesced: &quiesced}
	if err := oldSrv.RegisterName(controlServiceName, oldCtrl); err != nil {
		t.Fatalf("register old Control: %v", err)
	}
	oldListener, oldCleanup := startFakeControlListener(t, oldSrv)
	t.Cleanup(oldCleanup)
	oldCtrl.closeOld = func() {
		_ = oldListener.Close()
		if socketPath, err := DaemonSocketPath(); err == nil {
			_ = os.Remove(socketPath)
		}
	}

	// The candidate is bound lazily inside EnsureDaemon's launch path — the
	// way a real upgrade candidate comes up once the gate releases. The
	// production EnsureDaemon reaches this only on the dead-window re-run the
	// fix adds; the initial EnsureDaemon pings the still-alive old daemon and
	// returns nil without launching.
	var (
		candidateMu       sync.Mutex
		candidateListener net.Listener
	)
	bindCandidate := func() error {
		candidateMu.Lock()
		defer candidateMu.Unlock()
		if candidateListener != nil {
			return nil
		}
		socketPath, err := DaemonSocketPath()
		if err != nil {
			return err
		}
		_ = os.Remove(socketPath)
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			return err
		}
		srv := rpc.NewServer()
		if err := srv.RegisterName(controlServiceName, candidateAcceptControl{}); err != nil {
			_ = ln.Close()
			return err
		}
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go srv.ServeConn(conn)
			}
		}()
		candidateListener = ln
		return nil
	}
	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = bindCandidate
	t.Cleanup(func() {
		launchDaemonProcessFn = prevLaunch
		candidateMu.Lock()
		ln := candidateListener
		candidateMu.Unlock()
		if ln != nil {
			_ = ln.Close()
			if socketPath, err := DaemonSocketPath(); err == nil {
				_ = os.Remove(socketPath)
			}
		}
	})

	start := time.Now()
	err := callDaemon("QuiescingMutation", struct{}{}, &struct{}{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("callDaemon must ride the hand-off dead window to the candidate and succeed; got: %v", err)
	}
	if !quiesced.Load() {
		t.Fatalf("the old daemon never returned a quiescing admission; the hand-off scenario was not exercised")
	}
	candidateMu.Lock()
	gotCandidate := candidateListener != nil
	candidateMu.Unlock()
	if !gotCandidate {
		t.Fatalf("the candidate never bound; EnsureDaemon was not re-run on the absent dial error")
	}
	// The candidate binds within the first poll after the dead window opens,
	// well under the 5s retry budget. This guards against a hang, not against
	// the pre-fix bail (which is also fast and is caught by the err != nil
	// assertion above).
	if elapsed > daemonAdmissionRetryWait {
		t.Fatalf("callDaemon took %s, exceeding the %s retry budget", elapsed, daemonAdmissionRetryWait)
	}
}

// startingThenGoneControl serves Ping so the cold EnsureDaemon succeeds, then
// returns errDaemonStarting on the first mutation and closes the listener — so
// the retry loop's next re-dial hits the freed socket. The refusal is a
// retryable admission that is NOT erorDaemonQuiescing, so callDaemon must NOT
// set seenQuiescing and must therefore bail on the absent dial rather than
// treating it as a hand-off to ride out.
type startingThenGoneControl struct {
	mu        sync.Mutex
	closed    bool
	closeOnce func()
}

func (c *startingThenGoneControl) Ping(_ PingRequest, _ *PingResponse) error { return nil }

func (c *startingThenGoneControl) StartingMutation(_ struct{}, _ *struct{}) error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		if c.closeOnce != nil {
			c.closeOnce()
		}
	}
	c.mu.Unlock()
	return errDaemonStarting()
}

// TestCallDaemon_AbsentDialWithoutQuiescingBailsFast locks in the seenQuiescing
// narrowing: a dial that goes absent without a preceding quiescing admission
// is NOT made retryable, preserving the existing fail-open behavior for
// non-hand-off failures (the #2212 R1 gate proceeds when the journal is
// stale/corrupt/absent, and a genuine no-daemon dial must not spin the retry
// budget). If a future change folded isDaemonAbsentErr into
// IsDaemonAdmissionRetryable unconditionally, this loop would ride the full
// daemonAdmissionRetryWait instead of bailing on the first absent re-dial, and
// this test would catch it.
func TestCallDaemon_AbsentDialWithoutQuiescingBailsFast(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)

	// Defense in depth: EnsureDaemon's launch path must not spawn a real
	// daemon. The initial EnsureDaemon succeeds via its ping against the fake
	// server, and the seenQuiescing guard keeps the loop from re-running
	// EnsureDaemon on the absent dial, so this stub does not fire in the
	// fixed code.
	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return errors.New("test: must not launch a real daemon") }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	ctrl := &startingThenGoneControl{}
	srv := rpc.NewServer()
	if err := srv.RegisterName(controlServiceName, ctrl); err != nil {
		t.Fatalf("register Control: %v", err)
	}
	listener, cleanup := startFakeControlListener(t, srv)
	t.Cleanup(cleanup)
	ctrl.closeOnce = func() {
		_ = listener.Close()
		if socketPath, err := DaemonSocketPath(); err == nil {
			_ = os.Remove(socketPath)
		}
	}

	start := time.Now()
	err := callDaemon("StartingMutation", struct{}{}, &struct{}{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("callDaemon must surface the absent dial error, got nil")
	}
	if !isDaemonAbsentErr(err) {
		t.Fatalf("expected a daemon-absent dial error after the socket was freed; got: %v", err)
	}
	// The seenQuiescing guard makes the absent dial non-retryable here, so the
	// loop bails on the first absent re-dial (one ~100ms poll) instead of
	// riding the full daemonAdmissionRetryWait budget. A bail is comfortably
	// under the budget; an accidental ride approaches it and fails.
	if elapsed >= daemonAdmissionRetryWait {
		t.Fatalf("absent dial without a quiescing admission rode the %s retry budget; the seenQuiescing guard is missing", elapsed)
	}
}

// realGateQuiescingControl serves Ping so EnsureDaemon sees a live daemon, and
// returns the REAL quiescing admission (manager.lifecycle.mutationAdmissionError,
// the same gate the production controlServer's requireMutationAdmission calls)
// for AddTask. The first quiescing refusal closes the bound listener — modeling
// the quiescing daemon exiting and freeing the socket before the candidate binds.
// Unlike the stub-based test above, the refusal comes through the real lifecycle
// gate (not errDaemonQuiescing() spelled inline) and the candidate executes the
// real controlServer AddTask handler end to end, making this the in-process
// form of the #2212 R2b hand-off E2E (real admission + real callDaemon loop +
// real socket lifecycle + real candidate handler; only the candidate "spawn" is
// stubbed to bind a pre-built controlServer, as EnsureDaemonFromPath tests do).
type realGateQuiescingControl struct {
	mu       sync.Mutex
	closed   bool
	manager  *Manager
	closeOld func()
}

func (c *realGateQuiescingControl) Ping(_ PingRequest, _ *PingResponse) error { return nil }

func (c *realGateQuiescingControl) AddTask(_ AddTaskRequest, _ *AddTaskResponse) error {
	err := c.manager.lifecycle.mutationAdmissionError()
	c.mu.Lock()
	if !c.closed && IsDaemonQuiescingErr(err) {
		c.closed = true
		if c.closeOld != nil {
			c.closeOld()
		}
	}
	c.mu.Unlock()
	return err
}

// TestCallDaemon_RidesHandoffViaRealAdmissionGate drives the real callDaemon
// loop through a real-admission quiescing refusal, then the dead window (old
// listener closed + socket unlinked), then the re-run EnsureDaemon binding a
// real candidate controlServer whose AddTask handler executes for real and
// persists the task. Pre-fix the loop bails on the absent re-dial and surfaces
// the bare dial error; post-fix it rides the window to the candidate.
func TestCallDaemon_RidesHandoffViaRealAdmissionGate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)

	oldManager, err := newManagerShellForDaemon(config.DefaultConfig(), "")
	if err != nil {
		t.Fatalf("newManagerShellForDaemon: %v", err)
	}
	if err := oldManager.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}
	oldManager.lifecycle.markQuiescing()

	oldSrv := rpc.NewServer()
	oldCtrl := &realGateQuiescingControl{manager: oldManager}
	if err := oldSrv.RegisterName(controlServiceName, oldCtrl); err != nil {
		t.Fatalf("register old Control: %v", err)
	}
	oldListener, oldCleanup := startFakeControlListener(t, oldSrv)
	t.Cleanup(oldCleanup)
	oldCtrl.closeOld = func() {
		_ = oldListener.Close() // closing unlinks the socket (net default)
	}

	// The candidate is bound lazily inside EnsureDaemon's launch path, the way
	// a real candidate comes up once the gate releases. EnsureDaemon reaches
	// this only on the dead-window re-run the fix adds; the initial EnsureDaemon
	// pings the still-live quiescing old daemon and returns nil without spawning.
	var (
		candidateMu       sync.Mutex
		candidateListener net.Listener
		candidateClose    func() error
	)
	bindCandidate := func() error {
		candidateMu.Lock()
		defer candidateMu.Unlock()
		if candidateListener != nil {
			return nil
		}
		candManager, mErr := NewManager(config.DefaultConfig())
		if mErr != nil {
			return mErr
		}
		closeFn, sErr := startControlServer(candManager, newTaskScheduler(), nil, nil)
		if sErr != nil {
			return sErr
		}
		candidateClose = closeFn
		candidateListener = nil // closeFn holds the listener; track existence via candidateClose != nil
		return nil
	}
	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = bindCandidate
	t.Cleanup(func() {
		launchDaemonProcessFn = prevLaunch
		candidateMu.Lock()
		closeFn := candidateClose
		candidateMu.Unlock()
		if closeFn != nil {
			_ = closeFn()
		}
	})

	taskProject := t.TempDir()
	createdTask := task.Task{
		ID:          "e2e00001",
		Name:        "handoff-e2e",
		Prompt:      "run it",
		CronExpr:    "*/15 * * * *",
		ProjectPath: taskProject,
		Enabled:     false,
		CreatedAt:   time.Now(),
	}

	var resp AddTaskResponse
	if err := callDaemon("AddTask", AddTaskRequest{Task: createdTask, Actor: "test"}, &resp); err != nil {
		t.Fatalf("callDaemon must ride the hand-off dead window to the candidate and succeed; got: %v", err)
	}
	if !resp.OK {
		t.Fatalf("candidate AddTask returned OK=false: %+v", resp)
	}

	// The candidate's real handler persisted the task to tasks.json. Loading it
	// back proves callDaemon reached the NEW daemon and the mutation took effect
	// there, not on the quiescing old one (which refused at the admission gate).
	persisted, loadErr := task.LoadTasks()
	if loadErr != nil {
		t.Fatalf("LoadTasks after hand-off: %v", loadErr)
	}
	found := false
	for _, tk := range persisted {
		if tk.ID == createdTask.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("task %s was not persisted by the candidate; tasks on disk: %s", createdTask.ID, tasksDebug(persisted))
	}
	if !oldCtrl.closed {
		t.Fatalf("the old daemon never closed its listener; the hand-off scenario was not exercised")
	}
}

func tasksDebug(tasks []task.Task) string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	return "[" + strings.Join(ids, ", ") + "]"
}
