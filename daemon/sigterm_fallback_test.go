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
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// assertBinaryAgnosticDaemonHint checks that a SIGTERM-fallback recovery
// message points at a binary-name-agnostic kill command (#937). The old hint
// `pkill -f "af --daemon"` does not match daemons built from source as
// `agent-factory --daemon`, so we both require the new form and assert the
// stale binary-specific one is gone.
func assertBinaryAgnosticDaemonHint(t *testing.T, msg string) {
	t.Helper()
	if !strings.Contains(msg, `pkill -f -- '--daemon'`) {
		t.Errorf("error %q missing binary-agnostic recovery hint `pkill -f -- '--daemon'`", msg)
	}
	if strings.Contains(msg, `pkill -f "af --daemon"`) {
		t.Errorf("error %q still uses the binary-specific hint `pkill -f \"af --daemon\"`, which misses `agent-factory --daemon` (#937)", msg)
	}
}

// TestWriteDaemonPIDFile_AtomicAndPermissions verifies that writing the PID
// file produces a 0600 file containing the current process's PID (#504).
func TestWriteDaemonPIDFile_AtomicAndPermissions(t *testing.T) {
	home := testguard.SocketTempDir(t)
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
	home := testguard.SocketTempDir(t)
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

// stubDaemonScan replaces the host-wide pgrep scan with a controlled
// candidate list for the duration of the test (#793). Without this, any
// developer machine running the supervised daemon (`af daemon install`)
// contributes a real `af --daemon` process to the scan, making every branch
// that depends on the candidate count nondeterministic — and letting a test
// SIGTERM the host's real daemon.
func stubDaemonScan(t *testing.T, pids []int, err error) {
	t.Helper()
	orig := scanDaemonCandidatesFn
	scanDaemonCandidatesFn = func() ([]int, error) { return pids, err }
	t.Cleanup(func() { scanDaemonCandidatesFn = orig })
}

// spawnFakeDaemonWithDaemonFlag launches a long-lived child process whose
// /proc/<pid>/cmdline presents an agent-factory daemon: an "af" argv[0] plus a
// discrete "--daemon" token as a real argv element, satisfying both checks
// isAgentFactoryDaemon requires (#1004, #1214). See spawnFakeDaemonProc for how
// the crafted argv is built and reaped; it terminates cleanly on SIGTERM,
// making this the minimum-moving-parts way to test the fallback's SIGTERM path.
//
// Returns the *exec.Cmd so the test can call cmd.Wait() to reap the zombie
// after SIGTERM. Without that, kill(pid, 0) keeps returning success against
// the zombie, defeating any "did it die?" check based on signal probes.
func spawnFakeDaemonWithDaemonFlag(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := spawnFakeDaemonProc(t, "af", "sleep 60; :", "--daemon", "af-test")
	// Wait until the crafted post-exec argv is visible. Event-driven with a
	// generous bound so a loaded CI runner cannot miss the window — the old
	// fixed ~500ms wait could expire just before the exec landed, failing the
	// caller's cmdline sanity check spuriously (#878).
	waitForReady(t, "fake daemon cmdline exposes --daemon", func() bool {
		return isAgentFactoryDaemon(cmd.Process.Pid)
	})
	return cmd
}

// TestSigtermFallback_KillsPIDFileDaemon verifies the happy path of #504:
// when the PID file points at a live process whose cmdline matches
// `--daemon`, SIGTERM is delivered and the process exits.
func TestSigtermFallback_KillsPIDFileDaemon(t *testing.T) {
	home := testguard.SocketTempDir(t)
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
// reuse). The fallback must reject the PID-file candidate on its cmdline and
// fall through to the process scan — stubbed to zero candidates (#793) — so
// the outcome is deterministic: ShutdownFailed, and the unrelated process
// remains alive.
func TestSigtermFallback_IgnoresNonMatchingCmdline(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	stubDaemonScan(t, nil, nil)

	// sleep, no --daemon flag in its argv — argsHaveDaemonFlag must say no.
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

	result, err := sigtermFallback()
	if result != ShutdownFailed {
		t.Errorf("sigtermFallback returned %v, want ShutdownFailed (PID-file candidate rejected, scan empty)", result)
	}
	if err == nil {
		t.Errorf("sigtermFallback returned nil error; expected one carrying the recovery hint")
	}

	time.Sleep(100 * time.Millisecond)
	if !pidLooksAlive(victim.Process.Pid) {
		t.Fatalf("sigtermFallback killed an unrelated process (pid=%d) whose cmdline did not match --daemon",
			victim.Process.Pid)
	}
}

// TestSigtermFallback_DeadPID covers the dead-PID case: the PID file points
// at a process that no longer exists, and the process scan finds no
// candidates. Per #553 the fallback's contract is invoked only after the
// Shutdown RPC has proven the daemon is running, so "could not locate a PID"
// must be reported as ShutdownFailed (not ShutdownNoDaemon) along with an
// actionable recovery hint — anything else would silently leave the stale
// daemon up. The scan is stubbed to zero candidates so the stale-PID branch
// is asserted deterministically regardless of host daemons (#793).
func TestSigtermFallback_DeadPID(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	stubDaemonScan(t, nil, nil)

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
	assertBinaryAgnosticDaemonHint(t, msg)
	if !strings.Contains(msg, strconv.Itoa(deadPID)) {
		t.Errorf("sigtermFallback error %q missing stale PID-file pid=%d in source", msg, deadPID)
	}
}

// TestSigtermFallback_AmbiguousCandidates covers the multiple-daemons branch,
// previously untestable without real host daemons (#793): with no usable PID
// file and a scan returning several candidates, the fallback must refuse to
// guess, returning ShutdownFailed with an error naming every candidate PID.
//
// The home-scoped scan counts only THIS home's proven daemons, so the
// candidates must both be proven-ours for the guard to fire: two live `af
// --daemon` fakes serving THIS home. The singleton lock makes two same-home
// daemons unreachable in production, but the guard's contract — refuse to
// guess between proven-ours PIDs rather than signal an arbitrary one — must
// hold anyway, and is the only branch the count-greater-than-one case still
// guards now that foreign/unverifiable scan results are filtered out.
func TestSigtermFallback_AmbiguousCandidates(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	a := spawnFakeDaemonWithHome(t, home)
	b := spawnFakeDaemonWithHome(t, home)
	stubDaemonScan(t, []int{a, b}, nil)

	result, err := sigtermFallback()
	if result != ShutdownFailed {
		t.Fatalf("sigtermFallback returned %v for ambiguous candidates, want ShutdownFailed", result)
	}
	if err == nil {
		t.Fatalf("sigtermFallback returned nil error for ambiguous candidates")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") {
		t.Errorf("sigtermFallback error %q missing 'ambiguous'", msg)
	}
	for _, pid := range []int{a, b} {
		if !strings.Contains(msg, strconv.Itoa(pid)) {
			t.Errorf("sigtermFallback error %q missing candidate pid %d", msg, pid)
		}
	}
}

// TestSigtermFallback_NoPIDFileAndNoPgrep covers #553: the Shutdown RPC
// proved the daemon is listening on the socket, but the fallback path has
// no PID file to consult AND pgrep is unavailable. Returning ShutdownNoDaemon
// would contradict the established state and leave the stale daemon running;
// the fix is to return ShutdownFailed with an actionable error pointing at a
// binary-name-agnostic `pkill -f -- '--daemon'`.
func TestSigtermFallback_NoPIDFileAndNoPgrep(t *testing.T) {
	home := testguard.SocketTempDir(t)
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
	assertBinaryAgnosticDaemonHint(t, msg)
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
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	stop := startPreShutdownFakeDaemon(t)
	defer stop()

	// Sanity: a Ping over the socket succeeds — this is what makes the
	// daemon look "alive" so RequestShutdown does not short-circuit at the
	// socket-stat step.
	if err := pingDaemon(); err != nil {
		t.Fatalf("ping fake daemon: %v", err)
	}

	// The fallback's process scan is stubbed to zero candidates (#793):
	// unstubbed it is host-wide, and a host running the supervised daemon
	// would either get SIGTERMed by this test or make the result
	// nondeterministic. With no PID file and an empty scan, the only
	// acceptable outcome is ShutdownFailed with a non-nil error — the
	// daemon is provably running (Ping succeeded) but cannot be signaled.
	// What is NOT acceptable: a panic, ShutdownViaRPC (would mean the
	// fake daemon answered Shutdown, which it does not implement), or
	// ShutdownNoDaemon (would contradict the proven-alive socket).
	stubDaemonScan(t, nil, nil)
	result, err := RequestShutdown()
	if result == ShutdownViaRPC {
		t.Fatalf("RequestShutdown returned ShutdownViaRPC; fake daemon has no Shutdown method — routing into the SIGTERM fallback is broken (err=%v)", err)
	}
	if result == ShutdownNoDaemon {
		t.Fatalf("RequestShutdown returned ShutdownNoDaemon after a successful Ping; the socket proved the daemon is alive, so #553's invariant is violated (err=%v)", err)
	}
	if result != ShutdownFailed {
		t.Fatalf("RequestShutdown returned %v, want ShutdownFailed (no PID file, scan stubbed empty)", result)
	}
	if err == nil {
		t.Fatalf("RequestShutdown returned nil error; expected one carrying the recovery hint")
	}
}

// TestPidBelongsToThisHome exercises the home-binding helper the PID-file fast
// paths now depend on, driving it through the CALLER's resolved config dir (the
// path pidBelongsToThisHome takes) rather than an explicit wantHome parameter
// (the path TestVerifyScopedDaemon_MatchesOnlyOurHome exercises). It covers the
// three cases that gate a PID-file trust decision:
//   - a daemon serving THIS home is ours (trust the PID file),
//   - a daemon serving a DIFFERENT home is not ours (do not trust — the bug),
//   - a daemon with NO AGENT_FACTORY_HOME resolved the DEFAULT home, which is
//     ours only when the caller is also on the default home.
//
// Gated on /proc: the home binding reads AGENT_FACTORY_HOME from the candidate's
// environ, and the inline /proc skip is the established convention for these
// scoping tests (#1939 deliberately removed the testguard helper).
func TestPidBelongsToThisHome(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	ours := spawnFakeDaemonWithHome(t, home)
	if !pidBelongsToThisHome(ours) {
		t.Errorf("daemon serving this home (%q) not bound to this home; want true (trust the PID file)", home)
	}

	otherHome := testguard.SocketTempDir(t)
	foreign := spawnFakeDaemonWithHome(t, otherHome)
	if pidBelongsToThisHome(foreign) {
		t.Errorf("daemon serving a different home (%q) bound to this home (%q); want false — "+
			"this is the cross-home stale-PID-recycle case the fix exists to reject", otherHome, home)
	}

	// A daemon with NO AGENT_FACTORY_HOME resolved the DEFAULT home. We are
	// pointed at `home` (a throwaway temp dir), so the default-resolving daemon
	// is foreign to us. The complementary "is ours when the caller is also on
	// the default home" half is pinned in
	// TestVerifyScopedDaemon_UnsetHomeMeansDefaultHome.
	bare := spawnFakeDaemonWithHome(t, "")
	if pidBelongsToThisHome(bare) {
		t.Errorf("bare daemon (default home) bound to a non-default caller home (%q); want false", home)
	}
}

// TestSigtermFallback_PIDFileForeignHomeNotKilled is the cross-home regression
// for the stale-PID-recycle bypass: the caller's daemon.pid is stale and its
// PID has been recycled by ANOTHER home's live `af --daemon` (a different
// AGENT_FACTORY_HOME). The PID-file fast path in locateDaemonPID used to accept
// that PID on a cmdline-only match and SIGTERM it BEFORE the pgrep scan ever
// ran — killing the wrong home's daemon and reporting ShutdownViaSIGTERM
// (success) against it. With the home binding (pidBelongsToThisHome), the fast
// path rejects the foreign PID and falls through to the pgrep scan.
//
// The scan is then scoped by home too, so a foreign daemon alive alongside THIS
// home's own no longer makes the fallback wrongly "ambiguous": filtering to
// this home's proven daemons leaves exactly the one correct target, which is
// signalled. The foreign home's daemon is left untouched — the cross-home
// safety property — and this home's own stale daemon is reclaimed.
//
// This mirrors the real-world trigger (two different homes, the stale int
// lands on the other home's daemon) rather than a same-home two-daemon setup:
// the singleton lock prevents two daemons from serving the same home, so the
// same-home form of the bypass is not a reachable configuration.
func TestSigtermFallback_PIDFileForeignHomeNotKilled(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// The caller's own stale pre-#501 daemon: a live `af --daemon` serving
	// THIS home. Once the stale PID file is rejected, the home-scoped scan
	// reclaims this one correct target.
	ours := spawnFakeDaemonWithHome(t, home)

	// The OTHER home's daemon: a live `af --daemon` serving a different
	// AGENT_FACTORY_HOME. This is the PID the stale daemon.pid names — i.e.
	// the kernel recycled the stale int onto this process.
	otherHome := testguard.SocketTempDir(t)
	foreign := spawnFakeDaemonWithHome(t, otherHome)

	// Stale daemon.pid in THIS home points at the foreign home's daemon.
	if err := os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(foreign)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	// Stub the host-wide scan the way a real pgrep would answer: BOTH `af
	// --daemon` processes are visible. The home binding on the fast path
	// rejects the foreign PID file; the home-scoped scan keeps only OURS, so
	// the fallback reclaims the one correct target instead of refusing
	// (pre-fix) as "ambiguous" between a foreign and our daemon.
	stubDaemonScan(t, []int{foreign, ours}, nil)

	result, err := sigtermFallback()
	if err != nil {
		t.Fatalf("sigtermFallback: %v", err)
	}
	if result != ShutdownViaSIGTERM {
		t.Fatalf("sigtermFallback returned %v for a stale foreign PID file with this home's own daemon live, "+
			"want ShutdownViaSIGTERM (the home-scoped scan reclaims THIS home's daemon)", result)
	}

	// The foreign home's daemon MUST still be alive — the whole point of the
	// fix is that the stale PID file did not get it SIGTERM'd.
	if !pidLooksAlive(foreign) {
		t.Fatalf("foreign home's daemon pid=%d (serving %q) was killed by sigtermFallback run against %q; "+
			"the stale PID file bypassed the home binding", foreign, otherHome, home)
	}
	// This home's own stale daemon WAS reclaimed — the home-scoped scan turned
	// the old false "ambiguous" refusal into the correct single target.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !pidLooksAlive(ours) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pidLooksAlive(ours) {
		t.Fatalf("this home's daemon pid=%d was not reclaimed by sigtermFallback; the home-scoped scan "+
			"should have signalled the one proven-ours candidate", ours)
	}
	// The stale PID file is cleaned up on the success path.
	if _, err := os.Stat(filepath.Join(home, "daemon.pid")); !os.IsNotExist(err) {
		t.Errorf("expected stale PID file to be removed after sigtermFallback, stat err=%v", err)
	}
}

// TestSigtermFallback_PIDFileForeignHomeFallsThroughEmptyScan isolates the
// home-binding effect from the pgrep ambiguity guard. With a foreign PID file
// AND a pgrep scan stubbed to find nothing, the ONLY thing keeping
// sigtermFallback from killing the foreign daemon is the fast path's home
// binding: pre-fix, the fast path returned the foreign PID and SIGTERM'd it
// regardless of the scan; post-fix, the home binding rejects it and the empty
// scan yields the safe ShutdownFailed. Nothing is signalled.
func TestSigtermFallback_PIDFileForeignHomeFallsThroughEmptyScan(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	otherHome := testguard.SocketTempDir(t)
	foreign := spawnFakeDaemonWithHome(t, otherHome)

	if err := os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(foreign)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	// Scan finds nothing on top of the rejected PID-file candidate.
	stubDaemonScan(t, nil, nil)

	result, err := sigtermFallback()
	if result != ShutdownFailed {
		t.Fatalf("sigtermFallback returned %v, want ShutdownFailed (foreign PID rejected, scan empty)", result)
	}
	if err == nil {
		t.Fatalf("sigtermFallback returned nil error; expected the recovery hint")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(foreign)) {
		t.Errorf("sigtermFallback error %q missing the stale PID-file pid=%d source", err.Error(), foreign)
	}
	if !pidLooksAlive(foreign) {
		t.Fatalf("foreign home's daemon pid=%d was killed by sigtermFallback; the home binding should have "+
			"rejected the PID-file candidate and the empty scan should have signalled nothing", foreign)
	}
}

// TestSigtermFallback_PIDFileOwnHomeStillKills guards the happy path now that
// the fast path carries a home binding: a PID file pointing at a daemon that
// DOES serve THIS home must still be SIGTERM'd from the fast path. The home
// binding must not turn a correct PID file into a refusal — that would be a
// regression of #505's original behavior and strand the upgrade on a good
// candidate.
func TestSigtermFallback_PIDFileOwnHomeStillKills(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	ours := spawnFakeDaemonWithHome(t, home)
	// Sanity: the candidate is bound to this home before we even write the PID
	// file, so the test cannot pass for the wrong reason (e.g. a cmdline-only
	// match that the home binding later rejects).
	if !pidBelongsToThisHome(ours) {
		t.Fatalf("setup invariant: this home's fake daemon pid=%d is not bound to this home", ours)
	}

	if err := os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(ours)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	// Stub the scan to NONE: the fast path must short-circuit and return
	// before the scan runs, so an empty stub must not turn this into a
	// ShutdownFailed. This proves the home binding ACCEPTS our own daemon,
	// not that the scan would have found it anyway.
	stubDaemonScan(t, nil, nil)

	result, err := sigtermFallback()
	if err != nil {
		t.Fatalf("sigtermFallback: %v", err)
	}
	if result != ShutdownViaSIGTERM {
		t.Fatalf("sigtermFallback returned %v, want ShutdownViaSIGTERM (the PID file points at this home's own daemon)", result)
	}

	// The PID file should be cleaned up on the success path, and the daemon
	// must be dead (reaped via its process group by the spawn helper).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !pidLooksAlive(ours) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pidLooksAlive(ours) {
		t.Fatalf("this home's daemon pid=%d did not exit within 8s after sigtermFallback", ours)
	}
	if _, err := os.Stat(filepath.Join(home, "daemon.pid")); !os.IsNotExist(err) {
		t.Errorf("expected PID file to be removed after sigtermFallback, stat err=%v", err)
	}
}

