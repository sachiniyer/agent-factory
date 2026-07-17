package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestRunDaemon_StopsVSCodeEditorsOnWarmUpShutdown pins the ONE thing that makes
// the "a daemon shutdown strands no editor" invariant hold on every exit path:
// the vscode.Stop() defer must be registered at manager construction, not after
// the instance restore.
//
// The restore runs concurrently precisely so a shutdown during a slow warm-up
// exits promptly — and both of those early returns (SIGTERM and the Shutdown RPC)
// leave RunDaemon without ever reaching the post-restore section. A defer
// installed down there is therefore skipped by exactly the exits it exists for.
//
// This is reachable, not theoretical. The HTTP server and its webtab route bind
// BEFORE the restore, and webTabProxyHandler gates only on a nil manager — so a
// stale iframe refresh during warm-up resolves its session through refreshLocked
// (which loads instances from disk on its own, driving its own restore) and can
// spawn a code-server while the daemon is still warming. A Shutdown/SIGTERM
// moments later orphaned it: a live editor holding a loopback port with no daemon
// left to reap it.
//
// The editor is a registered marker rather than a real code-server: the wiring —
// whether Stop runs at all on this exit path — is what rots, and a marker proves
// it without paying for a process start. vscode_server_test.go covers really
// killing a child.
func TestRunDaemon_StopsVSCodeEditorsOnWarmUpShutdown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	restoreStarted := make(chan struct{})
	restoreGate := make(chan struct{})
	restoreReturned := make(chan struct{})
	// The restore hook hands us RunDaemon's OWN manager — the only way to reach the
	// supervisor it will (or will not) stop — and holds the daemon in warm-up.
	var warming *Manager
	prevRestore := restoreManagerForStartup
	restoreManagerForStartup = func(m *Manager) error {
		warming = m
		close(restoreStarted)
		<-restoreGate
		close(restoreReturned)
		return nil
	}
	t.Cleanup(func() {
		restoreManagerForStartup = prevRestore
		close(restoreGate)
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

	select {
	case <-restoreStarted:
	case err := <-runDone:
		t.Fatalf("RunDaemon exited before starting the restore: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunDaemon never started the instance restore")
	}

	// An editor exists while the daemon is still warming — the state a webtab
	// request serving off the already-bound HTTP route creates.
	const key = "repo|worker"
	registerVSCodeMarker(warming, key)
	if !vscodeServerRegistered(warming, key) {
		t.Fatal("fixture precondition: the marker editor was not registered")
	}

	// Shut down mid-warm-up: the restore is still gated and never completes, so
	// RunDaemon takes the early return that skipped the old defer.
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
		t.Fatal("RunDaemon did not exit within 5s of a warm-up Shutdown RPC")
	}
	select {
	case <-restoreReturned:
		t.Fatal("the restore completed before shutdown — the gate is broken and this test proves nothing")
	default:
	}

	// The assertion: the warm-up exit stopped the supervisor.
	if vscodeServerRegistered(warming, key) {
		t.Fatal("a daemon shut down during warm-up left a VS Code editor running: " +
			"the vscode.Stop() defer is registered after the restore, so both warm-up exit paths skip it, " +
			"stranding a code-server that holds its loopback port with no daemon left to reap it")
	}
	warming.vscode.mu.Lock()
	stopped := warming.vscode.stopped
	warming.vscode.mu.Unlock()
	if !stopped {
		t.Fatal("the supervisor was not marked stopped on the warm-up exit path; " +
			"a late spawn could still register an editor after shutdown")
	}
}
