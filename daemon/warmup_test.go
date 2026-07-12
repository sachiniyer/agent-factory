package daemon

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// The tests in this file cover the #829 startup reorder: the control socket
// binds before the (potentially minutes-long) instance restore, so during the
// warm-up window Ping and Shutdown must work, state-dependent RPCs must fail
// fast with the typed "daemon is starting" error, and EnsureDaemon must treat
// the bound-but-warming daemon as running rather than spawning another.

// TestControlServer_WarmupGatesStateRPCs drives the warm-up window at the
// control-server level: a manager shell with no restore answers Ping, rejects
// every state-dependent RPC with the typed starting error, and acks
// ReloadTasks (RunDaemon reloads tasks.json after the restore anyway). Once
// RestoreInstances completes, the same RPCs pass the gate.
func TestControlServer_WarmupGatesStateRPCs(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := newManagerShell(config.DefaultConfig())
	if err != nil {
		t.Fatalf("newManagerShell: %v", err)
	}
	if manager.Ready() {
		t.Fatalf("manager shell must not report ready before RestoreInstances")
	}
	scheduler := newTaskScheduler()
	closeServer, err := startControlServer(manager, scheduler, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	// Ping must work during warm-up — it is what stops EnsureDaemon callers
	// from spawning duplicate daemons.
	if err := pingDaemon(); err != nil {
		t.Fatalf("Ping during warm-up: %v", err)
	}

	// Every state-dependent RPC must return the typed starting error.
	var createResp CreateSessionResponse
	err = callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title: "warmup", RepoPath: repoPath, Program: "claude",
	}, &createResp)
	if !IsDaemonStartingErr(err) {
		t.Fatalf("CreateSession during warm-up: want daemon-starting error, got: %v", err)
	}
	var killResp KillSessionResponse
	err = callDaemonNoEnsure("KillSession", KillSessionRequest{Title: "warmup"}, &killResp)
	if !IsDaemonStartingErr(err) {
		t.Fatalf("KillSession during warm-up: want daemon-starting error, got: %v", err)
	}
	var promptResp SendPromptResponse
	err = callDaemonNoEnsure("SendPrompt", SendPromptRequest{Title: "warmup", Prompt: "hi"}, &promptResp)
	if !IsDaemonStartingErr(err) {
		t.Fatalf("SendPrompt during warm-up: want daemon-starting error, got: %v", err)
	}

	// ReloadTasks acks during warm-up: the caller's tasks.json write is
	// durable and RunDaemon reloads it right after the restore completes.
	var reloadResp ReloadTasksResponse
	if err := callDaemonNoEnsure("ReloadTasks", ReloadTasksRequest{}, &reloadResp); err != nil {
		t.Fatalf("ReloadTasks during warm-up: %v", err)
	}
	if !reloadResp.OK {
		t.Fatalf("ReloadTasks during warm-up returned OK=false")
	}

	// After the restore the gate opens and the same RPC path works end to end.
	if err := manager.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}
	if !manager.Ready() {
		t.Fatalf("manager must report ready after RestoreInstances")
	}
	if err := callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title: "warmup", RepoPath: repoPath, Program: "claude",
	}, &createResp); err != nil {
		t.Fatalf("CreateSession after restore: %v", err)
	}
	if createResp.Instance.Title != "warmup" {
		t.Fatalf("unexpected create response after restore: %+v", createResp.Instance)
	}
}

// TestCallDaemon_RetriesThroughWarmup verifies the client side of the warm-up
// contract: a state-dependent call that lands during the warm-up window keeps
// retrying (bounded by daemonWarmupWait) and succeeds once the restore
// completes, so call sites racing a fresh daemon spawn — CLI create right
// after boot, task runs after an upgrade respawn — need no retry logic of
// their own.
func TestCallDaemon_RetriesThroughWarmup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return nil }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	manager, err := newManagerShell(config.DefaultConfig())
	if err != nil {
		t.Fatalf("newManagerShell: %v", err)
	}
	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	restored := make(chan struct{})
	go func() {
		defer close(restored)
		time.Sleep(300 * time.Millisecond)
		if err := manager.RestoreInstances(); err != nil {
			t.Errorf("RestoreInstances: %v", err)
		}
	}()
	t.Cleanup(func() { <-restored })

	data, err := CreateSession(CreateSessionRequest{
		Title: "retry-through-warmup", RepoPath: repoPath, Program: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession across warm-up: %v", err)
	}
	if data.Title != "retry-through-warmup" {
		t.Fatalf("unexpected created session: %+v", data)
	}
}

// TestControlServer_ShutdownWorksDuringWarmup verifies the Shutdown RPC is
// not gated: `af upgrade` must be able to stop a daemon that is still
// restoring instances.
func TestControlServer_ShutdownWorksDuringWarmup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	manager, err := newManagerShell(config.DefaultConfig())
	if err != nil {
		t.Fatalf("newManagerShell: %v", err)
	}
	shutdownCh := make(chan struct{})
	closeServer, err := startControlServer(manager, nil, nil, shutdownCh)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	var resp ShutdownResponse
	if err := callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp); err != nil {
		t.Fatalf("Shutdown during warm-up: %v", err)
	}
	if !resp.OK {
		t.Fatalf("Shutdown during warm-up returned OK=false")
	}
	select {
	case <-shutdownCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("shutdownCh was not closed within 2s of a warm-up Shutdown RPC")
	}
}

