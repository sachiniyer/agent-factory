package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

func TestMain(m *testing.M) {
	// A re-exec of this test binary standing in for code-server (see
	// writeFakeVSCodeBinary): serve and exit. It must return BEFORE the tripwires
	// and sandboxing below — it is a child process of a test, not a test run, and
	// it must neither run the suite nor trip the guards its parent armed.
	if os.Getenv(fakeVSCodeEnv) != "" {
		fakeVSCodeServerMain()
		return
	}
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: fail loudly if a test leaks an af_ session onto the ambient tmux
	// server, and default the whole package into a sandboxed
	// AGENT_FACTORY_HOME so stray config/state/log writes land in a temp dir
	// instead of the developer's real one. Sandbox AFTER the tripwires
	// snapshot the real environment, BEFORE logging resolves its file path.
	verifyTmux := testguard.TmuxTripwire()
	restoreHome := testguard.SandboxHome()
	// #1122: default the whole package onto a private tmux server so a test
	// that forgets IsolateTmux can never create or sweep sessions on the
	// developer's real server.
	restoreTmux := testguard.SandboxTmux()
	log.Initialize(false)
	code := m.Run()
	log.Close()
	restoreTmux()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := verifyTmux(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// processAlive returns true if sending signal 0 to pid succeeds, meaning the process is still
// running and reachable.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// TestStopDaemon_DoesNotKillUnrelatedPID verifies that StopDaemon refuses to kill a process whose
// command line does not match an agent-factory daemon. Regression test for issue #264.
func TestStopDaemon_DoesNotKillUnrelatedPID(t *testing.T) {
	// Redirect config dir to a scratch location so we don't touch the user's real daemon.pid.
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	pidFile := filepath.Join(tmpHome, "daemon.pid")

	// Spawn a long-running process that is NOT an agent-factory daemon.
	sleepCmd := exec.Command("sleep", "60")
	if err := sleepCmd.Start(); err != nil {
		t.Fatalf("failed to start sleep process: %v", err)
	}
	victimPID := sleepCmd.Process.Pid
	defer func() {
		// Best-effort cleanup regardless of test outcome.
		_ = sleepCmd.Process.Kill()
		_, _ = sleepCmd.Process.Wait()
	}()

	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", victimPID)), 0644); err != nil {
		t.Fatalf("failed to write PID file: %v", err)
	}

	stopped, err := StopDaemon()
	if err != nil {
		t.Fatalf("StopDaemon returned error: %v", err)
	}
	if stopped {
		t.Fatalf("StopDaemon reported stopped=true for an unrelated PID; expected false")
	}

	// Give the process a brief moment; if StopDaemon killed it (the bug), it will have exited.
	time.Sleep(100 * time.Millisecond)

	if !processAlive(victimPID) {
		t.Fatalf("StopDaemon killed an unrelated process (PID %d); the vulnerability is still present", victimPID)
	}

	// PID file should have been cleaned up as stale.
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale PID file to be removed, stat err = %v", err)
	}
}

// TestStopDaemon_NoPIDFile verifies StopDaemon succeeds silently when there is
// no PID file AND reports stopped=false so callers don't claim a phantom
// success (#937). Pre-1.0.69 daemons write no PID file, so this no-op path is
// exactly the case `af reset` must not describe as "daemon has been stopped".
// Hermetic: a fresh temp config dir, no real daemon involved, nothing signaled.
func TestStopDaemon_NoPIDFile(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	stopped, err := StopDaemon()
	if err != nil {
		t.Fatalf("StopDaemon with no PID file should succeed, got: %v", err)
	}
	if stopped {
		t.Fatalf("StopDaemon with no PID file should report stopped=false (did nothing), got true")
	}
}

// TestStopDaemon_NonExistentPID verifies that StopDaemon treats a PID file pointing at a dead
// process as stale and removes it instead of returning an error or killing a reused PID.
func TestStopDaemon_NonExistentPID(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)
	pidFile := filepath.Join(tmpHome, "daemon.pid")

	// Use a large PID that we're confident isn't in use. On Linux the default pid_max is 32768
	// and on macOS it's 99999; 0x7fffffff is well above both.
	deadPID := 0x7fffffff

	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", deadPID)), 0644); err != nil {
		t.Fatalf("failed to write PID file: %v", err)
	}

	stopped, err := StopDaemon()
	if err != nil {
		t.Fatalf("StopDaemon returned error for dead PID: %v", err)
	}
	if stopped {
		t.Fatalf("StopDaemon reported stopped=true for a dead PID; expected false")
	}

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale PID file to be removed, stat err = %v", err)
	}
}