// spawnFakeDaemonIn is the cwd- and binary-path-flexible variant of
// spawnFakeDaemonWithHome (stopall_test.go): it places the fake daemon's binary
// in binDir and starts it with its working directory set to cwd. Both axes are
// load-bearing for the home-binding classifier's two recorded-PID corrections:
// a RELATIVE AGENT_FACTORY_HOME must be resolved against the DAEMON's own cwd
// (not ours), and a binary living under /tmp/Test* must not be excluded as a
// Go test binary — and proving either requires controlling exactly those two
// axes that spawnFakeDaemonWithHome pins for the reset tests.
func spawnFakeDaemonIn(t *testing.T, home, binDir, cwd string) int {
	t.Helper()
	argv0 := filepath.Join(binDir, "af")
	cmd := fakeDaemonCmd(t, argv0, "sleep 300; :", "--daemon")
	env := []string{"PATH=" + os.Getenv("PATH")}
	if home != "" {
		env = append(env, "AGENT_FACTORY_HOME="+home)
	}
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	waitForArgv(t, pid, argv0)
	return pid
}

// TestSigtermFallback_ScanForeignOnlyNotKilled pins the pgrep-scan half of the
// cross-home fix. With no PID file and a host-wide scan that returns a SINGLE
// candidate serving ANOTHER home, the pre-fix pgrep branch returned that lone
// PID for signalAndWait and SIGTERM'd exactly the unrelated process the home
// binding is meant to protect (a foreign or unverifiable daemon that happens
// to be the only --daemon process on the box, e.g. its environ is unreadable or
// the intended daemon exited before the scan). locateDaemonPID must now
// reclassify every scanned candidate by uid and AGENT_FACTORY_HOME before
// selecting a signal target, so a single foreign match yields a safe
// ShutdownFailed and nothing is signalled.
func TestSigtermFallback_ScanForeignOnlyNotKilled(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// The only scan match serves a DIFFERENT home. Before the home-scoped
	// scan, this single foreign result was signalled unconditionally.
	otherHome := testguard.SocketTempDir(t)
	foreign := spawnFakeDaemonWithHome(t, otherHome)
	stubDaemonScan(t, []int{foreign}, nil)

	result, err := sigtermFallback()
	if result != ShutdownFailed {
		t.Fatalf("sigtermFallback returned %v for a single foreign scan result, want ShutdownFailed "+
			"(a foreign daemon must never be the signal target)", result)
	}
	if err == nil {
		t.Fatalf("sigtermFallback returned nil error; expected the recovery hint")
	}
	// Having deliberately refused to signal the foreign daemon, the recovery
	// hint must NOT tell the user to run a blanket `pkill -f -- '--daemon'`:
	// that command has no home or PID constraint, so it would kill exactly the
	// foreign daemon this filter left untouched, plus any unrelated process
	// carrying "--daemon". The message must say so plainly instead.
	if strings.Contains(err.Error(), "pkill -f -- '--daemon'") {
		t.Errorf("sigtermFallback error %q recommends a blanket `pkill -f -- '--daemon'` after the scan "+
			"found only foreign/unverifiable daemons; following it would kill exactly the daemons the "+
			"home filter refused to touch", err.Error())
	}
	if !strings.Contains(err.Error(), "left untouched") {
		t.Errorf("sigtermFallback error %q does not name the foreign/unverifiable candidates that were "+
			"left untouched, so the user has no way to tell refusing-to-signal from a host with no daemons", err.Error())
	}
	if !pidLooksAlive(foreign) {
		t.Fatalf("foreign home's daemon pid=%d (serving %q) was killed by sigtermFallback run against %q; "+
			"a single foreign scan result must not be signalled", foreign, otherHome, home)
	}
}

