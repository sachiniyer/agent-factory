package daemon

import (
	"errors"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// Tests for the #5007 PID-exit wait: RequestShutdown reports the shutdown
// target's PID, and WaitForShutdownCompletion(pid) waits for that process to
// exit — a positive signal — instead of for its socket to go quiet, which a
// still-draining daemon can satisfy early. Every test points
// AGENT_FACTORY_HOME at a temp dir, and the "daemons" whose exit is awaited are
// throwaway sleep processes, so the host's real daemon is never touched.

// TestRequestShutdownReturnsAckPID: the Shutdown ack carries the acknowledging
// process's PID, and RequestShutdown hands it back to the caller.
func TestRequestShutdownReturnsAckPID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	closeFn, err := startControlServer(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	result, pid, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if result != ShutdownViaRPC {
		t.Fatalf("shutdown result = %v, want ShutdownViaRPC", result)
	}
	if pid != os.Getpid() {
		t.Fatalf("shutdown pid = %d, want the acknowledging process %d", pid, os.Getpid())
	}
}

// pidlessShutdownControl is a daemon built before ShutdownResponse carried a
// PID: Ping reports one, Shutdown acknowledges without.
type pidlessShutdownControl struct{ pid int }

func (c pidlessShutdownControl) Ping(_ PingRequest, resp *PingResponse) error {
	resp.PID = c.pid
	return nil
}

func (c pidlessShutdownControl) Shutdown(_ ShutdownRequest, resp *ShutdownResponse) error {
	resp.OK = true
	return nil
}

// TestRequestShutdownFallsBackToPingPID: against a daemon whose Shutdown ack
// has no PID, RequestShutdown returns the PID its pre-shutdown Ping reported.
func TestRequestShutdownFallsBackToPingPID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	const fakePID = 424242
	srv := rpc.NewServer()
	if err := srv.RegisterName(controlServiceName, pidlessShutdownControl{pid: fakePID}); err != nil {
		t.Fatalf("register Control: %v", err)
	}
	_, cleanup := startFakeControlListener(t, srv)
	t.Cleanup(cleanup)

	result, pid, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if result != ShutdownViaRPC {
		t.Fatalf("shutdown result = %v, want ShutdownViaRPC", result)
	}
	if pid != fakePID {
		t.Fatalf("shutdown pid = %d, want the Ping-reported %d", pid, fakePID)
	}
}

// startReapedProcess starts name/args and reaps it in the background, so once
// it exits it does not linger as a zombie that kill(pid, 0) still reports
// alive (macOS has no /proc cmdline to tell the difference). The cleanup kills
// it if the test left it running.
func startReapedProcess(t *testing.T, name string, args ...string) int {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd.Process.Pid
}

// TestWaitForShutdownCompletionWaitsForPIDExit: the wait returns nil once the
// named process exits, well before the bound.
func TestWaitForShutdownCompletionWaitsForPIDExit(t *testing.T) {
	pid := startReapedProcess(t, "sleep", "0.3")

	start := time.Now()
	if err := WaitForShutdownCompletion(pid); err != nil {
		t.Fatalf("WaitForShutdownCompletion(%d): %v", pid, err)
	}
	if elapsed := time.Since(start); elapsed >= shutdownCompleteGrace {
		t.Fatalf("wait took %s, want well under the %s bound", elapsed, shutdownCompleteGrace)
	}
	if pidLooksAlive(pid) {
		t.Fatalf("WaitForShutdownCompletion returned while pid %d is still alive", pid)
	}
}

// TestWaitForShutdownCompletionIgnoresProcessName: a daemon whose argv[0] is
// not `af` (a renamed or relocated binary) is still waited on correctly,
// because the wait checks only liveness, never the process name.
func TestWaitForShutdownCompletionIgnoresProcessName(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	pid := startReapedProcess(t, "bash", "-c", "exec -a af-renamed sleep 0.5")

	if err := WaitForShutdownCompletion(pid); err != nil {
		t.Fatalf("WaitForShutdownCompletion(%d): %v", pid, err)
	}
	if pidLooksAlive(pid) {
		t.Fatalf("WaitForShutdownCompletion returned while pid %d is still alive", pid)
	}
}

// TestWaitForShutdownCompletionPIDTimesOut: a process still alive at the bound
// yields ErrShutdownIncomplete — and the wait never signals it, so it is still
// alive afterwards.
func TestWaitForShutdownCompletionPIDTimesOut(t *testing.T) {
	pid := startReapedProcess(t, "sleep", "30")

	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 200 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })

	err := WaitForShutdownCompletion(pid)
	if err == nil {
		t.Fatalf("expected a timeout error while pid %d keeps running", pid)
	}
	if !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("timeout error = %v, want it to wrap ErrShutdownIncomplete", err)
	}
	if !pidLooksAlive(pid) {
		t.Fatalf("pid %d died during the wait; the wait must only observe, never signal", pid)
	}
}

// TestProcessStartTokenIdentifiesIncarnation: the token is observable and
// stable for a live process on the platforms that support it — including one
// whose comm contains spaces and parentheses, which the Linux /proc/<pid>/stat
// parse must read past.
func TestProcessStartTokenIdentifiesIncarnation(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("no start token on %s", runtime.GOOS)
	}
	name := "sleep"
	if runtime.GOOS == "linux" {
		sleepPath, err := exec.LookPath("sleep")
		if err != nil {
			t.Skip("sleep not available")
		}
		data, err := os.ReadFile(sleepPath)
		if err != nil {
			t.Skipf("read %s: %v", sleepPath, err)
		}
		name = filepath.Join(t.TempDir(), "a) (b c")
		if err := os.WriteFile(name, data, 0o755); err != nil {
			t.Fatalf("copy sleep: %v", err)
		}
	}
	pid := startReapedProcess(t, name, "30")

	token := processStartToken(pid)
	if token == "" {
		t.Fatalf("processStartToken(%d) = \"\" for a live process", pid)
	}
	if again := processStartToken(pid); again != token {
		t.Fatalf("processStartToken(%d) changed for the same process: %q then %q", pid, token, again)
	}
}

// stubStartTokens replaces processStartTokenFn with one that returns tokens in
// order, repeating the last.
func stubStartTokens(t *testing.T, tokens ...string) {
	t.Helper()
	prev := processStartTokenFn
	t.Cleanup(func() { processStartTokenFn = prev })
	calls := 0
	processStartTokenFn = func(int) string {
		tok := tokens[min(calls, len(tokens)-1)]
		calls++
		return tok
	}
}

// TestWaitForShutdownCompletionTreatsPIDReuseAsExit: a PID that is still alive
// but now belongs to a different incarnation means the daemon exited and its
// PID was recycled — the wait returns rather than burning the grace and naming
// an unrelated process in the hint. A token read that fails midway is NOT a
// change, so it cannot fabricate an exit.
func TestWaitForShutdownCompletionTreatsPIDReuseAsExit(t *testing.T) {
	pid := startReapedProcess(t, "sleep", "30")
	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 300 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })

	stubStartTokens(t, "daemon", "recycled")
	if err := WaitForShutdownCompletion(pid); err != nil {
		t.Fatalf("WaitForShutdownCompletion with a recycled pid: %v", err)
	}

	stubStartTokens(t, "daemon", "")
	if err := WaitForShutdownCompletion(pid); !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("WaitForShutdownCompletion with a failed token read = %v, want ErrShutdownIncomplete", err)
	}
}
