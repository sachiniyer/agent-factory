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
)

// GitHub API endpoints for release discovery, one per channel (#1041).
// Variables so tests can point them at a local httptest server.
var (
	// githubAPILatestReleaseURL answers the stable channel: GitHub pins
	// /releases/latest to the newest non-prerelease release, with no
	// pagination to overflow. Listing /releases and filtering would break
	// here — with a preview cut every 3 hours, page 1 fills with
	// prereleases within days and the newest stable falls off it, so a
	// stable-channel client would resolve nothing (Greptile review, #1078).
	githubAPILatestReleaseURL = "https://api.github.com/repos/sachiniyer/agent-factory/releases/latest"
	// githubAPIReleasesURL answers the preview channel: /releases/latest
	// never returns prereleases, so previews must come from the list. One
	// page of 100 suffices: under the release scheme versions are only ever
	// created in increasing order (preview z/base move forward, stable
	// releases are validated strictly greater), so the version-newest
	// release is always among the most recently created — and the whole
	// page is scanned for the max rather than trusting item order.
	githubAPIReleasesURL = "https://api.github.com/repos/sachiniyer/agent-factory/releases?per_page=100"
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

	// Auto-update is not supported on Windows, so skip before any network
	// operations — there is nothing to fetch, download, or install (#1002).
	// Still record the check so the 24-hour throttle fires and we don't probe
	// shouldCheck() on every launch (#262). Use the runtimeGOOS seam so tests
	// can exercise this branch without building for Windows.
	if runtimeGOOS == "windows" {
		recordCheck()
		return nil
	}

	// Record the check up front so failure-prone steps below (network fetch,
	// download, install) still honor the 24-hour throttle. Without this,
	// persistent failures (blocked api.github.com, corp proxy, DNS) would
	// retry on every launch and flood GitHub's rate limit. See issue #459.
	recordCheck()

	// Windows already returned above, before any network call (#1002).
	goos := runtimeGOOS
	goarch := runtime.GOARCH

	latestTag, downloadURL, err := latestDownloadURL(updateChannel(), goos, goarch)
	if err != nil {
		return err
	}

	// Strip leading "v" from tags for comparison
	latestVersion := strings.TrimPrefix(latestTag, "v")
	currentVersion := strings.TrimPrefix(version, "v")
	if !isNewer(latestVersion, currentVersion) {
		return nil
	}

	log.InfoLog.Printf("auto-update: updating from %s to %s", version, latestVersion)

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
		// The daemon hosts task schedules and autoyes mode (#782); we just
		// stopped a running one, so respawn it unconditionally from the
		// freshly written binary instead of leaving schedules and autoyes
		// sessions dark until the next af invocation (#813).
		respawnDaemonFn()
	}
	return nil
}

// updateChannel returns the release channel auto-update and `af upgrade`
// follow: config.UpdateChannelStable unless the user opted into
// config.UpdateChannelPreview via the update_channel key (#1041). A config
// that fails to load must never block or redirect an update check, so any
// error falls back to the stable channel.
func updateChannel() string {
	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil {
		return config.UpdateChannelStable
	}
	return cfg.UpdateChannel
}

// latestDownloadURL resolves the newest release on the given channel (#1041)
// and returns its tag plus the tarball URL for goos/goarch. Previews are not
// served by the releases/latest/download redirect (GitHub pins that to the
// newest stable), so the download must address the tag directly.
func latestDownloadURL(channel, goos, goarch string) (tag, url string, err error) {
	tag, err = fetchLatestReleaseTagFn(channel)
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch latest release: %w", err)
	}
	url = fmt.Sprintf("%s/download/%s/agent-factory-%s-%s.tar.gz",
		releaseBaseURL, tag, goos, goarch)
	return tag, url, nil
}