// TestPidBelongsToThisHome_TestBinaryNotExcluded pins the recorded-PID
// classifier's deliberate split from the host-scan heuristic. A daemon whose
// executable lives under /tmp/Test* (the shape `go test`-spawned binaries take
// via t.TempDir) serving THIS home IS ours to stop: a PID FILE names a real
// daemon, so the isTestBinaryArgs filter that keeps test fakes out of the
// host-wide scan must NOT be applied to it. Reusing verifyScopedDaemon for the
// PID-file path (which applies that heuristic) classified this binary foreign,
// deleted its live PID file, and left the daemon running — failing open.
// classifyDaemonHome now skips that heuristic, so it is bound to this home.
func TestPidBelongsToThisHome_TestBinaryNotExcluded(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// Binary placed under t.TempDir(), whose paths start with /tmp/Test…
	// (isTestBinaryArgs's trigger). spawnFakeDaemonWithHome deliberately uses
	// a dir outside /tmp/Test* (fakeBinDir) so isTestBinaryArgs does not fire;
	// here we WANT it to fire against the host-scan heuristic to prove the
	// recorded-PID path no longer applies it.
	binDir := t.TempDir()
	pid := spawnFakeDaemonIn(t, home, binDir, "")

	if !pidBelongsToThisHome(pid) {
		t.Errorf("daemon serving this home (%q) with a /tmp/Test* binary was not bound to this home; "+
			"want true — the recorded-PID classifier must not exclude a legitimate daemon as a Go "+
			"test binary (isTestBinaryArgs stays on host-wide scans only)", home)
	}
}

