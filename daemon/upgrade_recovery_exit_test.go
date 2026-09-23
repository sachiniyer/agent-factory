package daemon

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the WHOLE af binary as a subprocess because the bug
// lives on the os.Exit path of HandleUpgradeRecoveryExec (daemon/
// upgrade_recovery.go), and os.Exit terminates the test binary if driven
// in-process — the same reason commands/doctorcmd_exit_test.go drives the
// doctor path through the built binary (see commands/doctorcmd_exit_test.go:
// 18-24). They reuse the subprocess-build pattern the commands package's
// afTestBinary seam (commands/sshrelaycmd_test.go) established, but the
// daemon package has no such seam today, so buildAFBinary below builds the
// production main-package binary per test into a t.TempDir() that
// t.Cleanup removes — self-contained, and it does not touch daemon/
// daemon_test.go's TestMain.
//
// The recovery handler is invoked pre-Cobra from main.go
// (daemon.HandleUpgradeRecoveryExec) and runs the previous-binary recovery
// actor. On a non-nil actor error it logs "daemon upgrade recovery actor
// failed: <err>" through log.ErrorLog (which on the file-open path writes
// only to the rotating log file, setting dirty=true) and then exits 1. The
// fix is that log.Close() runs before that os.Exit(1) so the operator-facing
// "wrote logs to <path>" hint (log/log.go:565, gated on fileWasOpened &&
// report && dirty) reaches stderr — the line that bridges the platform's
// stderr capture (the journal on Linux, recovery.log on macOS) to the file
// holding the actual error. Before the fix, os.Exit(1) skipped the dead
// `defer log.Close()` and stderr was empty.
//
// Captures go ONLY against the child's stderr and the child's
// <AGENT_FACTORY_HOME>/agent-factory.log file — never against the parent
// test binary's process-global loggers (log.WarningLog/InfoLog/ErrorLog),
// which scripts/daemon_log_capture_test.go pins to daemon/logcapture_test.go
// as the only installer in this package.

// buildAFBinary builds the production af binary (package main) for this test
// and returns its absolute path. The build output lives in a per-test temp
// dir that Go removes on cleanup. go must be on the test process's PATH; on
// a host where it is not, the test skips rather than fails (mirroring
// afTestBinary in commands/sshrelaycmd_test.go:216-238). build.Dir is ".."
// because a go test binary runs with the package dir as its working
// directory, and daemon/ is one level below the repo root the module
// resolves from.
func buildAFBinary(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("cannot build the af binary under test without go on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "af")
	build := exec.Command(goBin, "build", "-o", out, "github.com/sachiniyer/agent-factory")
	build.Dir = ".."
	if combined, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building af: %v: %s", err, combined)
	}
	return out
}

// canonicalHome returns the symlink-resolved, cleaned form of dir. The
// recovery invocation parser requires --home to be absolute and canonical
// (upgradetxn.ParseRecoveryInvocation rejects a home whose Clean differs),
// and upgradetxn.Load re-resolves the home through filepath.EvalSymlinks
// (canonicalExistingDir in internal/upgradetxn/storage.go) before reading
// activeJournalPath. On macOS t.TempDir() lives under /var -> /private/var,
// so the raw and canonical spellings differ; resolving once up front keeps
// the --home arg, the planted journal, the log file, and the assertions on
// ONE path so the binary reads exactly the journal this test writes and
// prints exactly the path this test checks.
func canonicalHome(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve home symlinks: %v", err)
	}
	return filepath.Clean(resolved)
}

// plantCorruptJournal writes a malformed active.json so that upgradetxn.Load
// returns a JSON decode error (not ErrNoActiveTransaction). That is the
// cheapest hermetic way to make RunUpgradeRecoveryActor return a non-nil
// error: runRecoveryActorWith returns the Load error immediately
// (internal/upgradetxn/recovery_actor.go:72-73), before any lease
// acquisition, supervisor run, daemon spawn, or tmux touch — so the child
// process fails fast, logs "daemon upgrade recovery actor failed: <err>"
// (which sets dirty=true via dirtyWriter), and reaches the os.Exit(1) path
// the bug lives on. home must be the canonical home, because Load reads
// active.json under the canonicalized home (see canonicalHome).
func plantCorruptJournal(t *testing.T, home string) {
	t.Helper()
	upgradeDir := filepath.Join(home, "upgrade")
	if err := os.MkdirAll(upgradeDir, 0o700); err != nil {
		t.Fatalf("mkdir upgrade journal dir: %v", err)
	}
	journalPath := filepath.Join(upgradeDir, "active.json")
	if err := os.WriteFile(journalPath, []byte("{not-valid-json"), 0o600); err != nil {
		t.Fatalf("write corrupt active.json: %v", err)
	}
}

