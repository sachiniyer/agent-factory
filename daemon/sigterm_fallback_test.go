package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// TestWriteDaemonPIDFile_AtomicAndPermissions verifies that writing the PID
// file produces a 0600 file containing the current process's PID (#504).
func TestWriteDaemonPIDFile_AtomicAndPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	if err := writeDaemonPIDFile(); err != nil {
		t.Fatalf("writeDaemonPIDFile: %v", err)
	}
	pidPath := filepath.Join(home, "daemon.pid")
	info, err := os.Stat(pidPath)
	if err != nil {
		t.Fatalf("stat PID file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("PID file mode = %o, want 0600", mode)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read PID file: %v", err)
	}
	gotPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse PID: %v", err)
	}
	if gotPID != os.Getpid() {
		t.Errorf("PID file contains %d, want %d (this process)", gotPID, os.Getpid())
	}

	// removeDaemonPIDFile is the defer companion. After running it the file
	// must be gone.
	removeDaemonPIDFile()
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("expected PID file to be removed, stat err=%v", err)
	}

	// Second remove must be a no-op (graceful shutdown can race with a
	// second SIGTERM arriving during teardown).
	removeDaemonPIDFile()
}

// TestRunDaemonPIDFileLifecycle drives the daemon's main loop end-to-end:
// start RunDaemon in a goroutine, wait for the PID file to appear, ask it to
// shut down via the Shutdown RPC, then verify the PID file was removed when
// the daemon exited (#504).
func TestRunDaemonPIDFileLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	installInstantBackend(t)
	// RunDaemon sweeps legacy per-task units at startup (#782); point the
	// sweep at empty temp dirs so the test never touches the host's real
	// unit directories or shells out to systemctl/launchctl.
	stubLegacyUnitSweep(t)

	pidPath := filepath.Join(home, "daemon.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("PID file already exists before daemon start, stat err=%v", err)
	}

	cfg := config.DefaultConfig()
	cfg.DaemonPollInterval = 50

	done := make(chan error, 1)
	go func() { done <- RunDaemon(cfg) }()

	// The daemon writes the PID file early in RunDaemon, before the main
	// select. Poll briefly for it to appear.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("PID file did not appear within 3s, stat err=%v", err)
	}

	// Ask the daemon to exit via the Shutdown RPC.
	result, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if result != ShutdownViaRPC {
		t.Fatalf("RequestShutdown returned %v, want ShutdownViaRPC", result)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunDaemon returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("RunDaemon did not return within 5s after Shutdown RPC")
	}

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("expected PID file to be removed after graceful shutdown, stat err=%v", err)
	}
}