// TestPidBelongsToThisHome_RelativeHomeResolvedAgainstDaemonCwd pins the
// recorded-PID classifier's frame for a RELATIVE AGENT_FACTORY_HOME. Two
// daemons launched with the same relative value ("rel") from DIFFERENT
// directories serve different homes; resolving both against the CALLER's cwd
// (verifyScopedDaemon's canonicalDir) labelled the foreign one "ours" and let a
// stale PID file signal it. classifyDaemonHome now resolves a relative home
// against the DAEMON's own working directory, so the daemon sharing the
// caller's cwd is ours and the one launched from elsewhere is foreign.
func TestPidBelongsToThisHome_RelativeHomeResolvedAgainstDaemonCwd(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	// The caller is on a RELATIVE home; our wantHome resolves against OUR cwd.
	callerCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get caller cwd: %v", err)
	}
	t.Setenv("AGENT_FACTORY_HOME", "rel")

	// Binary outside /tmp/Test* so isTestBinaryArgs never fires and the test
	// isolates the cwd-frame variable.
	ours := spawnFakeDaemonIn(t, "rel", fakeBinDir(t), callerCwd)
	if !pidBelongsToThisHome(ours) {
		t.Errorf("daemon with AGENT_FACTORY_HOME=rel launched from the caller's cwd (%q) not bound to "+
			"this home; want true (its resolved home is the caller's)", callerCwd)
	}

	otherCwd := t.TempDir()
	foreign := spawnFakeDaemonIn(t, "rel", fakeBinDir(t), otherCwd)
	if pidBelongsToThisHome(foreign) {
		t.Errorf("daemon with AGENT_FACTORY_HOME=rel launched from a different cwd (%q) was bound to "+
			"this home; want false — a relative home must be resolved against the DAEMON's cwd, not "+
			"the caller's (resolving both against ours labels a foreign daemon 'ours')", otherCwd)
	}
}

