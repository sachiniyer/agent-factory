package commands

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/autoupdate"
)

// reexecCapture records what autoUpdateOnLaunch handed to the re-exec instead
// of letting it replace the test process.
type reexecCapture struct {
	calls int
	argv0 string
	argv  []string
	env   []string
	// proceed is autoUpdateOnLaunch's answer: whether the launch goes on.
	proceed bool
	// notices are the user-facing lines the launch path printed.
	notices []string
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
	autoUpdateNotice = func(format string, a ...any) {
		got.notices = append(got.notices, fmt.Sprintf(format, a...))
	}
	got.proceed = autoUpdateOnLaunch(cfg)
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
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, int, error) { return daemon.ShutdownNoDaemon, 0, nil }
	respawnDaemonFn = func(string, int) (respawnResult, error) { return respawnResult{}, nil }
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
		_ = config.WithFileLock(autoupdate.CheckCachePath(), func() error {
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

// drainingRestartSeams wires a launch update whose daemon restart hits an
// unfinished shutdown (#5007): the old daemon acked Shutdown as oldPID, and the
// first respawn is withheld with daemon.ErrShutdownIncomplete. secondWait is
// what the one extra wait on oldPID reports, retryErr what the retried respawn
// returns. It returns the recorded wait PIDs and respawn calls.
func drainingRestartSeams(t *testing.T, oldPID int, secondWait, retryErr error) (waitPIDs *[]int, respawnPIDs *[]int) {
	t.Helper()
	prevWait := waitForShutdownCompletionFn
	t.Cleanup(func() { waitForShutdownCompletionFn = prevWait })
	waitPIDs, respawnPIDs = new([]int), new([]int)
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, int, error) {
		return daemon.ShutdownViaRPC, oldPID, nil
	}
	respawnDaemonFn = func(_ string, pid int) (respawnResult, error) {
		*respawnPIDs = append(*respawnPIDs, pid)
		if len(*respawnPIDs) == 1 {
			return respawnResult{}, fmt.Errorf("the old daemon is still finishing its shutdown (%w)", daemon.ErrShutdownIncomplete)
		}
		return respawnResult{}, retryErr
	}
	waitForShutdownCompletionFn = func(pid int) error {
		*waitPIDs = append(*waitPIDs, pid)
		return secondWait
	}
	return waitPIDs, respawnPIDs
}

// TestAutoUpdateOnLaunchStandsDownWhileOldDaemonDrains: the binary installed,
// but the previous daemon outlived both shutdown waits and still holds the home
// lock. Re-execing would land the new TUI on an EnsureDaemon it cannot win, so
// the launch must stand down with one line saying why and what to do.
func TestAutoUpdateOnLaunchStandsDownWhileOldDaemonDrains(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	tempBin := seedNewerRelease(t, "1.0.0", "v1.0.1")
	waitPIDs, respawnPIDs := drainingRestartSeams(t, 4242,
		fmt.Errorf("%w: daemon pid 4242 still running", daemon.ErrShutdownIncomplete), nil)

	got := launchWithTTY(t, nil)

	if got.proceed {
		t.Fatalf("autoUpdateOnLaunch = true, want false while the old daemon still drains")
	}
	if got.calls != 0 {
		t.Fatalf("re-exec calls = %d, want 0 — the new TUI could not reach a daemon yet", got.calls)
	}
	if !slices.Equal(*waitPIDs, []int{4242}) {
		t.Fatalf("second wait PIDs = %v, want [4242]", *waitPIDs)
	}
	if !slices.Equal(*respawnPIDs, []int{4242}) {
		t.Fatalf("respawn calls = %v, want only the withheld first attempt", *respawnPIDs)
	}
	notice := strings.Join(got.notices, "")
	if !strings.Contains(notice, "still finishing its shutdown") || !strings.Contains(notice, "run af again") {
		t.Fatalf("notice = %q, want the still-finishing shutdown and run-again guidance", notice)
	}
	if strings.Contains(notice, "and exits on its own") || !strings.Contains(notice, "may be wedged") {
		t.Fatalf("notice = %q, must not promise the old daemon exits and must name the wedged case", notice)
	}
	if contents, err := os.ReadFile(tempBin); err != nil || string(contents) != "new-binary" {
		t.Fatalf("binary contents = %q (err %v), want the update installed even though the launch stood down", contents, err)
	}
}

// TestAutoUpdateOnLaunchRetriesRespawnOnceDrainFinishes: the first respawn was
// withheld, but the old daemon exits during the second wait. The withheld
// respawn is retried with the old PID and the launch re-execs as normal.
func TestAutoUpdateOnLaunchRetriesRespawnOnceDrainFinishes(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	tempBin := seedNewerRelease(t, "1.0.0", "v1.0.1")
	waitPIDs, respawnPIDs := drainingRestartSeams(t, 4242, nil, nil)

	got := launchWithTTY(t, nil)

	if !got.proceed {
		t.Fatalf("autoUpdateOnLaunch = false, want true once the old daemon has exited")
	}
	if got.calls != 1 || got.argv0 != tempBin {
		t.Fatalf("re-exec calls = %d into %q, want 1 into %q", got.calls, got.argv0, tempBin)
	}
	if !slices.Equal(*waitPIDs, []int{4242}) {
		t.Fatalf("second wait PIDs = %v, want [4242]", *waitPIDs)
	}
	if !slices.Equal(*respawnPIDs, []int{4242, 4242}) {
		t.Fatalf("respawn calls = %v, want the withheld attempt and one retry, both with pid 4242", *respawnPIDs)
	}
}

// TestAutoUpdateOnLaunchProceedsWhenRespawnRetryFails: once the old daemon has
// exited, a failed retry does not stand the launch down — the re-exec'd TUI's
// own EnsureDaemon starts a daemon now that nothing holds the home lock.
func TestAutoUpdateOnLaunchProceedsWhenRespawnRetryFails(t *testing.T) {
	withTestHome(t)
	captureLogs(t)
	seedNewerRelease(t, "1.0.0", "v1.0.1")
	_, respawnPIDs := drainingRestartSeams(t, 4242, nil, errors.New("unit restart failed"))

	got := launchWithTTY(t, nil)

	if !got.proceed || got.calls != 1 {
		t.Fatalf("proceed = %v, re-exec calls = %d; want true and 1 once the old daemon is gone", got.proceed, got.calls)
	}
	if len(*respawnPIDs) != 2 {
		t.Fatalf("respawn calls = %v, want 2", *respawnPIDs)
	}
}
