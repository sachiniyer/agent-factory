package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
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
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: default the whole package into a sandboxed AGENT_FACTORY_HOME so
	// stray config/state/log writes land in a temp dir instead of the
	// developer's real one. Sandbox AFTER the tripwire snapshots the real
	// environment, BEFORE logging resolves its file path.
	restoreHome := testguard.SandboxHome()
	// autoUpdate() calls log.ErrorLog.Printf, which panics if logging has not
	// been initialized. Initialize once for the whole package test binary.
	aflog.Initialize(false)
	code := m.Run()
	aflog.Close()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
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
// fires and the GitHub API is not hit on every launch. It also guards #1002:
// the Windows skip must precede any network call, so the fetch seam here is a
// tripwire that fails the test if invoked.
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
	fetchCalls := 0
	fetchLatestReleaseTagFn = func(string) (string, error) {
		fetchCalls++
		return "v1.0.1", nil
	}

	if err := autoUpdate(); err != nil {
		t.Fatalf("autoUpdate returned error: %v", err)
	}

	// #1002: Windows must skip before touching the network.
	if fetchCalls != 0 {
		t.Fatalf("fetchLatestReleaseTagFn called %d times on Windows; expected 0 (network must be skipped before the GitHub check)", fetchCalls)
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

// TestAutoUpdateWindowsSkipsNetworkRegardlessOfVersion covers the Windows skip
// for an up-to-date build. Since the skip now precedes the fetch (#1002), the
// fetch seam is a tripwire: Windows must record the throttle and return without
// any network call regardless of what version it is running.
func TestAutoUpdateWindowsSkipsNetworkRegardlessOfVersion(t *testing.T) {
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
	fetchLatestReleaseTagFn = func(string) (string, error) {
		t.Fatalf("fetchLatestReleaseTagFn must not be called on Windows (#1002)")
		return "", nil
	}

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

	fetchLatestReleaseTagFn = func(string) (string, error) {
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
	fetchLatestReleaseTagFn = func(string) (string, error) { return "v1.0.1", nil }
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
	prevRespawn := respawnDaemonFn
	respawnCalls := 0
	var respawnPath string
	respawnDaemonFn = func(path string) error {
		respawnCalls++
		respawnPath = path
		return nil
	}
	t.Cleanup(func() { respawnDaemonFn = prevRespawn })

	runtimeGOOS = "linux"
	version = "1.0.0"
	fetchLatestReleaseTagFn = func(string) (string, error) { return "v1.0.1", nil }
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
	if respawnCalls != 1 {
		t.Fatalf("expected the daemon respawn check to run once after shutdown, got %d", respawnCalls)
	}
	if respawnPath != tempBin {
		t.Fatalf("daemon respawn path = %q, want %q", respawnPath, tempBin)
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
	prevRespawn := respawnDaemonFn
	respawnCalls := 0
	respawnDaemonFn = func(string) error {
		respawnCalls++
		return nil
	}
	t.Cleanup(func() { respawnDaemonFn = prevRespawn })

	runtimeGOOS = "linux"
	version = "1.0.0"
	fetchLatestReleaseTagFn = func(string) (string, error) { return "v1.0.1", nil }
	downloadBinaryFn = func(string) ([]byte, error) { return []byte("new-binary"), nil }
	osExecutableFn = func() (string, error) { return tempBin, nil }
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		return daemon.ShutdownNoDaemon, errors.New("simulated rpc failure")
	}

	if err := autoUpdate(); err != nil {
		t.Fatalf("autoUpdate should not fail when Shutdown errors, got: %v", err)
	}
	if respawnCalls != 0 {
		t.Fatalf("no respawn check should run when shutdown errored, got %d", respawnCalls)
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

		// Numeric, not lexical, comparison of the base.
		{"1.0.10", "1.0.9", true},

		// Preview channel ordering (#1041): z is monotonic within a base.
		{"1.2.0-preview-3", "1.2.0-preview-2", true},
		{"1.2.0-preview-2", "1.2.0-preview-3", false},
		{"1.2.0-preview-10", "1.2.0-preview-9", true},
		{"1.2.0-preview-2", "1.2.0-preview-2", false},

		// Stable outranks its own previews (standard semver precedence),
		// never the other way around.
		{"1.2.0", "1.2.0-preview-9", true},
		{"1.2.0-preview-1", "1.2.0", false},

		// A preview off a newer base outranks an older stable, and any
		// newer stable outranks old previews.
		{"1.2.1-preview-1", "1.2.0", true},
		{"1.3.0", "1.2.1-preview-9", true},
		{"1.2.0", "1.2.1-preview-1", false},

		// Migration from the pre-#1041 scheme: the first preview off the
		// last auto-bumped stable must be seen as an update.
		{"1.0.138-preview-1", "1.0.137", true},

		// Unknown or malformed prerelease suffixes are never update targets.
		{"1.2.0-rc-1", "1.0.0", false},
		{"1.2.0-preview-", "1.0.0", false},
		{"1.2.0-preview-x", "1.0.0", false},
		{"1.2.0-preview--3", "1.0.0", false},
		{"1.2.0.4", "1.0.0", false},
	}
	for _, c := range cases {
		if got := isNewer(c.latest, c.current); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestPickLatestReleaseTag(t *testing.T) {
	cases := []struct {
		name     string
		channel  string
		releases []releaseEntry
		want     string
	}{
		{
			name:    "preview channel: newest preview wins over older stable",
			channel: config.UpdateChannelPreview,
			releases: []releaseEntry{
				{TagName: "v1.0.138-preview-2", Prerelease: true},
				{TagName: "v1.0.138-preview-1", Prerelease: true},
				{TagName: "v1.0.137"},
			},
			want: "v1.0.138-preview-2",
		},
		{
			name:    "stable channel: previews are skipped entirely",
			channel: config.UpdateChannelStable,
			releases: []releaseEntry{
				{TagName: "v1.0.138-preview-2", Prerelease: true},
				{TagName: "v1.0.138-preview-1", Prerelease: true},
				{TagName: "v1.0.137"},
			},
			want: "v1.0.137",
		},
		{
			name:    "stable channel: preview-shaped tag is skipped even without the prerelease flag",
			channel: config.UpdateChannelStable,
			releases: []releaseEntry{
				{TagName: "v1.0.138-preview-2"},
				{TagName: "v1.0.137"},
			},
			want: "v1.0.137",
		},
		{
			name:    "stable channel: stable-shaped tag flagged prerelease is skipped",
			channel: config.UpdateChannelStable,
			releases: []releaseEntry{
				{TagName: "v1.0.138", Prerelease: true},
				{TagName: "v1.0.137"},
			},
			want: "v1.0.137",
		},
		{
			name:    "preview channel: fresh stable wins over previews of the old base",
			channel: config.UpdateChannelPreview,
			releases: []releaseEntry{
				{TagName: "v1.1.0"},
				{TagName: "v1.0.138-preview-9", Prerelease: true},
				{TagName: "v1.0.137"},
			},
			want: "v1.1.0",
		},
		{
			name:    "preview channel: promoted base outranks its own previews",
			channel: config.UpdateChannelPreview,
			releases: []releaseEntry{
				{TagName: "v1.0.138-preview-9", Prerelease: true},
				{TagName: "v1.0.138"},
			},
			want: "v1.0.138",
		},
		{
			name:    "drafts and unparseable tags are skipped",
			channel: config.UpdateChannelPreview,
			releases: []releaseEntry{
				{TagName: "v9.9.9", Draft: true},
				{TagName: "nightly"},
				{TagName: "v1.0.137"},
			},
			want: "v1.0.137",
		},
		{
			name:    "API order does not matter",
			channel: config.UpdateChannelPreview,
			releases: []releaseEntry{
				{TagName: "v1.0.137"},
				{TagName: "v1.0.138-preview-10", Prerelease: true},
				{TagName: "v1.0.138-preview-9", Prerelease: true},
			},
			want: "v1.0.138-preview-10",
		},
		{
			name:     "no usable releases",
			channel:  config.UpdateChannelPreview,
			releases: []releaseEntry{{TagName: "junk"}, {TagName: "v1.0.0", Draft: true}},
			want:     "",
		},
		{
			name:    "stable channel: only previews published means no target",
			channel: config.UpdateChannelStable,
			releases: []releaseEntry{
				{TagName: "v1.0.138-preview-1", Prerelease: true},
			},
			want: "",
		},
	}
	for _, c := range cases {
		if got := pickLatestReleaseTag(c.channel, c.releases); got != c.want {
			t.Errorf("%s: pickLatestReleaseTag = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestFetchLatestReleaseTagChannels mirrors the Greptile repro on #1078:
// with a preview cut every 3 hours, page 1 of /releases fills up with 100
// prereleases and the newest stable release is NOT in the list response —
// it is only reachable via /releases/latest. The stable channel (the
// default) must still resolve it; resolving nothing would silently stop
// auto-update for every default-channel user. The preview channel must keep
// resolving the newest preview from the list.
func TestFetchLatestReleaseTagChannels(t *testing.T) {
	previews := make([]releaseEntry, 0, 100)
	for i := 100; i >= 1; i-- {
		previews = append(previews, releaseEntry{
			TagName:    fmt.Sprintf("v1.9.10-preview-%d", i),
			Prerelease: true,
		})
	}
	mux := http.NewServeMux()
	listCalls := 0
	mux.HandleFunc("/releases", func(w http.ResponseWriter, r *http.Request) {
		listCalls++
		if err := json.NewEncoder(w).Encode(previews); err != nil {
			t.Errorf("encode releases list: %v", err)
		}
	})
	latestCalls := 0
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		latestCalls++
		if err := json.NewEncoder(w).Encode(releaseEntry{TagName: "v1.9.9"}); err != nil {
			t.Errorf("encode latest release: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	prevLatestURL := githubAPILatestReleaseURL
	prevListURL := githubAPIReleasesURL
	githubAPILatestReleaseURL = srv.URL + "/releases/latest"
	githubAPIReleasesURL = srv.URL + "/releases?per_page=100"
	t.Cleanup(func() {
		githubAPILatestReleaseURL = prevLatestURL
		githubAPIReleasesURL = prevListURL
	})

	tag, err := fetchLatestReleaseTag(config.UpdateChannelStable)
	if err != nil {
		t.Fatalf("stable channel: %v", err)
	}
	if tag != "v1.9.9" {
		t.Fatalf("stable channel resolved %q, want v1.9.9", tag)
	}
	if listCalls != 0 {
		t.Fatalf("stable channel hit the paginated /releases list %d times; it must use /releases/latest only", listCalls)
	}

	tag, err = fetchLatestReleaseTag(config.UpdateChannelPreview)
	if err != nil {
		t.Fatalf("preview channel: %v", err)
	}
	if tag != "v1.9.10-preview-100" {
		t.Fatalf("preview channel resolved %q, want v1.9.10-preview-100", tag)
	}
	if listCalls != 1 {
		t.Fatalf("preview channel should list /releases exactly once, got %d", listCalls)
	}
	if latestCalls != 1 {
		t.Fatalf("/releases/latest should be hit exactly once (by the stable channel), got %d", latestCalls)
	}
}

// TestFetchLatestStableTagRejectsOffSchemeTag pins the tripwire: a
// /releases/latest response whose tag does not parse as a stable X.Y.Z must
// error rather than become an update target.
func TestFetchLatestStableTagRejectsOffSchemeTag(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(releaseEntry{TagName: "nightly-2026-07-02"}); err != nil {
			t.Errorf("encode latest release: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	prevLatestURL := githubAPILatestReleaseURL
	githubAPILatestReleaseURL = srv.URL + "/releases/latest"
	t.Cleanup(func() { githubAPILatestReleaseURL = prevLatestURL })

	if _, err := fetchLatestReleaseTag(config.UpdateChannelStable); err == nil {
		t.Fatalf("expected error for off-scheme releases/latest tag, got nil")
	}
}

// TestAutoUpdateChannelFromConfig exercises the real config read path:
// update_channel in the global config.json decides which channel the fetch
// seam is asked for — stable when the key is absent (the default), preview
// when opted in.
func TestAutoUpdateChannelFromConfig(t *testing.T) {
	cases := []struct {
		name        string
		configJSON  string // empty = no config.json (first run, materialized defaults)
		wantChannel string
	}{
		{
			name:        "no config defaults to stable",
			wantChannel: config.UpdateChannelStable,
		},
		{
			name:        "config without the key defaults to stable",
			configJSON:  `{"default_program": "claude"}`,
			wantChannel: config.UpdateChannelStable,
		},
		{
			name:        "preview opt-in is honored",
			configJSON:  `{"default_program": "claude", "update_channel": "preview"}`,
			wantChannel: config.UpdateChannelPreview,
		},
		{
			name:        "invalid value falls back to stable",
			configJSON:  `{"default_program": "claude", "update_channel": "nightly"}`,
			wantChannel: config.UpdateChannelStable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := withTestHome(t)
			captureLogs(t)
			if c.configJSON != "" {
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(c.configJSON), 0644); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}

			prevGOOS := runtimeGOOS
			prevFetch := fetchLatestReleaseTagFn
			prevVersion := version
			t.Cleanup(func() {
				runtimeGOOS = prevGOOS
				fetchLatestReleaseTagFn = prevFetch
				version = prevVersion
			})

			runtimeGOOS = "linux"
			version = "1.0.0"
			gotChannel := ""
			fetchLatestReleaseTagFn = func(channel string) (string, error) {
				gotChannel = channel
				// Same version as current: no download or install follows.
				return "v1.0.0", nil
			}

			if err := autoUpdate(); err != nil {
				t.Fatalf("autoUpdate: %v", err)
			}
			if gotChannel != c.wantChannel {
				t.Fatalf("channel = %q, want %q", gotChannel, c.wantChannel)
			}
		})
	}
}

// TestAutoUpdateDownloadsByTag verifies that the update tarball is fetched by
// tag rather than through the releases/latest/download redirect: that
// redirect never serves prereleases, so the preview channel (#1041) would be
// unreachable through it.
func TestAutoUpdateDownloadsByTag(t *testing.T) {
	withTestHome(t)
	captureLogs(t)

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
	prevRespawn := respawnDaemonFn
	t.Cleanup(func() {
		runtimeGOOS = prevGOOS
		fetchLatestReleaseTagFn = prevFetch
		downloadBinaryFn = prevDownload
		version = prevVersion
		osExecutableFn = prevExe
		requestDaemonShutdownFn = prevShutdown
		respawnDaemonFn = prevRespawn
	})

	runtimeGOOS = "linux"
	version = "1.0.137"
	fetchLatestReleaseTagFn = func(string) (string, error) { return "v1.0.138-preview-2", nil }
	var gotURL string
	downloadBinaryFn = func(url string) ([]byte, error) {
		gotURL = url
		return []byte("new-binary"), nil
	}
	osExecutableFn = func() (string, error) { return tempBin, nil }
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		return daemon.ShutdownNoDaemon, nil
	}
	respawnDaemonFn = func(string) error { return nil }

	if err := autoUpdate(); err != nil {
		t.Fatalf("autoUpdate: %v", err)
	}
	wantURL := fmt.Sprintf(
		"https://github.com/sachiniyer/agent-factory/releases/download/v1.0.138-preview-2/agent-factory-linux-%s.tar.gz",
		runtime.GOARCH)
	if gotURL != wantURL {
		t.Fatalf("download URL = %q, want %q", gotURL, wantURL)
	}
}