// spawnFakeDaemonWithDefaultHome starts a fake `af --daemon` with NO
// AGENT_FACTORY_HOME (so it resolves the DEFAULT home) and HOME set to homeEnv,
// the frame the default is resolved in. The recorded-PID classifier must derive
// a default-home daemon's home from ITS OWN $HOME, not the caller's: two
// same-UID daemons that both unset AGENT_FACTORY_HOME but were launched under
// different HOME dirs serve different homes, and resolving either against the
// caller's $HOME makes the foreign one compare equal to ours (the #4793 hazard
// via the empty-env form). HOME is the only env the default-home resolution
// reads, so it is the only env the spawn pins beyond PATH.
func spawnFakeDaemonWithDefaultHome(t *testing.T, homeEnv string) int {
	t.Helper()
	argv0 := filepath.Join(fakeBinDir(t), "af")
	cmd := fakeDaemonCmd(t, argv0, "sleep 300; :", "--daemon")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + homeEnv,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	waitForArgv(t, pid, argv0)
	return pid
}

// TestPidBelongsToThisHome_DefaultHomeResolvedFromDaemonHome pins the
// recorded-PID classifier's frame for the DEFAULT home (no AGENT_FACTORY_HOME).
// The default home a daemon resolved is <ITS OWN $HOME>/.agent-factory, not
// <ours>; classifyDaemonHome previously routed the unset case through
// config.ConfigDirFor(""), which resolves the default against the CALLER's
// $HOME (os.UserHomeDir), so a same-UID daemon launched under a different HOME
// was labelled ours and could be signalled on a stale PID file or lone pgrep
// result. It now derives the default from the DAEMON's HOME, so a default-home
// daemon under a foreign HOME is foreign, and one under the caller's HOME is
// ours.
func TestPidBelongsToThisHome_DefaultHomeResolvedFromDaemonHome(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	// Put the CALLER on the DEFAULT home: unset AGENT_FACTORY_HOME and pin HOME
	// to a temp dir so os.UserHomeDir resolves there and the caller's home is
	// callerHome/.agent-factory.
	callerHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", "")
	t.Setenv("HOME", callerHome)

	// A same-UID default-home daemon launched under a DIFFERENT HOME serves
	// otherHome/.agent-factory. Pre-fix this compared equal to the caller's
	// default (both resolved against the caller's $HOME) and was signalled;
	// post-fix it is foreign.
	otherHome := t.TempDir()
	foreign := spawnFakeDaemonWithDefaultHome(t, otherHome)
	if scope := classifyDaemonHome(foreign); scope != daemonForeign {
		t.Errorf("default-home daemon with HOME=%q classified %v; want daemonForeign — the default "+
			"home must be resolved from the DAEMON's HOME, not the caller's ($HOME=%q)", otherHome, scope, callerHome)
	}

	// A default-home daemon launched under the CALLER's HOME serves the same
	// home and IS ours — the fix must not turn the matching case into a refusal.
	ours := spawnFakeDaemonWithDefaultHome(t, callerHome)
	if scope := classifyDaemonHome(ours); scope != daemonOurs {
		t.Errorf("default-home daemon with HOME=%q (the caller's) classified %v; want daemonOurs", callerHome, scope)
	}
}