// releaseEntry is the subset of the GitHub release object needed to pick an
// update target.
type releaseEntry struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// fetchLatestReleaseTag queries the GitHub API for the newest release tag on
// the given channel (#1041): the stable channel resolves directly through
// /releases/latest, the preview channel through the release list (see the
// endpoint docs above for why each channel needs its own endpoint).
func fetchLatestReleaseTag(channel string) (string, error) {
	if channel == config.UpdateChannelPreview {
		return fetchLatestPreviewChannelTag()
	}
	return fetchLatestStableTag()
}

// fetchLatestStableTag resolves the newest stable release via
// /releases/latest. The endpoint already excludes prereleases and drafts;
// the shape checks below are a tripwire against a stable release published
// with an off-scheme tag, which must fail loudly rather than become an
// update target.
func fetchLatestStableTag() (string, error) {
	var release releaseEntry
	if err := getGitHubJSON(githubAPILatestReleaseURL, &release); err != nil {
		return "", err
	}
	parsed := parseSemver(strings.TrimPrefix(release.TagName, "v"))
	if parsed == nil || parsed.preview || release.Prerelease || release.Draft {
		return "", fmt.Errorf("releases/latest returned unusable tag %q for the stable channel", release.TagName)
	}
	return release.TagName, nil
}

// fetchLatestPreviewChannelTag resolves the newest release including
// prereleases from the release list.
func fetchLatestPreviewChannelTag() (string, error) {
	var releases []releaseEntry
	if err := getGitHubJSON(githubAPIReleasesURL, &releases); err != nil {
		return "", err
	}
	tag := pickLatestReleaseTag(config.UpdateChannelPreview, releases)
	if tag == "" {
		return "", fmt.Errorf("no published release with a parseable version tag found on the preview channel")
	}
	return tag, nil
}

// getGitHubJSON fetches url from the GitHub API and decodes the JSON
// response into out.
func getGitHubJSON(url string, out any) error {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// pickLatestReleaseTag returns the version-newest non-draft release tag on
// the given channel, or "" when none qualifies. On the stable channel a
// release is skipped when either GitHub's prerelease flag or the tag's
// -preview-N suffix marks it as a preview; the preview channel includes
// everything.
func pickLatestReleaseTag(channel string, releases []releaseEntry) string {
	best := ""
	for _, r := range releases {
		if r.Draft {
			continue
		}
		v := strings.TrimPrefix(r.TagName, "v")
		parsed := parseSemver(v)
		if parsed == nil {
			continue
		}
		if channel != config.UpdateChannelPreview && (r.Prerelease || parsed.preview) {
			continue
		}
		if best == "" || isNewer(v, strings.TrimPrefix(best, "v")) {
			best = r.TagName
		}
	}
	return best
}

// semver is a parsed version under the two-channel scheme (#1041):
// MAJOR.MINOR.PATCH with an optional "-preview-N" prerelease suffix.
type semver struct {
	nums    [3]int
	preview bool
	z       int
}

// isNewer returns true if latest is strictly newer than current under the
// two-channel scheme (#1041). MAJOR.MINOR.PATCH compare numerically; on a
// tie a stable release is newer than any of its previews (standard semver
// precedence), and previews order by their preview number:
// 1.2.0 < 1.2.1-preview-1 < 1.2.1-preview-2 < 1.2.1.
func isNewer(latest, current string) bool {
	l := parseSemver(latest)
	c := parseSemver(current)
	if l == nil || c == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if l.nums[i] != c.nums[i] {
			return l.nums[i] > c.nums[i]
		}
	}
	if l.preview != c.preview {
		// Equal base: the stable release outranks its previews.
		return c.preview
	}
	return l.preview && l.z > c.z
}

// parseSemver parses "X.Y.Z" or "X.Y.Z-preview-N". Any other shape —
// including unknown prerelease suffixes — returns nil so that unrecognized
// tags are never treated as update targets.
func parseSemver(v string) *semver {
	core := v
	var preview bool
	var z int
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core = v[:i]
		numStr, ok := strings.CutPrefix(v[i+1:], "preview-")
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(numStr)
		if err != nil || n < 0 {
			return nil
		}
		preview = true
		z = n
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return nil
	}
	out := &semver{preview: preview, z: z}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out.nums[i] = n
	}
	return out
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
