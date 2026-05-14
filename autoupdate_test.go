package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	aflog "github.com/sachiniyer/agent-factory/log"
)

// captureLogs redirects InfoLog and ErrorLog to in-memory buffers for the
// duration of the test. Returns the buffers; output collected reflects every
// Printf call routed through those loggers after the swap.
func captureLogs(t *testing.T) (info, errLog *bytes.Buffer) {
	t.Helper()
	info = &bytes.Buffer{}
	errLog = &bytes.Buffer{}
	prevInfoOut := aflog.InfoLog.Writer()
	prevErrOut := aflog.ErrorLog.Writer()
	aflog.InfoLog.SetOutput(info)
	aflog.ErrorLog.SetOutput(errLog)
	t.Cleanup(func() {
		aflog.InfoLog.SetOutput(prevInfoOut)
		aflog.ErrorLog.SetOutput(prevErrOut)
	})
	return info, errLog
}

func TestMain(m *testing.M) {
	// autoUpdate() calls log.ErrorLog.Printf, which panics if logging has not
	// been initialized. Initialize once for the whole package test binary.
	aflog.Initialize(false)
	os.Exit(m.Run())
}

// withTestHome points config.GetConfigDir at a temp dir for the duration of
// the test and restores AGENT_FACTORY_HOME on cleanup.
func withTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, had := os.LookupEnv("AGENT_FACTORY_HOME")
	if err := os.Setenv("AGENT_FACTORY_HOME", dir); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv("AGENT_FACTORY_HOME", prev)
		} else {
			os.Unsetenv("AGENT_FACTORY_HOME")
		}
	})
	return dir
}

// TestAutoUpdateWindowsRecordsCheckWhenUpdateAvailable guards against the
// regression tracked in issue #262: on Windows, when a newer release exists,
// the early-return path must still call recordCheck() so the 24-hour throttle
// fires and the GitHub API is not hit on every launch.
func TestAutoUpdateWindowsRecordsCheckWhenUpdateAvailable(t *testing.T) {
	dir := withTestHome(t)
	infoBuf, errBuf := captureLogs(t)

	prevGOOS := runtimeGOOS
	prevFetch := fetchLatestReleaseTagFn
	prevVersion := version
	t.Cleanup(func() {
		runtimeGOOS = prevGOOS
		fetchLatestReleaseTagFn = prevFetch
		version = prevVersion
	})

	runtimeGOOS = "windows"
	version = "1.0.0"
	fetchLatestReleaseTagFn = func() (string, error) { return "v1.0.1", nil }

	if err := autoUpdate(); err != nil {
		t.Fatalf("autoUpdate returned error: %v", err)
	}

	// last_update_check must exist so shouldCheck() returns false.
	if _, err := os.Stat(filepath.Join(dir, lastCheckFile)); err != nil {
		t.Fatalf("expected %s to be written after Windows early-return, got: %v",
			lastCheckFile, err)
	}
	if shouldCheck() {
		t.Fatalf("shouldCheck() returned true after recordCheck(); throttle is broken")
	}
	// Windows skips the actual update, so the misleading "updating from X to Y"
	// line must not appear in any log stream (issue #519).
	if strings.Contains(infoBuf.String(), "updating from") {
		t.Fatalf("InfoLog contained 'updating from' on Windows skip path:\n%s", infoBuf.String())
	}
	if strings.Contains(errBuf.String(), "updating from") {
		t.Fatalf("ErrorLog contained 'updating from' on Windows skip path:\n%s", errBuf.String())
	}
}

