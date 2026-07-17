package commands

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// reexecCapture records what autoUpdateOnLaunch handed to the re-exec instead
// of letting it replace the test process.
type reexecCapture struct {
	calls int
	argv0 string
	argv  []string
	env   []string
}

// launchWithTTY drives autoUpdateOnLaunch with the TTY gate forced open and
// the re-exec captured, so tests can assert on the launch decision without the
// process vanishing mid-test.
func launchWithTTY(t *testing.T, cfg *config.Config) *reexecCapture {
	t.Helper()
	return launchWith(t, true, cfg)
}

// launchWith drives autoUpdateOnLaunch with the TTY gate forced to isTTY.
func launchWith(t *testing.T, isTTY bool, cfg *config.Config) *reexecCapture {
	t.Helper()
	prevTTY := stdoutIsTTYFn
	prevReexec := reexecFn
	prevNotice := autoUpdateNotice
	t.Cleanup(func() {
		stdoutIsTTYFn = prevTTY
		reexecFn = prevReexec
		autoUpdateNotice = prevNotice
	})
	got := &reexecCapture{}
	stdoutIsTTYFn = func() bool { return isTTY }
	reexecFn = func(argv0 string, argv, env []string) error {
		got.calls++
		got.argv0, got.argv, got.env = argv0, argv, env
		return nil // a real exec never returns; the capture is the observation
	}
	autoUpdateNotice = func(string, ...any) {}
	autoUpdateOnLaunch(cfg)
	return got
}

// seedNewerRelease wires every seam an end-to-end launch update touches: a
// linux host on `current`, a release `latest` waiting on the channel, a
// download that yields "new-binary", and a daemon restart that no-ops.
// Returns the path standing in for the installed binary.
func seedNewerRelease(t *testing.T, current, latest string) string {
	t.Helper()
	tempBin := tempBinPath(t)
	if err := os.WriteFile(tempBin, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}
	prevGOOS, prevFetch, prevDownload := runtimeGOOS, fetchLatestReleaseTagFn, downloadBinaryFn
	prevVersion, prevExe := version, osExecutableFn
	prevShutdown, prevRespawn := requestDaemonShutdownFn, respawnDaemonFn
	t.Cleanup(func() {
		runtimeGOOS, fetchLatestReleaseTagFn, downloadBinaryFn = prevGOOS, prevFetch, prevDownload
		version, osExecutableFn = prevVersion, prevExe
		requestDaemonShutdownFn, respawnDaemonFn = prevShutdown, prevRespawn
	})
	runtimeGOOS = "linux"
	version = current
	fetchLatestReleaseTagFn = func(string, time.Duration) (string, error) { return latest, nil }
	// Bypass the tarball extract by returning the raw binary directly.
	downloadBinaryFn = func(string, time.Duration) ([]byte, error) { return []byte("new-binary"), nil }
	osExecutableFn = func() (string, error) { return tempBin, nil }
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) { return daemon.ShutdownNoDaemon, nil }
	respawnDaemonFn = func(string) (respawnResult, error) { return respawnResult{}, nil }
	return tempBin
}

// TestAutoUpdateOnLaunchInstallsAndReexecs is the headline path: an
// interactive launch on a stale binary installs the newer release and re-execs
// into it, so the user lands in the new version on THIS launch rather than
// being told to restart.
func TestAutoUpdateOnLaunchInstallsAndReexecs(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	tempBin := seedNewerRelease(t, "1.0.0", "v1.0.1")

	got := launchWithTTY(t, nil)

	if got.calls != 1 {
		t.Fatalf("re-exec calls = %d, want 1 — an installed update must relaunch into the new binary", got.calls)
	}
	if got.argv0 != tempBin {
		t.Fatalf("re-exec argv0 = %q, want the freshly written binary %q", got.argv0, tempBin)
	}
	if contents, err := os.ReadFile(tempBin); err != nil || string(contents) != "new-binary" {
		t.Fatalf("binary contents = %q (err %v), want new-binary on disk before the re-exec", contents, err)
	}
	// The user's original invocation must survive the relaunch.
	if !slices.Equal(got.argv, os.Args) {
		t.Fatalf("re-exec argv = %v, want the original %v", got.argv, os.Args)
	}
	// The guard env is what stops a version-mismatched release from looping.
	if !slices.Contains(got.env, reexecGuardEnv+"=1") {
		t.Fatalf("re-exec env missing %s=1; without it a bad release could re-exec forever", reexecGuardEnv)
	}
}