// TestEnsureDaemon_TreatsWarmingDaemonAsRunning is the doomed-spawn-race
// regression test for #829: with a warming daemon bound on the socket,
// EnsureDaemon's ping succeeds, so it must return nil without launching a
// second daemon process.
func TestEnsureDaemon_TreatsWarmingDaemonAsRunning(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	var launches atomic.Int32
	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error {
		launches.Add(1)
		return nil
	}
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	manager, err := newManagerShell(config.DefaultConfig())
	if err != nil {
		t.Fatalf("newManagerShell: %v", err)
	}
	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	if err := EnsureDaemon(); err != nil {
		t.Fatalf("EnsureDaemon against warming daemon: %v", err)
	}
	if got := launches.Load(); got != 0 {
		t.Fatalf("EnsureDaemon spawned %d daemon(s) while a warming daemon held the socket — the #829 doomed-spawn race", got)
	}
}

// TestRunDaemon_BindsSocketBeforeRestoreCompletes exercises the full RunDaemon
// startup with a restore that never finishes until the test releases it. If
// RunDaemon still bound the socket after the restore (the pre-#829 ordering),
// the ping poll below would never succeed and the test would time out — so a
// passing run proves the bind happens strictly before the restore completes,
// regardless of how slow the restore is. It also pins the warm-up contract at
// the daemon level: PID file present, EnsureDaemon does not spawn, state RPCs
// return the typed starting error, and a Shutdown RPC ends a warming daemon
// promptly without saving (or wiping) instance state.
func TestRunDaemon_BindsSocketBeforeRestoreCompletes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	restoreStarted := make(chan struct{})
	restoreGate := make(chan struct{})
	restoreReturned := make(chan struct{})
	prevRestore := restoreManagerForStartup
	restoreManagerForStartup = func(m *Manager) error {
		close(restoreStarted)
		<-restoreGate
		close(restoreReturned)
		return nil
	}
	var launches atomic.Int32
	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error {
		launches.Add(1)
		return nil
	}
	t.Cleanup(func() {
		restoreManagerForStartup = prevRestore
		launchDaemonProcessFn = prevLaunch
		close(restoreGate)
		// Join the gated goroutine only if the stub actually ran — on an
		// early test failure RunDaemon may not have reached the restore.
		select {
		case <-restoreStarted:
		default:
			return
		}
		select {
		case <-restoreReturned:
		case <-time.After(2 * time.Second):
			t.Errorf("gated restore goroutine did not exit after release")
		}
	})

	runDone := make(chan error, 1)
	go func() { runDone <- RunDaemon(config.DefaultConfig()) }()

	// The socket must come up while the restore is still blocked. Generous
	// deadline for CI; on a healthy box this completes in a few milliseconds.
	bindDeadline := time.Now().Add(5 * time.Second)
	for pingDaemon() != nil {
		if time.Now().After(bindDeadline) {
			t.Fatalf("control socket did not bind while restore was in progress (#829 ordering regressed)")
		}
		select {
		case err := <-runDone:
			t.Fatalf("RunDaemon exited before binding the socket: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-restoreReturned:
		t.Fatalf("restore completed before the socket answered — the gate is broken and this test proves nothing")
	default:
	}
	// Wait for the warm-up goroutine to enter the restore: in RunDaemon's
	// program order that is strictly after the PID-file write, making the
	// assertions below deterministic.
	select {
	case <-restoreStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("RunDaemon never started the instance restore")
	}

	// PID file is written right after the bind so StopDaemon and the upgrade
	// SIGTERM fallback can find a warming daemon.
	pidPath, err := daemonPIDFilePath()
	if err != nil {
		t.Fatalf("daemonPIDFilePath: %v", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("daemon PID file missing during warm-up: %v", err)
	}

	// EnsureDaemon must see the warming daemon as RUNNING — no second spawn.
	if err := EnsureDaemon(); err != nil {
		t.Fatalf("EnsureDaemon during warm-up: %v", err)
	}
	if got := launches.Load(); got != 0 {
		t.Fatalf("EnsureDaemon spawned %d daemon(s) during warm-up", got)
	}

	// State-dependent RPCs fail fast with the typed starting error.
	var promptResp SendPromptResponse
	rpcErr := callDaemonNoEnsure("SendPrompt", SendPromptRequest{Title: "x", Prompt: "hi"}, &promptResp)
	if !IsDaemonStartingErr(rpcErr) {
		t.Fatalf("SendPrompt during warm-up: want daemon-starting error, got: %v", rpcErr)
	}

	// Shutdown must end a warming daemon promptly — it cannot wait for the
	// restore (which this test never releases until cleanup).
	result, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown during warm-up: %v", err)
	}
	if result != ShutdownViaRPC {
		t.Fatalf("RequestShutdown during warm-up returned %v, want ShutdownViaRPC", result)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("RunDaemon returned error on warm-up shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("RunDaemon did not exit within 5s of a warm-up Shutdown RPC")
	}
}
