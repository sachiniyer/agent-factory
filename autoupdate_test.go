package main

import (
	"os"
	"path/filepath"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
)

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