// TestAutoUpdateOnLaunchSkipsWhenStdoutNotTTY covers the CI/script gate: a
// non-interactive `af` must not swap the binary out from under its caller.
func TestAutoUpdateOnLaunchSkipsWhenStdoutNotTTY(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	tempBin := seedNewerRelease(t, "1.0.0", "v1.0.1")

	fetchCalls := 0
	fetchLatestReleaseTagFn = func(string, time.Duration) (string, error) {
		fetchCalls++
		return "v1.0.1", nil
	}

	got := launchWith(t, false, nil)

	if fetchCalls != 0 {
		t.Fatalf("fetch called %d times with stdout not a TTY; expected 0 — a script's `af` must not self-update", fetchCalls)
	}
	if got.calls != 0 {
		t.Fatalf("re-exec calls = %d, want 0 when stdout is not a TTY", got.calls)
	}
	if contents, _ := os.ReadFile(tempBin); string(contents) != "old-binary" {
		t.Fatalf("binary contents = %q, want the original old-binary left untouched", contents)
	}
}

// TestAutoUpdateOnLaunchSkipsWhenAlreadyReexeced makes the loop guard real: the
// process an update exec'd into must not update again, however the versions
// compare.
func TestAutoUpdateOnLaunchSkipsWhenAlreadyReexeced(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	seedNewerRelease(t, "1.0.0", "v1.0.1")
	t.Setenv(reexecGuardEnv, "1")

	fetchCalls := 0
	fetchLatestReleaseTagFn = func(string, time.Duration) (string, error) {
		fetchCalls++
		return "v1.0.1", nil
	}

	got := launchWithTTY(t, nil)

	if fetchCalls != 0 {
		t.Fatalf("fetch called %d times inside a re-exec'd process; expected 0", fetchCalls)
	}
	if got.calls != 0 {
		t.Fatalf("re-exec calls = %d, want 0 — a re-exec'd process must never re-exec again", got.calls)
	}
	// The guard must be consumed, not left in the environment: this process
	// goes on to spawn tmux sessions and agents that inherit it, and a nested
	// `af` reading a stale guard would never auto-update again.
	if _, stillSet := os.LookupEnv(reexecGuardEnv); stillSet {
		t.Fatalf("%s survived the launch; it would leak into every tmux session this TUI spawns and pin nested af to no-auto-update", reexecGuardEnv)
	}
}

// TestAutoUpdateOnLaunchIsSilentWhenOffline is the fail-silent contract: the
// launch must proceed with no re-exec and nothing printed at the user when the
// release lookup fails.
func TestAutoUpdateOnLaunchIsSilentWhenOffline(t *testing.T) {
	withTestHome(t)
	_, errBuf := captureLogs(t)
	seedNewerRelease(t, "1.0.0", "v1.0.1")
	fetchLatestReleaseTagFn = func(string, time.Duration) (string, error) {
		return "", errors.New("simulated offline")
	}

	notices := 0
	prevNotice := autoUpdateNotice
	t.Cleanup(func() { autoUpdateNotice = prevNotice })

	prevTTY, prevReexec := stdoutIsTTYFn, reexecFn
	t.Cleanup(func() { stdoutIsTTYFn, reexecFn = prevTTY, prevReexec })
	reexecs := 0
	stdoutIsTTYFn = func() bool { return true }
	reexecFn = func(string, []string, []string) error { reexecs++; return nil }
	autoUpdateNotice = func(string, ...any) { notices++ }

	// The contract is that this returns at all: no panic, no error surfaced.
	autoUpdateOnLaunch(nil)

	if reexecs != 0 {
		t.Fatalf("re-exec calls = %d, want 0 when the check failed", reexecs)
	}
	if notices != 0 {
		t.Fatalf("notice printed %d times on a failed check; an offline user must not see update chatter at launch", notices)
	}
	// The detail belongs in the log, not on the user's terminal.
	if !strings.Contains(errBuf.String(), "simulated offline") {
		t.Fatalf("expected the failure in ErrorLog, got:\n%s", errBuf.String())
	}
}

// TestAutoUpdateOnLaunchWithinThrottleMakesNoNetworkCall pins the zero-latency
// promise: a relaunch inside the window must not touch the network at all.
func TestAutoUpdateOnLaunchWithinThrottleMakesNoNetworkCall(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	seedNewerRelease(t, "1.0.0", "v1.0.0") // up to date: first launch records a check

	fetchCalls := 0
	fetchLatestReleaseTagFn = func(string, time.Duration) (string, error) {
		fetchCalls++
		return "v1.0.0", nil
	}

	launchWithTTY(t, nil)
	if fetchCalls != 1 {
		t.Fatalf("first launch fetch calls = %d, want 1", fetchCalls)
	}

	launchWithTTY(t, nil)
	if fetchCalls != 1 {
		t.Fatalf("second launch fetch calls = %d, want 1 — a relaunch inside the throttle window must skip the check entirely", fetchCalls)
	}
}