// recoveryChildEnv builds the child's environment from the parent's, with
// AGENT_FACTORY_HOME and HOME pinned to the recovery throwaway home. The
// daemon package TestMain's testguard.SandboxHome() scopes only the TEST
// process; the child is a separate process that must be pinned itself (the
// pattern in commands/doctorcmd_exit_test.go:57-61). Existing AGENT_FACTORY_HOME
// and HOME entries are stripped first so the pinned value is unambiguous
// regardless of how getenv resolves duplicate keys across platforms. The
// handler also re-sets AGENT_FACTORY_HOME from --home before log.Initialize
// (daemon/upgrade_recovery.go:64), so the env pin is belt-and-suspenders;
// pinning HOME keeps any os.UserHomeDir fallback off the real home.
func recoveryChildEnv(home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AGENT_FACTORY_HOME=") || strings.HasPrefix(kv, "HOME=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "AGENT_FACTORY_HOME="+home, "HOME="+home)
	return env
}

// runRecoverySubprocess runs the built af binary with the given recovery
// argv in a throwaway home, capturing stdout, stderr, and the exit code. A
// 60s bound matches commands/sshrelaycmd_test.go's runAF so a wedged child
// fails the test instead of hanging the suite.
func runRecoverySubprocess(t *testing.T, bin, home string, args ...string) (stdout, stderr []byte, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = recoveryChildEnv(home)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	done := make(chan error, 1)
	require.NoError(t, cmd.Start())
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("af %v failed to run: %v (stderr: %s)", args, err, errBuf.String())
			}
			exitCode = ee.ExitCode()
		} else {
			exitCode = 0
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("af %v did not exit within 60s; a wedged recovery child must fail the test, not hang the suite (stderr: %s)", args, errBuf.String())
	}
	return outBuf.Bytes(), errBuf.Bytes(), exitCode
}

// TestRecoveryExitLogsHintOnActorFailure is the previously-failing test for
// the bug: HandleUpgradeRecoveryExec installed `defer log.Close()` but then
// exited via os.Exit on every terminal path, and os.Exit does not run
// deferred functions (Go spec, os#Exit), so log.Close() never ran and the
// operator-facing "wrote logs to <path>" hint was never emitted on the
// failure branch. The error went only to the rotating log file, the process
// exited 1 with empty stderr, and an operator reading the journal (Linux)
// or recovery.log (macOS) had no in-band pointer to agent-factory.log.
//
// It plants a corrupt active.json so the recovery actor fails hermetically
// (no daemon, tmux, or network), drives the built af binary as a subprocess
// with the internal __upgrade-recovery argv, and asserts: (1) exit 1,
// (2) the failure detail is in the rotating log file, and (3) the
// "wrote logs to <path>" hint reached stderr before exit 1 — the line the
// dead defer was supposed to print.
func TestRecoveryExitLogsHintOnActorFailure(t *testing.T) {
	home := canonicalHome(t, t.TempDir())
	plantCorruptJournal(t, home)

	bin := buildAFBinary(t)
	const txn = "test-txn"
	stdout, stderr, exitCode := runRecoverySubprocess(t, bin, home,
		"__upgrade-recovery", "--home", home, "--transaction", txn)

	// (1) A non-nil recovery-actor error must surface as exit 1: the
	// corrupt journal makes Load fail, RunUpgradeRecoveryActor returns
	// that error, and the handler exits 1.
	require.Equal(t, 1, exitCode,
		"a corrupt journal must abort the recovery actor and exit 1, got %d (stderr: %q)", exitCode, stderr)

	// (2) The failure detail must be in the rotating log file the handler
	// writes log.ErrorLog into (the only place it lands on the file-open
	// path), so the hinted pointer is worth following. Go's *log.Logger
	// writes through to the *os.File directly (O_APPEND, no buffering), so
	// the content is on disk before log.Close() runs and survives the
	// child's exit.
	logPath := filepath.Join(home, "agent-factory.log")
	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err, "the recovery run must have written its log at %s", logPath)
	assert.Contains(t, string(logBytes), "daemon upgrade recovery actor failed",
		"the log file must record the recovery actor's failure detail")

	// (3) THE FIX: the "wrote logs to <path>" hint reaches stderr before
	// exit 1. Before the fix, os.Exit(1) skipped the deferred log.Close(),
	// so stderr was empty and an operator had no in-band pointer to the
	// log file holding the failure detail.
	assert.Contains(t, string(stderr), "wrote logs to ",
		"stderr should contain the 'wrote logs to' hint before exit 1, but it is empty — os.Exit skipped defer log.Close(); stderr was: %q", stderr)
	assert.Contains(t, string(stderr), logPath,
		"the hint must name the log file the actor wrote to (%s); got: %q", logPath, stderr)

	// The recovery handler writes nothing to stdout; assert it stays empty
	// so a future change that adds a stdout write is a visible signal.
	assert.Empty(t, stdout, "the recovery handler writes nothing to stdout; got: %q", stdout)
}
