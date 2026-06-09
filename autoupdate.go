package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

const (
	autoUpdateCheckInterval = 24 * time.Hour
	lastCheckFile           = "last_update_check"
	githubAPILatestRelease  = "https://api.github.com/repos/sachiniyer/agent-factory/releases/latest"
)

// runtimeGOOS is a variable so tests can override the value reported by
// runtime.GOOS to exercise platform-specific branches (e.g. the Windows
// early-return path below).
var runtimeGOOS = runtime.GOOS

// fetchLatestReleaseTagFn is indirected through a variable so tests can inject
// a fake without hitting the network.
var fetchLatestReleaseTagFn = fetchLatestReleaseTag

// autoUpdateInBackground checks for a newer release and applies it silently.
// It runs in a goroutine and never blocks the main program.
func autoUpdateInBackground() {
	go func() {
		if err := autoUpdate(); err != nil {
			log.ErrorLog.Printf("auto-update: %v", err)
		}
	}()
}

func autoUpdate() error {
	// Throttle: only check once per 24 hours
	if !shouldCheck() {
		return nil
	}
	// Record the check up front so failure-prone steps below (network fetch,
	// download, install) still honor the 24-hour throttle. Without this,
	// persistent failures (blocked api.github.com, corp proxy, DNS) would
	// retry on every launch and flood GitHub's rate limit. See issue #459.
	recordCheck()

	latestTag, err := fetchLatestReleaseTagFn()
	if err != nil {
		return fmt.Errorf("failed to fetch latest release: %w", err)
	}

	// Strip leading "v" from tags for comparison
	latestVersion := strings.TrimPrefix(latestTag, "v")
	currentVersion := strings.TrimPrefix(version, "v")
	if !isNewer(latestVersion, currentVersion) {
		return nil
	}

	goos := runtimeGOOS
	goarch := runtime.GOARCH
	if goos == "windows" {
		// Auto-update is not supported on Windows.
		return nil
	}

	log.InfoLog.Printf("auto-update: updating from %s to %s", version, latestVersion)

	downloadURL := fmt.Sprintf("%s/latest/download/agent-factory-%s-%s.tar.gz",
		releaseBaseURL, goos, goarch)

	binary, err := downloadBinaryFn(downloadURL)
	if err != nil {
		return err
	}

	execPath, err := osExecutableFn()
	if err != nil {
		return fmt.Errorf("failed to find executable: %w", err)
	}
	// Resolve symlinks so we replace the real binary, not the symlink
	// pointing to it (e.g. on macOS Homebrew installs).
	resolvedPath, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	if err := config.AtomicWriteFile(resolvedPath, binary, 0755); err != nil {
		return fmt.Errorf("failed to write new binary: %w", err)
	}

	// Same rationale as `af upgrade` (#498): take down the running daemon so
	// the next RPC respawns it from the freshly written binary. Quiet on the
	// no-daemon path since autoUpdate runs in the background on every launch.
	// Pre-#501 daemons don't speak the Shutdown RPC; RequestShutdown falls
	// back to PID-file-based SIGTERM (#504).
	result, shutdownErr := requestDaemonShutdownFn()
	switch {
	case shutdownErr != nil:
		log.WarningLog.Printf("auto-update: updated to %s but failed to restart daemon: %v", latestVersion, shutdownErr)
	case result == daemon.ShutdownViaRPC:
		log.InfoLog.Printf("auto-update: updated to %s and stopped running daemon", latestVersion)
	case result == daemon.ShutdownViaSIGTERM:
		log.InfoLog.Printf("auto-update: updated to %s and stopped pre-#501 running daemon via SIGTERM fallback", latestVersion)
	default:
		log.InfoLog.Printf("auto-update: updated to %s (effective on next launch)", latestVersion)
	}
	if shutdownErr == nil && result != daemon.ShutdownNoDaemon {
		// The daemon hosts task cron schedules (#782); respawn it from the
		// freshly written binary when enabled tasks exist, instead of
		// leaving schedules dark until the next af invocation.
		respawnDaemonForTasksFn()
	}
	return nil
}

// fetchLatestReleaseTag queries the GitHub API for the latest release tag name.
func fetchLatestReleaseTag() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", githubAPILatestRelease, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}
	return release.TagName, nil
}

// isNewer returns true if latestVersion is newer than currentVersion.
// Both should be semver strings like "1.0.16".
func isNewer(latest, current string) bool {
	lParts := parseSemver(latest)
	cParts := parseSemver(current)
	if lParts == nil || cParts == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if lParts[i] > cParts[i] {
			return true
		}
		if lParts[i] < cParts[i] {
			return false
		}
	}
	return false
}

func parseSemver(v string) []int {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return nil
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		nums[i] = n
	}
	return nums
}

func lastCheckPath() string {
	dir, err := config.GetConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, lastCheckFile)
}

func shouldCheck() bool {
	path := lastCheckPath()
	if path == "" {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return true
	}
	if t.After(time.Now()) {
		return true
	}
	return time.Since(t) >= autoUpdateCheckInterval
}

func recordCheck() {
	path := lastCheckPath()
	if path == "" {
		return
	}
	_ = config.AtomicWriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0644)
}