// TestAutoUpdateOnLaunchRefusesDowngrade guards the never-downgrade rule on the
// automatic path: a preview user switching back to stable resolves an OLDER
// tag, which must never be installed behind their back.
func TestAutoUpdateOnLaunchRefusesDowngrade(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	tempBin := seedNewerRelease(t, "1.0.138-preview-2", "v1.0.137")

	downloads := 0
	downloadBinaryFn = func(string, time.Duration) ([]byte, error) {
		downloads++
		return []byte("older-binary"), nil
	}

	got := launchWithTTY(t, nil)

	if downloads != 0 {
		t.Fatalf("download called %d times for an older release; expected 0 — auto-update must never downgrade", downloads)
	}
	if got.calls != 0 {
		t.Fatalf("re-exec calls = %d, want 0 when nothing was installed", got.calls)
	}
	if contents, _ := os.ReadFile(tempBin); string(contents) != "old-binary" {
		t.Fatalf("binary contents = %q, want the newer old-binary left in place", contents)
	}
}

// TestAutoUpdateOnLaunchFallsBackToNoticeWhenReexecFails covers the degraded
// path from the spec: the binary is already on disk, so a failed exec must not
// take the launch down with it — the user gets one line and their TUI.
func TestAutoUpdateOnLaunchFallsBackToNoticeWhenReexecFails(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	seedNewerRelease(t, "1.0.0", "v1.0.1")

	prevTTY, prevReexec, prevNotice := stdoutIsTTYFn, reexecFn, autoUpdateNotice
	t.Cleanup(func() { stdoutIsTTYFn, reexecFn, autoUpdateNotice = prevTTY, prevReexec, prevNotice })

	var printed []string
	stdoutIsTTYFn = func() bool { return true }
	reexecFn = func(string, []string, []string) error { return errors.New("exec format error") }
	autoUpdateNotice = func(format string, a ...any) { printed = append(printed, format) }

	autoUpdateOnLaunch(nil) // must return rather than exit or panic

	joined := strings.Join(printed, "")
	if !strings.Contains(joined, "restart to use it") {
		t.Fatalf("notices = %q, want the restart-to-use-it fallback when the re-exec fails", printed)
	}
}

// TestAutoUpdateOnLaunchSkipsWhileAnotherLaunchHoldsTheLock pins the
// no-waiting rule. The check now sits in front of the TUI, so a second `af`
// started while the first is mid-download must skip and boot immediately — if
// it queued on the lock instead, it would hang, unexplained, for as long as
// the peer's download takes.
func TestAutoUpdateOnLaunchSkipsWhileAnotherLaunchHoldsTheLock(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	seedNewerRelease(t, "1.0.0", "v1.0.1")

	fetchCalls := 0
	fetchLatestReleaseTagFn = func(string, time.Duration) (string, error) {
		fetchCalls++
		return "v1.0.1", nil
	}

	// Stand in for a peer `af` holding the lock across its download.
	held := make(chan struct{})
	released := make(chan struct{})
	go func() {
		_ = config.WithFileLock(lastCheckPath(), func() error {
			close(held)
			<-released
			return nil
		})
	}()
	<-held
	defer close(released)

	done := make(chan *reexecCapture, 1)
	go func() { done <- launchWithTTY(t, nil) }()

	select {
	case got := <-done:
		if fetchCalls != 0 {
			t.Fatalf("fetch called %d times while a peer held the lock; expected 0", fetchCalls)
		}
		if got.calls != 0 {
			t.Fatalf("re-exec calls = %d, want 0 when the check was skipped", got.calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("autoUpdateOnLaunch blocked on the update lock; a launch must never queue behind a peer's download")
	}
}

// TestAutoUpdateOnLaunchConfigOptOutSkipsCheck pins the documented pin:
// auto_update = false means no check at all, not merely no install.
func TestAutoUpdateOnLaunchConfigOptOutSkipsCheck(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	seedNewerRelease(t, "1.0.0", "v1.0.1")

	fetchCalls := 0
	fetchLatestReleaseTagFn = func(string, time.Duration) (string, error) {
		fetchCalls++
		return "v1.0.1", nil
	}

	got := launchWithTTY(t, &config.Config{AutoUpdate: false})

	if fetchCalls != 0 {
		t.Fatalf("fetch called %d times with auto_update = false; expected 0 — the opt-out must skip the check, not just the install", fetchCalls)
	}
	if got.calls != 0 {
		t.Fatalf("re-exec calls = %d, want 0 with auto_update = false", got.calls)
	}
}