// TestAutoUpdateWindowsRecordsCheckWhenNoUpdate exercises the pre-existing
// code path where no update is available, which already recorded the check.
// Included so both Windows branches are covered.
func TestAutoUpdateWindowsRecordsCheckWhenNoUpdate(t *testing.T) {
	dir := withTestHome(t)

	prevGOOS := runtimeGOOS
	prevFetch := fetchLatestReleaseTagFn
	prevVersion := version
	t.Cleanup(func() {
		runtimeGOOS = prevGOOS
		fetchLatestReleaseTagFn = prevFetch
		version = prevVersion
	})

	runtimeGOOS = "windows"
	version = "1.0.1"
	fetchLatestReleaseTagFn = func() (string, error) { return "v1.0.1", nil }

	if err := autoUpdate(); err != nil {
		t.Fatalf("autoUpdate returned error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, lastCheckFile)); err != nil {
		t.Fatalf("expected %s to be written, got: %v", lastCheckFile, err)
	}
}

// TestAutoUpdateRecordsCheckOnFetchFailure guards against the regression
// tracked in issue #459: when fetchLatestReleaseTag fails (blocked API, DNS,
// proxy, rate limit), the 24-hour throttle must still fire so the next launch
// does not retry the network immediately.
func TestAutoUpdateRecordsCheckOnFetchFailure(t *testing.T) {
	dir := withTestHome(t)

	prevFetch := fetchLatestReleaseTagFn
	t.Cleanup(func() {
		fetchLatestReleaseTagFn = prevFetch
	})

	fetchLatestReleaseTagFn = func() (string, error) {
		return "", errors.New("simulated network failure")
	}

	if err := autoUpdate(); err == nil {
		t.Fatalf("autoUpdate returned nil error; expected fetch failure")
	}

	if _, err := os.Stat(filepath.Join(dir, lastCheckFile)); err != nil {
		t.Fatalf("expected %s to be written after fetch failure, got: %v",
			lastCheckFile, err)
	}
	if shouldCheck() {
		t.Fatalf("shouldCheck() returned true after fetch failure; throttle is broken")
	}
}

// TestAutoUpdateRecordsCheckOnDownloadFailure guards against the regression
// tracked in issue #459: when downloadBinary fails (network error mid-update),
// the 24-hour throttle must still fire.
func TestAutoUpdateRecordsCheckOnDownloadFailure(t *testing.T) {
	dir := withTestHome(t)

	prevGOOS := runtimeGOOS
	prevFetch := fetchLatestReleaseTagFn
	prevDownload := downloadBinaryFn
	prevVersion := version
	t.Cleanup(func() {
		runtimeGOOS = prevGOOS
		fetchLatestReleaseTagFn = prevFetch
		downloadBinaryFn = prevDownload
		version = prevVersion
	})

	runtimeGOOS = "linux"
	version = "1.0.0"
	fetchLatestReleaseTagFn = func() (string, error) { return "v1.0.1", nil }
	downloadBinaryFn = func(string) ([]byte, error) {
		return nil, errors.New("simulated download failure")
	}

	if err := autoUpdate(); err == nil {
		t.Fatalf("autoUpdate returned nil error; expected download failure")
	}

	if _, err := os.Stat(filepath.Join(dir, lastCheckFile)); err != nil {
		t.Fatalf("expected %s to be written after download failure, got: %v",
			lastCheckFile, err)
	}
	if shouldCheck() {
		t.Fatalf("shouldCheck() returned true after download failure; throttle is broken")
	}
}

// TestAutoUpdateCallsShutdownAfterBinarySwap guards the #498 fix on the
// background path: when autoUpdate() successfully writes a new binary, it
// must also ask any running daemon to exit so users don't keep running
// the stale image silently for days.
func TestAutoUpdateCallsShutdownAfterBinarySwap(t *testing.T) {
	withTestHome(t)
	infoBuf, errBuf := captureLogs(t)

	tempBin := filepath.Join(t.TempDir(), "agent-factory")
	if err := os.WriteFile(tempBin, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	prevGOOS := runtimeGOOS
	prevFetch := fetchLatestReleaseTagFn
	prevDownload := downloadBinaryFn
	prevVersion := version
	prevExe := osExecutableFn
	prevShutdown := requestDaemonShutdownFn
	t.Cleanup(func() {
		runtimeGOOS = prevGOOS
		fetchLatestReleaseTagFn = prevFetch
		downloadBinaryFn = prevDownload
		version = prevVersion
		osExecutableFn = prevExe
		requestDaemonShutdownFn = prevShutdown
	})

	runtimeGOOS = "linux"
	version = "1.0.0"
	fetchLatestReleaseTagFn = func() (string, error) { return "v1.0.1", nil }
	// Bypass the tarball extract by returning the raw binary directly.
	downloadBinaryFn = func(string) ([]byte, error) { return []byte("new-binary"), nil }
	osExecutableFn = func() (string, error) { return tempBin, nil }
	shutdownCalls := 0
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		shutdownCalls++
		return daemon.ShutdownViaRPC, nil
	}

	if err := autoUpdate(); err != nil {
		t.Fatalf("autoUpdate: %v", err)
	}
	if shutdownCalls != 1 {
		t.Fatalf("expected one Shutdown call, got %d", shutdownCalls)
	}
	got, err := os.ReadFile(tempBin)
	if err != nil {
		t.Fatalf("read upgraded binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("binary contents = %q, want new-binary", got)
	}
	// On non-Windows hosts where an update actually runs, the "updating from"
	// line must be emitted at INFO level (issue #519).
	if !strings.Contains(infoBuf.String(), "updating from 1.0.0 to 1.0.1") {
		t.Fatalf("expected InfoLog to contain 'updating from 1.0.0 to 1.0.1', got:\n%s",
			infoBuf.String())
	}
	if strings.Contains(errBuf.String(), "updating from") {
		t.Fatalf("'updating from' must not be logged at ERROR level, got:\n%s",
			errBuf.String())
	}
}

// TestAutoUpdateSucceedsWhenShutdownErrors verifies that an error from
// RequestShutdown is logged but does not cause autoUpdate to surface a
// failure to the background goroutine — the binary is on disk.
func TestAutoUpdateSucceedsWhenShutdownErrors(t *testing.T) {
	withTestHome(t)

	tempBin := filepath.Join(t.TempDir(), "agent-factory")
	if err := os.WriteFile(tempBin, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	prevGOOS := runtimeGOOS
	prevFetch := fetchLatestReleaseTagFn
	prevDownload := downloadBinaryFn
	prevVersion := version
	prevExe := osExecutableFn
	prevShutdown := requestDaemonShutdownFn
	t.Cleanup(func() {
		runtimeGOOS = prevGOOS
		fetchLatestReleaseTagFn = prevFetch
		downloadBinaryFn = prevDownload
		version = prevVersion
		osExecutableFn = prevExe
		requestDaemonShutdownFn = prevShutdown
	})

	runtimeGOOS = "linux"
	version = "1.0.0"
	fetchLatestReleaseTagFn = func() (string, error) { return "v1.0.1", nil }
	downloadBinaryFn = func(string) ([]byte, error) { return []byte("new-binary"), nil }
	osExecutableFn = func() (string, error) { return tempBin, nil }
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		return daemon.ShutdownNoDaemon, errors.New("simulated rpc failure")
	}

	if err := autoUpdate(); err != nil {
		t.Fatalf("autoUpdate should not fail when Shutdown errors, got: %v", err)
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.9", true},
		{"2.0.0", "1.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"bogus", "1.0.0", false},
	}
	for _, c := range cases {
		if got := isNewer(c.latest, c.current); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}