// TestArgsHaveDaemonFlag verifies that --daemon is matched only as a discrete argv element,
// not as a substring of unrelated flags like --daemonize (#342), and that spaces inside another
// argument (e.g. a spaced binary path in argv[0]) neither fabricate nor hide a match (#1214).
func TestArgsHaveDaemonFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "empty", args: nil, want: false},
		{name: "bare --daemon flag", args: []string{"/usr/local/bin/agent-factory", "--daemon"}, want: true},
		{name: "--daemon with leading args", args: []string{"agent-factory", "--verbose", "--daemon"}, want: true},
		{name: "--daemon= form", args: []string{"agent-factory", "--daemon=foo"}, want: true},
		{name: "--daemonize substring should not match", args: []string{"/usr/bin/some-tool", "--daemonize"}, want: false},
		{name: "--daemon-mode substring should not match", args: []string{"agent-factory", "--daemon-mode"}, want: false},
		{name: "no daemon flag at all", args: []string{"/usr/bin/sleep", "60"}, want: false},
		// #1214: a spaced binary path in argv[0] must not shred into a false
		// "--daemon" match, and a genuine --daemon still matches alongside it.
		{name: "spaced path, real --daemon", args: []string{"/home/John Smith/.local/bin/af", "--daemon"}, want: true},
		{name: "spaced path, no --daemon", args: []string{"/home/John Smith/.local/bin/af", "serve"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := argsHaveDaemonFlag(tt.args); got != tt.want {
				t.Errorf("argsHaveDaemonFlag(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestArgsAreDaemonBinary verifies the host-wide pgrep scan keeps only
// agent-factory daemon binaries — installed `af` or source-built
// `agent-factory` — so broadening the pgrep pattern to a bare `--daemon` can't
// claim an unrelated `--daemon` process (#937). Pure argv logic; nothing is
// signaled. Spaced argv[0] paths (#1214) are the key regression: filepath.Base
// must see the whole path, not a whitespace-split fragment.
func TestArgsAreDaemonBinary(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "empty", args: nil, want: false},
		{name: "installed af binary", args: []string{"/home/u/.local/bin/af", "--daemon"}, want: true},
		{name: "source-built agent-factory binary", args: []string{"/home/u/src/agent-factory", "--daemon"}, want: true},
		{name: "bare af in PATH", args: []string{"af", "--daemon"}, want: true},
		{name: "bare agent-factory in PATH", args: []string{"agent-factory", "--daemon"}, want: true},
		{name: "unrelated daemon", args: []string{"/usr/bin/dockerd", "--daemon"}, want: false},
		{name: "lookalike suffix", args: []string{"/usr/bin/not-agent-factory", "--daemon"}, want: false},
		// #1214: the spaced-install path must resolve to base "af"/"agent-factory".
		{name: "spaced path af", args: []string{"/home/John Smith/.local/bin/af", "--daemon"}, want: true},
		{name: "spaced path agent-factory", args: []string{"/opt/my tools/agent-factory", "--daemon"}, want: true},
		{name: "spaced path non-af", args: []string{"/opt/John Smith/tools/dockerd", "--daemon"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := argsAreDaemonBinary(tt.args); got != tt.want {
				t.Errorf("argsAreDaemonBinary(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestLooksLikeDaemonArgv is the exported-surface counterpart used by `af
// doctor`'s foreign-daemon scan. It must agree with the daemon's own
// PID-validation rules (#1004) and classify spaced-install paths correctly
// (#1214): both a discrete --daemon token AND an af/agent-factory basename.
func TestLooksLikeDaemonArgv(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "empty", args: nil, want: false},
		{name: "real af daemon", args: []string{"/home/u/.local/bin/af", "--daemon"}, want: true},
		{name: "spaced af daemon", args: []string{"/home/John Smith/.local/bin/af", "--daemon"}, want: true},
		{name: "af without --daemon", args: []string{"/home/u/.local/bin/af", "sessions"}, want: false},
		{name: "non-af with --daemon", args: []string{"sleep", "--daemon", "af-test"}, want: false},
		{name: "--daemonize lookalike", args: []string{"af", "--daemonize"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeDaemonArgv(tt.args); got != tt.want {
				t.Errorf("LooksLikeDaemonArgv(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestIsAgentFactoryDaemon_RequiresBinaryName is the regression test for
// #1004. isAgentFactoryDaemon validates PIDs sourced from the PID file
// (StopDaemon / locateDaemonPID) and previously checked only for a discrete
// "--daemon" token — never the binary name. That let a stale PID file whose
// PID had been reused by an unrelated process carrying "--daemon" in its argv
// be mistaken for our daemon and signaled. It must now require BOTH the
// "--daemon" flag AND an af/agent-factory binary name, mirroring the pgrep
// scan path (pgrepDaemonCandidates). Hermetic: spawns long-lived sleeps and
// reads /proc cmdline; never signals any real daemon.
func TestIsAgentFactoryDaemon_RequiresBinaryName(t *testing.T) {
	// spawn launches a process whose argv is [argv0, "--daemon", "af-test"]
	// with REAL argv boundaries and waits until that argv is visible in /proc.
	// argv0 may contain spaces (the #1214 spaced-install case).
	spawn := func(argv0 string) int {
		t.Helper()
		cmd := spawnFakeDaemonProc(t, argv0, "sleep 60; :", "--daemon", "af-test")
		pid := cmd.Process.Pid
		// Wait until the crafted argv is visible: the last element is the
		// "af-test" marker. Reading argv (not a joined string) means a spaced
		// argv0 stays intact, which is the whole point of the fix.
		waitForReady(t, fmt.Sprintf("pid=%d post-exec argv visible", pid), func() bool {
			args := daemonArgs(pid)
			return len(args) > 0 && args[len(args)-1] == "af-test"
		})
		return pid
	}

	// The exact #1004 repro: "--daemon" present but argv[0] base is "sleep",
	// not af/agent-factory. Pre-fix this returned true (the bug); it must now
	// be false so StopDaemon/locateDaemonPID refuse to signal it.
	nonAFPID := spawn("sleep")
	if isAgentFactoryDaemon(nonAFPID) {
		t.Errorf("isAgentFactoryDaemon(pid with argv %q) = true; "+
			"want false — a non-af process carrying --daemon must not be treated as our daemon (#1004)",
			daemonArgs(nonAFPID))
	}

	// A genuine agent-factory daemon (af argv[0] + --daemon) must still
	// validate true, so the fix doesn't break the real stop/reset path.
	afPID := spawn("af")
	if !isAgentFactoryDaemon(afPID) {
		t.Errorf("isAgentFactoryDaemon(pid with argv %q) = false; "+
			"want true — a real af --daemon process must still validate",
			daemonArgs(afPID))
	}

	// #1214: a daemon installed under a path with spaces. Pre-fix, the argv was
	// space-joined and re-split, so filepath.Base saw "John" (not "af") and the
	// daemon was undetectable — breaking `af reset`, health, and foreign-daemon
	// scans for every spaced install path. It must now validate true.
	//
	// Guarded on /proc because THIS ASSERTION CANNOT HOLD ON darwin — a REAL
	// DEFECT (#1942), not a harness quirk: daemonArgs recovers argv boundaries
	// from /proc/<pid>/cmdline, and macOS's `ps -p <pid> -o args=` fallback is
	// already space-joined, so #1214's fix is Linux-only and af's daemon really
	// is undetectable under a spaced install path there. Only this assertion is
	// guarded — the #1004 binary-name gates around it hold on darwin and must
	// keep running, which skipping the whole test would throw away. Remove the
	// guard when #1942 lands KERN_PROCARGS2-based argv on darwin.
	if testguard.HasProcFS() {
		spacedPID := spawn("/home/John Smith/.local/bin/af")
		if !isAgentFactoryDaemon(spacedPID) {
			t.Errorf("isAgentFactoryDaemon(pid with argv %q) = false; "+
				"want true — an af daemon installed under a spaced path must validate (#1214)",
				daemonArgs(spacedPID))
		}
	} else {
		t.Logf("skipping the #1214 spaced-install assertion: no /proc, so argv boundaries " +
			"are unrecoverable and af's daemon is genuinely undetectable under a spaced " +
			"path here — see #1942 (REAL DEFECT)")
	}

	// A non-af binary under a spaced path carrying --daemon must still be
	// rejected: the space fix must not weaken the binary-name gate (#1004).
	spacedNonAFPID := spawn("/opt/John Smith/tools/notdaemon")
	if isAgentFactoryDaemon(spacedNonAFPID) {
		t.Errorf("isAgentFactoryDaemon(pid with argv %q) = true; "+
			"want false — a non-af spaced-path process carrying --daemon must be rejected (#1004)",
			daemonArgs(spacedNonAFPID))
	}
}

// TestStopDaemon_SIGTERMFirst verifies that StopDaemon sends SIGTERM (giving
// the daemon's signal handler a chance to run SaveInstances) and only
// escalates to SIGKILL after the grace period. Regression test for #571 —
// before the fix this called proc.Kill() unconditionally, bypassing the
// daemon's state-save path.
func TestStopDaemon_SIGTERMFirst(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	// Spawn a process that responds to SIGTERM by exiting (bash re-raises the
	// signal by default) and presents an agent-factory daemon argv: an "af"
	// argv[0] plus a discrete "--daemon" token as a real argv element, so it
	// satisfies both checks isAgentFactoryDaemon requires (#1004, #1214). This
	// is the same recipe used in TestSigtermFallback_KillsPIDFileDaemon.
	cmd := spawnFakeDaemonProc(t, "af", "sleep 60; :", "--daemon", "af-test")
	pid := cmd.Process.Pid

	// Wait for the post-exec cmdline to be readable. Event-driven with a
	// generous bound so a loaded runner cannot expire the old fixed 2s wait
	// before the exec lands (#878).
	waitForReady(t, fmt.Sprintf("fake daemon pid=%d cmdline exposes --daemon", pid), func() bool {
		return isAgentFactoryDaemon(pid)
	})

	// Reap in a goroutine so /proc/<pid>/cmdline clears once the process
	// exits — otherwise pidLooksAlive keeps seeing the zombie as alive and
	// StopDaemon would wait the full grace period before declaring success.
	exited := make(chan *os.ProcessState, 1)
	go func() {
		state, _ := cmd.Process.Wait()
		exited <- state
	}()

	pidFile := filepath.Join(tmpHome, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	start := time.Now()
	stopped, err := StopDaemon()
	if err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if !stopped {
		t.Fatalf("StopDaemon reported stopped=false after signaling a live fake daemon; expected true")
	}
	elapsed := time.Since(start)

	// If StopDaemon respected SIGTERM-first, the process exits within the
	// poll cadence — well under the grace period. A near-full-grace duration
	// would mean we're back to the immediate-SIGKILL bug or that the poll
	// never observed the exit.
	if elapsed >= stopDaemonGrace {
		t.Errorf("StopDaemon took %s (>= grace %s); SIGTERM may not have been sent first", elapsed, stopDaemonGrace)
	}

	select {
	case state := <-exited:
		if state == nil {
			t.Fatalf("process state nil")
		}
		ws, ok := state.Sys().(syscall.WaitStatus)
		if !ok {
			t.Fatalf("WaitStatus assertion failed: %T", state.Sys())
		}
		if !ws.Signaled() {
			t.Fatalf("process exited without a signal (status=%v); expected SIGTERM", state)
		}
		if ws.Signal() != syscall.SIGTERM {
			t.Errorf("process exited via signal %v, want SIGTERM (the SIGKILL escalation path fired)", ws.Signal())
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("fake daemon did not exit within 8s of StopDaemon")
	}
}

// TestStopDaemon_EscalatesToSIGKILL verifies the SIGKILL escalation path:
// when the daemon ignores SIGTERM, StopDaemon must wait stopDaemonGrace and
// then SIGKILL the process. Companion to TestStopDaemon_SIGTERMFirst.
func TestStopDaemon_EscalatesToSIGKILL(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	// Shorten the grace so the test runs in ~300ms instead of ~5s.
	origGrace := stopDaemonGrace
	origPoll := stopDaemonPoll
	stopDaemonGrace = 250 * time.Millisecond
	stopDaemonPoll = 25 * time.Millisecond
	defer func() {
		stopDaemonGrace = origGrace
		stopDaemonPoll = origPoll
	}()

	// The fake daemon sets SIGTERM to SIG_IGN via `trap "" TERM` (the
	// empty-string form, which truly ignores the signal) and loops, so it can
	// only be stopped by the SIGKILL escalation. Its argv[0] is an "af" binary
	// name with a discrete "--daemon" token as a real argv element, so it
	// passes both checks isAgentFactoryDaemon requires (#1004, #1214). A
	// ready-file sentinel proves the trap was installed before SIGTERM lands,
	// closing a race where the cmdline becomes visible while bash is still
	// parsing the script.
	readyFile := filepath.Join(tmpHome, "trap-ready")
	script := fmt.Sprintf(`trap "" TERM; : > %s; while :; do sleep 1; done`, readyFile)
	cmd := spawnFakeDaemonProc(t, "af", script, "--daemon", "af-test")
	pid := cmd.Process.Pid

	// Wait until the trap is installed (sentinel present) AND the rewritten
	// cmdline is visible. Event-driven with a generous bound so a loaded runner
	// cannot expire the old fixed 2s wait before the child is ready (#878).
	waitForReady(t, fmt.Sprintf("trap-ready sentinel + pid=%d cmdline exposes --daemon", pid), func() bool {
		if _, err := os.Stat(readyFile); err != nil {
			return false
		}
		return isAgentFactoryDaemon(pid)
	})

	exited := make(chan *os.ProcessState, 1)
	go func() {
		state, _ := cmd.Process.Wait()
		exited <- state
	}()

	pidFile := filepath.Join(tmpHome, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	start := time.Now()
	stopped, err := StopDaemon()
	if err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if !stopped {
		t.Fatalf("StopDaemon reported stopped=false after signaling a live fake daemon; expected true")
	}
	elapsed := time.Since(start)

	// Must have waited at least the grace period before escalating. A
	// shorter elapsed time would mean we skipped the SIGTERM grace and
	// went straight to SIGKILL — the pre-#571 behavior.
	if elapsed < stopDaemonGrace {
		t.Errorf("StopDaemon returned in %s, want at least grace %s before SIGKILL", elapsed, stopDaemonGrace)
	}

	select {
	case state := <-exited:
		if state == nil {
			t.Fatalf("process state nil")
		}
		ws, ok := state.Sys().(syscall.WaitStatus)
		if !ok {
			t.Fatalf("WaitStatus assertion failed: %T", state.Sys())
		}
		if !ws.Signaled() {
			t.Fatalf("process exited without a signal (status=%v); expected SIGKILL", state)
		}
		if ws.Signal() != syscall.SIGKILL {
			t.Errorf("process exited via signal %v, want SIGKILL (escalation never fired)", ws.Signal())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("fake daemon did not exit within 5s after StopDaemon")
	}
}

// TestRefreshDaemonInstances_SkipsCorruptedRepoAtStartup is the regression
// test for #603. Pre-fix, a single corrupted per-repo instances.json caused
// refreshDaemonInstances(nil) to return (nil, err); NewManager propagated
// that error so the daemon never started, orphaning every AutoYes session
// across every repo. The fix logs a WARNING and continues, so valid repos
// still load on startup.
func TestRefreshDaemonInstances_SkipsCorruptedRepoAtStartup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	// Capture warning output so we can assert the corrupted repo was named
	// in the log line — silent skipping would re-introduce a different bug
	// (invisible data drop).
	var warnBuf bytes.Buffer
	prevOut := log.WarningLog.Writer()
	log.WarningLog.SetOutput(io.MultiWriter(prevOut, &warnBuf))
	t.Cleanup(func() { log.WarningLog.SetOutput(prevOut) })

	// Stub the session restore so we don't need a live tmux/PTY backend.
	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	validRepoID := "valid-repo-a"
	validData := []session.InstanceData{{Title: "valid-session"}}
	validJSON, err := json.Marshal(validData)
	if err != nil {
		t.Fatalf("marshal valid: %v", err)
	}
	if err := config.SaveRepoInstances(validRepoID, validJSON); err != nil {
		t.Fatalf("save valid repo: %v", err)
	}

	corruptedRepoID := "corrupted-repo-b"
	if err := config.SaveRepoInstances(corruptedRepoID, json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupted repo: %v", err)
	}

	got, err := refreshDaemonInstances(nil)
	if err != nil {
		t.Fatalf("refreshDaemonInstances(nil) returned error on corrupted-repo input — daemon startup would fail and orphan every AutoYes session: %v", err)
	}

	validKey := daemonInstanceKey(validRepoID, "valid-session")
	if _, ok := got[validKey]; !ok {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		t.Fatalf("expected valid repo's session %q to load; got keys: %v", validKey, keys)
	}

	corruptedPrefix := corruptedRepoID + "\x00"
	for k := range got {
		if strings.HasPrefix(k, corruptedPrefix) {
			t.Fatalf("corrupted repo %q must not contribute entries on startup; key=%q", corruptedRepoID, k)
		}
	}

	if !strings.Contains(warnBuf.String(), corruptedRepoID) {
		t.Fatalf("expected warning log to name corrupted repo %q so users can find the bad file; got: %q", corruptedRepoID, warnBuf.String())
	}
}

// TestRefreshDaemonInstances_PreservesExistingForCorruptedRepoOnPoll covers
// the polling path (existing != nil). The pre-fix code returned the entire
// `existing` map on any unmarshal error, preserving prior in-memory state.
// The fix replaces that with a per-repo skip — to keep the same "don't drop
// running sessions on a transient corrupt write" guarantee, the skip path
// re-hydrates this repo's prior keys from `existing` into the returned map.
func TestRefreshDaemonInstances_PreservesExistingForCorruptedRepoOnPoll(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	prevOut := log.WarningLog.Writer()
	log.WarningLog.SetOutput(io.Discard)
	t.Cleanup(func() { log.WarningLog.SetOutput(prevOut) })

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	repoID := "corrupted-repo-c"
	if err := config.SaveRepoInstances(repoID, json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupted repo: %v", err)
	}

	priorKey := daemonInstanceKey(repoID, "still-running")
	prior := &session.Instance{}
	existing := map[string]*session.Instance{priorKey: prior}

	got, err := refreshDaemonInstances(existing)
	if err != nil {
		t.Fatalf("refreshDaemonInstances on poll path errored on corrupted-repo input: %v", err)
	}
	if got[priorKey] != prior {
		t.Fatalf("polling refresh dropped prior in-memory instance for corrupted repo %q; running AutoYes session would be silently abandoned", repoID)
	}
}

// TestRefreshDaemonInstances_PreservesInstancesForMissingRepoDirectory covers
// #736: when a repo's instances directory is deleted externally while the
// daemon is running, config.LoadAllRepoInstances no longer returns that repo,
// so the polling refresh must preserve its in-memory instances (parallel to
// the corrupted-JSON path) and log a warning naming the missing repo. Dropping
// them would silently abandon a running AutoYes session.
func TestRefreshDaemonInstances_PreservesInstancesForMissingRepoDirectory(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	var warnBuf bytes.Buffer
	prevOut := log.WarningLog.Writer()
	log.WarningLog.SetOutput(io.MultiWriter(prevOut, &warnBuf))
	t.Cleanup(func() { log.WarningLog.SetOutput(prevOut) })

	prevFromInstance := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		return &session.Instance{}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prevFromInstance })

	// A repo that stays on disk — proves we don't over-preserve and that the
	// normal load path still works alongside a vanished repo.
	presentRepoID := "present-repo"
	presentJSON, err := json.Marshal([]session.InstanceData{{Title: "present-session"}})
	if err != nil {
		t.Fatalf("marshal present: %v", err)
	}
	if err := config.SaveRepoInstances(presentRepoID, presentJSON); err != nil {
		t.Fatalf("save present repo: %v", err)
	}

	// A repo whose directory we delete out from under the daemon.
	missingRepoID := "missing-repo"
	missingJSON, err := json.Marshal([]session.InstanceData{{Title: "running-session"}})
	if err != nil {
		t.Fatalf("marshal missing: %v", err)
	}
	if err := config.SaveRepoInstances(missingRepoID, missingJSON); err != nil {
		t.Fatalf("save missing repo: %v", err)
	}

	// In-memory state as if both sessions had already been loaded.
	presentKey := daemonInstanceKey(presentRepoID, "present-session")
	missingKey := daemonInstanceKey(missingRepoID, "running-session")
	presentInst := &session.Instance{}
	missingInst := &session.Instance{}
	existing := map[string]*session.Instance{
		presentKey: presentInst,
		missingKey: missingInst,
	}

	// Remove the missing repo's directory entirely (not just the file), as an
	// external `rm -rf` of the repo's storage would.
	missingPath, err := config.RepoInstancesPath(missingRepoID)
	if err != nil {
		t.Fatalf("resolve missing repo path: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(missingPath)); err != nil {
		t.Fatalf("remove missing repo dir: %v", err)
	}

	got, err := refreshDaemonInstances(existing)
	if err != nil {
		t.Fatalf("refreshDaemonInstances returned error: %v", err)
	}

	if got[missingKey] != missingInst {
		t.Fatalf("running AutoYes session for missing repo %q was dropped; in-memory instance must be preserved", missingRepoID)
	}
	if got[presentKey] != presentInst {
		t.Fatalf("session for present repo %q should still be loaded", presentRepoID)
	}
	if !strings.Contains(warnBuf.String(), missingRepoID) {
		t.Fatalf("expected warning naming missing repo %q; got: %q", missingRepoID, warnBuf.String())
	}
}

// TestRefreshDaemonInstances_StartupDoesNotInventMissingRepos guards the
// startup path (existing == nil): there is no prior in-memory state to
// preserve, so a missing repo must contribute nothing and not panic.
func TestRefreshDaemonInstances_StartupDoesNotInventMissingRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevOut := log.WarningLog.Writer()
	log.WarningLog.SetOutput(io.Discard)
	t.Cleanup(func() { log.WarningLog.SetOutput(prevOut) })

	got, err := refreshDaemonInstances(nil)
	if err != nil {
		t.Fatalf("startup refresh errored: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map at startup with no repos, got %d entries", len(got))
	}
}

// TestFindInstanceDataByTitle_NamesCorruptedRepoOnNotFound covers the all-repo
// scan path (#730): a corrupted repo must be logged and named in the returned
// error instead of silently skipped, so a title that could be hidden in the
// bad file doesn't surface as a bare "not found."
func TestFindInstanceDataByTitle_NamesCorruptedRepoOnNotFound(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	var warnBuf bytes.Buffer
	prevOut := log.WarningLog.Writer()
	log.WarningLog.SetOutput(io.MultiWriter(prevOut, &warnBuf))
	t.Cleanup(func() { log.WarningLog.SetOutput(prevOut) })

	corruptedRepoID := "corrupted-repo"
	if err := config.SaveRepoInstances(corruptedRepoID, json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupted repo: %v", err)
	}

	_, _, err := findInstanceDataByTitle("ghost-title", "")
	if err == nil {
		t.Fatalf("expected error when title missing and a repo is corrupted")
	}
	if !strings.Contains(err.Error(), corruptedRepoID) {
		t.Fatalf("expected error to name corrupted repo %q; got: %v", corruptedRepoID, err)
	}
	if !strings.Contains(warnBuf.String(), corruptedRepoID) {
		t.Fatalf("expected warning naming corrupted repo %q; got: %q", corruptedRepoID, warnBuf.String())
	}
}

// TestStopDaemon_RefusesSelfPID verifies that StopDaemon refuses to kill the current test process
// even if the PID file points at it.
func TestStopDaemon_RefusesSelfPID(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)
	pidFile := filepath.Join(tmpHome, "daemon.pid")

	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		t.Fatalf("failed to write PID file: %v", err)
	}

	// If StopDaemon killed us, the test binary would exit with signal: killed.
	stopped, err := StopDaemon()
	if err != nil {
		t.Fatalf("StopDaemon returned error: %v", err)
	}
	if stopped {
		t.Fatalf("StopDaemon reported stopped=true for our own PID; expected false")
	}

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("expected PID file to be removed, stat err = %v", err)
	}
}