// spawnFakeDaemonWithTildeHome starts a fake `af --daemon` with BOTH an
// explicit (tilde-spelled) AGENT_FACTORY_HOME and a pinned HOME, the frame a
// tilde home is expanded in (config's ExpandTilde reads os.UserHomeDir, i.e.
// the process's $HOME). The recorded-PID classifier must expand a
// "~"/"~/" home against the DAEMON's own $HOME, not the caller's: two same-UID
// daemons launched with the same AGENT_FACTORY_HOME=~/state under different
// HOME values serve different homes, and expanding both against the caller's
// $HOME makes the foreign one compare equal to ours (the #4793 hazard via the
// tilde form). HOME and AGENT_FACTORY_HOME are the only env the tilde
// resolution reads, so they are the only env the spawn pins beyond PATH.
func spawnFakeDaemonWithTildeHome(t *testing.T, afHome, homeEnv string) int {
	t.Helper()
	argv0 := filepath.Join(fakeBinDir(t), "af")
	cmd := fakeDaemonCmd(t, argv0, "sleep 300; :", "--daemon")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + homeEnv,
		"AGENT_FACTORY_HOME=" + afHome,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	waitForArgv(t, pid, argv0)
	return pid
}

// TestPidBelongsToThisHome_TildeHomeResolvedAgainstDaemonHome pins the
// recorded-PID classifier's frame for a TILDE AGENT_FACTORY_HOME. A
// "~/afhome" spelling expands against the DAEMON's own $HOME at the daemon's
// exec, but config.ConfigDirFor("~/afhome") expands it against the CALLER's
// $HOME (os.UserHomeDir). classifyDaemonHome previously routed the tilde form
// straight to ConfigDirFor, so two same-UID daemons launched with the same
// "~/afhome" under different HOME values both resolved to the caller's home
// and compared equal — the foreign one was labelled ours and could be
// signalled on a stale PID file or lone pgrep result. It now expands the tilde
// against the daemon's HOME, so a tilde-home daemon under a foreign HOME is
// foreign, and one under the caller's HOME is ours.
func TestPidBelongsToThisHome_TildeHomeResolvedAgainstDaemonHome(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	// Put the CALLER on a tilde home: AGENT_FACTORY_HOME=~/afhome expands
	// against the caller's HOME (callerHome), so wantHome = callerHome/afhome.
	callerHome := t.TempDir()
	t.Setenv("HOME", callerHome)
	t.Setenv("AGENT_FACTORY_HOME", "~/afhome")

	// A same-UID daemon launched with the SAME tilde spelling under the CALLER's
	// HOME serves the same directory and IS ours — the fix must not turn the
	// matching case into a refusal.
	ours := spawnFakeDaemonWithTildeHome(t, "~/afhome", callerHome)
	if scope := classifyDaemonHome(ours); scope != daemonOurs {
		t.Errorf("tilde-home daemon with HOME=%q (the caller's) classified %v; want daemonOurs", callerHome, scope)
	}

	// A same-UID daemon launched with the SAME tilde spelling but under a
	// DIFFERENT HOME serves otherHome/afhome. Pre-fix ConfigDirFor expanded the
	// tilde against the CALLER's HOME for both, so this compared equal to
	// wantHome and was signalled on a stale PID file; post-fix the tilde is
	// expanded against the DAEMON's HOME, so it is foreign.
	otherHome := t.TempDir()
	foreign := spawnFakeDaemonWithTildeHome(t, "~/afhome", otherHome)
	if scope := classifyDaemonHome(foreign); scope != daemonForeign {
		t.Errorf("tilde-home daemon with HOME=%q classified %v; want daemonForeign — the tilde must "+
			"expand against the DAEMON's HOME, not the caller's ($HOME=%q)", otherHome, scope, callerHome)
	}
}