// TestIsRPCMethodNotFoundErr covers the classifier change at the core of
// #504: a connected-but-stale daemon returns an rpc.ServerError that must
// trigger the SIGTERM fallback, NOT be folded into "no daemon present".
func TestIsRPCMethodNotFoundErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"method not found", rpc.ServerError("rpc: can't find method Control.Shutdown"), true},
		{"service not found", rpc.ServerError("rpc: can't find service Control"), true},
		{"other server error", rpc.ServerError("rpc: unexpected EOF"), false},
		{"plain error", errors.New("some other error"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRPCMethodNotFoundErr(c.err); got != c.want {
				t.Errorf("isRPCMethodNotFoundErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestIsDaemonAbsentErr_RejectsMethodNotFound is a regression guard: prior
// to #504 the issue report assumed isDaemonAbsentErr was folding the rpc
// method-not-found case into the "absent" bucket. It must NOT — otherwise
// the SIGTERM fallback never fires.
func TestIsDaemonAbsentErr_RejectsMethodNotFound(t *testing.T) {
	err := rpc.ServerError("rpc: can't find method Control.Shutdown")
	if isDaemonAbsentErr(err) {
		t.Fatalf("isDaemonAbsentErr(method-not-found) = true; would suppress the SIGTERM fallback")
	}
}

// spawnFakeDaemonWithDaemonFlag launches a long-lived child process whose
// /proc/<pid>/cmdline contains "--daemon" as a discrete token. We use
// bash's `exec -a` to rewrite argv[0] of `sleep` so the cmdline carries
// "--daemon" without sleep actually being asked to parse it as a flag (it
// would reject "--daemon" as an invalid time interval). The single process
// is just sleep, which terminates cleanly on SIGTERM — making this the
// minimum-moving-parts way to test the fallback's SIGTERM path.
//
// Returns the *exec.Cmd so the test can call cmd.Wait() to reap the zombie
// after SIGTERM. Without that, kill(pid, 0) keeps returning success against
// the zombie, defeating any "did it die?" check based on signal probes.
func spawnFakeDaemonWithDaemonFlag(t *testing.T) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available, skipping SIGTERM-fallback test: %v", err)
	}
	cmd := exec.Command("bash", "-c", "exec -a 'sleep --daemon af-test' sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	// Wait briefly for bash to exec into sleep so the cmdline we read is
	// the post-exec one with the rewritten argv[0]. Re-check up to ~500ms.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if isAgentFactoryDaemon(cmd.Process.Pid) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cmd
}

// TestSigtermFallback_KillsPIDFileDaemon verifies the happy path of #504:
// when the PID file points at a live process whose cmdline matches
// `--daemon`, SIGTERM is delivered and the process exits.
func TestSigtermFallback_KillsPIDFileDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	cmd := spawnFakeDaemonWithDaemonFlag(t)
	pid := cmd.Process.Pid
	defer func() {
		// Defensive: if anything went wrong and sigtermFallback didn't
		// terminate the fake daemon, make sure we don't leak a sleep.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Sanity: the spawned process is reachable and matches cmdline.
	if !pidLooksAlive(pid) {
		t.Fatalf("fake daemon pid=%d not alive immediately after spawn", pid)
	}
	if !isAgentFactoryDaemon(pid) {
		t.Fatalf("fake daemon pid=%d cmdline did not match --daemon", pid)
	}

	if err := os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(pid)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	// Start reaping in a goroutine so the kernel can clear the zombie as
	// soon as SIGTERM lands. Without this, kill(pid, 0) keeps reporting
	// the zombie as "alive" and we can't distinguish the success case from
	// "sleep ignored the signal".
	exited := make(chan *os.ProcessState, 1)
	go func() {
		state, _ := cmd.Process.Wait()
		exited <- state
	}()

	result, err := sigtermFallback()
	if err != nil {
		t.Fatalf("sigtermFallback: %v", err)
	}
	if result != ShutdownViaSIGTERM {
		t.Fatalf("sigtermFallback returned %v, want ShutdownViaSIGTERM", result)
	}

	select {
	case state := <-exited:
		if state == nil {
			t.Fatalf("fake daemon exited but ProcessState was nil")
		}
		if state.ExitCode() == 0 {
			// SIGTERM exits with a non-zero status in os.Process. A clean
			// 0 would mean sleep finished its natural 60s — i.e. SIGTERM
			// did not actually kill it.
			t.Fatalf("fake daemon exited with code 0; expected signal-induced exit (state=%v)", state)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("fake daemon pid=%d did not exit within 8s after sigtermFallback", pid)
	}

	// PID file should have been cleaned up by sigtermFallback.
	if _, err := os.Stat(filepath.Join(home, "daemon.pid")); !os.IsNotExist(err) {
		t.Errorf("expected PID file to be removed after sigtermFallback, stat err=%v", err)
	}
}

// TestSigtermFallback_IgnoresNonMatchingCmdline guards against killing an
// unrelated process whose PID happens to be in the PID file (e.g. PID
// reuse). The fallback should fall through to pgrep, which on a clean test
// env finds zero candidates and returns ShutdownNoDaemon — the unrelated
// process must remain alive.
func TestSigtermFallback_IgnoresNonMatchingCmdline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	// sleep, no --daemon flag in its argv — cmdlineHasDaemonFlag must say no.
	victim := exec.Command("sleep", "60")
	if err := victim.Start(); err != nil {
		t.Fatalf("start victim: %v", err)
	}
	defer func() {
		_ = victim.Process.Kill()
		_, _ = victim.Process.Wait()
	}()

	if err := os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(victim.Process.Pid)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	// We don't care which result the fallback returns (ShutdownNoDaemon or
	// an ambiguous-pgrep error if the host has rogue `af --daemon` procs).
	// We only require that the victim survives.
	_, _ = sigtermFallback()

	time.Sleep(100 * time.Millisecond)
	if !pidLooksAlive(victim.Process.Pid) {
		t.Fatalf("sigtermFallback killed an unrelated process (pid=%d) whose cmdline did not match --daemon",
			victim.Process.Pid)
	}
}

// TestSigtermFallback_DeadPID covers the dead-PID case: the PID file points
// at a process that no longer exists, pgrep falls through to "no matches".
// Per #553 the fallback's contract is invoked only after the Shutdown RPC
// has proven the daemon is running, so "could not locate a PID" must be
// reported as ShutdownFailed (not ShutdownNoDaemon) along with an actionable
// recovery hint — anything else would silently leave the stale daemon up.
func TestSigtermFallback_DeadPID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	// Pick a PID well above any realistic running PID. 0x7fffffff exceeds
	// the default pid_max on both Linux (32768/4M) and macOS (99999).
	deadPID := 0x7fffffff
	if err := os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(deadPID)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	result, err := sigtermFallback()
	if result != ShutdownFailed {
		t.Fatalf("sigtermFallback returned %v for dead PID, want ShutdownFailed", result)
	}
	if err == nil {
		t.Fatalf("sigtermFallback returned nil error for dead PID; expected one carrying the recovery hint")
	}
	msg := err.Error()
	if !strings.Contains(msg, `pkill -f "af --daemon"`) {
		t.Errorf("sigtermFallback error %q missing recovery hint with `pkill -f \"af --daemon\"`", msg)
	}
	if !strings.Contains(msg, strconv.Itoa(deadPID)) {
		t.Errorf("sigtermFallback error %q missing stale PID-file pid=%d in source", msg, deadPID)
	}
}

// TestSigtermFallback_NoPIDFileAndNoPgrep covers #553: the Shutdown RPC
// proved the daemon is listening on the socket, but the fallback path has
// no PID file to consult AND pgrep is unavailable. Returning ShutdownNoDaemon
// would contradict the established state and leave the stale daemon running;
// the fix is to return ShutdownFailed with an actionable error pointing at
// `pkill -f "af --daemon"`.
func TestSigtermFallback_NoPIDFileAndNoPgrep(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	// Force pgrep off PATH for this test only. Using a fresh, empty temp
	// dir guarantees exec.LookPath("pgrep") fails.
	t.Setenv("PATH", t.TempDir())

	result, err := sigtermFallback()
	if result != ShutdownFailed {
		t.Fatalf("sigtermFallback returned %v, want ShutdownFailed", result)
	}
	if err == nil {
		t.Fatalf("sigtermFallback returned nil error; expected one carrying the recovery hint")
	}
	msg := err.Error()
	if !strings.Contains(msg, `pkill -f "af --daemon"`) {
		t.Errorf("sigtermFallback error %q missing recovery hint with `pkill -f \"af --daemon\"`", msg)
	}
	if !strings.Contains(msg, "pgrep unavailable") {
		t.Errorf("sigtermFallback error %q missing `pgrep unavailable` source", msg)
	}
	if !strings.Contains(msg, "no pid-file") {
		t.Errorf("sigtermFallback error %q missing `no pid-file` source", msg)
	}
}

// TestErrIsProcessGone covers the small helper that decides whether a
// failed Signal call means "already gone" (benign) vs. a real problem.
func TestErrIsProcessGone(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"already finished", os.ErrProcessDone, true},
		{"message-only", fmt.Errorf("os: process already finished"), true},
		{"esrch literal", fmt.Errorf("kill: no such process"), true},
		{"unrelated", syscall.EPERM, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := errIsProcessGone(c.err); got != c.want {
				t.Errorf("errIsProcessGone(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// preShutdownDaemonRPC is an RPC service that exposes Ping but deliberately
// omits Shutdown — modeling a pre-#501 daemon that registered Control before
// the Shutdown method existed. Used by TestRequestShutdown_PreShutdownDaemon.
type preShutdownDaemonRPC struct{}

func (preShutdownDaemonRPC) Ping(_ PingRequest, resp *PingResponse) error {
	resp.OK = true
	return nil
}

// startPreShutdownFakeDaemon listens on the daemon control socket and serves
// a Control service that registers Ping but no Shutdown method. Returns a
// cleanup function that closes the listener.
func startPreShutdownFakeDaemon(t *testing.T) func() {
	t.Helper()
	socketPath, err := DaemonSocketPath()
	if err != nil {
		t.Fatalf("DaemonSocketPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		t.Fatalf("mkdir socket parent: %v", err)
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on %s: %v", socketPath, err)
	}
	server := rpc.NewServer()
	if err := server.RegisterName(controlServiceName, preShutdownDaemonRPC{}); err != nil {
		_ = listener.Close()
		t.Fatalf("RegisterName: %v", err)
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
	return func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}
}

// TestRequestShutdown_PreShutdownDaemon regresses sachiniyer/agent-factory#514.
// The scenario from the report: a daemon that registers Control as an RPC
// service but never registered Shutdown (the method added in #498/#501).
// RequestShutdown must:
//   - detect the rpc method-not-found via isRPCMethodNotFoundErr,
//   - route to sigtermFallback rather than treating the daemon as absent,
//   - return a sane (Result, error) pair without panicking.
//
// Prior to the #514 fix the sigtermFallback log.InfoLog.Printf call would
// nil-deref when the upgrade path reached this code without log.Initialize
// having been called. We can't replay the missing-Initialize case in this
// process (TestMain runs Initialize) — that's covered separately by the
// log-package default-initializer test. This test pins the routing+behavior
// half: the rpc-method-not-found branch must reach sigtermFallback cleanly,
// which is the structural precondition for the upgrade flow to recover.
func TestRequestShutdown_PreShutdownDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	stop := startPreShutdownFakeDaemon(t)
	defer stop()

	// Sanity: a Ping over the socket succeeds — this is what makes the
	// daemon look "alive" so RequestShutdown does not short-circuit at the
	// socket-stat step.
	if err := pingDaemon(); err != nil {
		t.Fatalf("ping fake daemon: %v", err)
	}

	// The fallback's pgrep step would, on a busy host, find a real
	// `af --daemon` process and try to signal it. That host-state
	// dependency belongs in an integration test, not a unit test —
	// here we only need to assert RequestShutdown completes without
	// a panic and surfaces a Result/error consistent with the routing
	// having reached the fallback. The result space (post-#553) is:
	//   - ShutdownFailed, non-nil error (clean test host: pgrep found
	//       no matches; or ambiguous pgrep matches; both fold into the
	//       "daemon is provably running but unsignaled" bucket)
	//   - ShutdownViaSIGTERM, nil       (host has a real daemon — uncommon)
	// What is NOT acceptable: a panic, ShutdownViaRPC (would mean the
	// fake daemon answered Shutdown, which it does not implement), or
	// ShutdownNoDaemon (would contradict the proven-alive socket).
	result, err := RequestShutdown()
	if result == ShutdownViaRPC {
		t.Fatalf("RequestShutdown returned ShutdownViaRPC; fake daemon has no Shutdown method — routing into the SIGTERM fallback is broken (err=%v)", err)
	}
	if result == ShutdownNoDaemon {
		t.Fatalf("RequestShutdown returned ShutdownNoDaemon after a successful Ping; the socket proved the daemon is alive, so #553's invariant is violated (err=%v)", err)
	}
}
