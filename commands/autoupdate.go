package commands

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
	autoUpdateEnv           = "AGENT_FACTORY_AUTO_UPDATE"
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
func autoUpdateInBackground(cfg *config.Config) {
	if !autoUpdateEnabled(cfg) {
		log.InfoLog.Printf("auto-update: disabled")
		return
	}
	channel := config.UpdateChannelStable
	if cfg != nil {
		channel = cfg.UpdateChannel
	}
	go func() {
		if err := autoUpdateForChannel(channel); err != nil {
			log.ErrorLog.Printf("auto-update: %v", err)
		}
	}()
}

func autoUpdate() error {
	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil {
		if !autoUpdateEnabled(nil) {
			return nil
		}
		return autoUpdateForChannel(config.UpdateChannelStable)
	}
	if !autoUpdateEnabled(cfg) {
		return nil
	}
	return autoUpdateForChannel(cfg.UpdateChannel)
}

func autoUpdateForChannel(channel string) error {
	channel = normalizeUpdateChannel(channel)
	// Auto-update is not supported on Windows, so skip before any network
	// operations — there is nothing to fetch, download, or install (#1002).
	// Treat the platform skip as a successful startup decision so Windows
	// launch loops do not re-enter this branch every time (#262).
	if runtimeGOOS == "windows" {
		return withUpdateCheckLock(func(cache *updateCheckCache, now time.Time) error {
			currentVersion := strings.TrimPrefix(version, "v")
			if !updateCheckDue(cache, channel, currentVersion, now) {
				return nil
			}
			return recordCheckLocked(cache, channel, "", currentVersion, now)
		})
	}

	// Windows already returned above, before any network call (#1002).
	goos := runtimeGOOS
	goarch := runtime.GOARCH

	return withUpdateCheckLock(func(cache *updateCheckCache, now time.Time) error {
		currentVersion := strings.TrimPrefix(version, "v")
		if !updateCheckDue(cache, channel, currentVersion, now) {
			return nil
		}

		latestTag, downloadURL, err := latestDownloadURL(channel, goos, goarch)
		if err != nil {
			return err
		}

		// Strip leading "v" from tags for comparison.
		latestVersion := strings.TrimPrefix(latestTag, "v")
		if !isNewer(latestVersion, currentVersion) {
			return recordCheckLocked(cache, channel, latestTag, currentVersion, now)
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

		// Same rationale as `af upgrade` (#498/#1386): restart the running daemon
		// immediately from the freshly written binary. Quiet on the no-daemon path
		// since autoUpdate runs in the background on every launch. Pre-#501
		// daemons don't speak the Shutdown RPC; RequestShutdown falls back to
		// PID-file-based SIGTERM (#504).
		result, restartErr := restartDaemonFromPath(resolvedPath)
		switch {
		case restartErr != nil:
			log.WarningLog.Printf("auto-update: updated to %s but failed to restart daemon: %v", latestVersion, restartErr)
		case result == daemon.ShutdownViaRPC:
			log.InfoLog.Printf("auto-update: updated to %s and restarted running daemon", latestVersion)
		case result == daemon.ShutdownViaSIGTERM:
			log.InfoLog.Printf("auto-update: updated to %s and restarted pre-#501 running daemon via SIGTERM fallback", latestVersion)
		default:
			log.InfoLog.Printf("auto-update: updated to %s (effective on next launch)", latestVersion)
		}
		return recordCheckLocked(cache, channel, latestTag, latestVersion, now)
	})
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
	return normalizeUpdateChannel(cfg.UpdateChannel)
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

func autoUpdateEnabled(cfg *config.Config) bool {
	enabled := true
	if cfg != nil {
		enabled = cfg.AutoUpdate
	}
	raw, ok := os.LookupEnv(autoUpdateEnv)
	if !ok {
		return enabled
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	case "0", "false", "f", "no", "n", "off":
		return false
	case "":
		return enabled
	default:
		log.WarningLog.Printf("auto-update: ignoring invalid %s=%q (expected true/false, 1/0, yes/no, on/off)", autoUpdateEnv, raw)
		return enabled
	}
}

func normalizeUpdateChannel(channel string) string {
	if channel == config.UpdateChannelPreview {
		return config.UpdateChannelPreview
	}
	return config.UpdateChannelStable
}

type updateCheckCache struct {
	path          string
	SchemaVersion int                          `json:"schema_version,omitempty"`
	LastChannel   string                       `json:"last_channel,omitempty"`
	Channels      map[string]updateCheckRecord `json:"channels,omitempty"`
}

type updateCheckRecord struct {
	CheckedAt      time.Time `json:"checked_at"`
	LastSeenTag    string    `json:"last_seen_tag,omitempty"`
	CurrentVersion string    `json:"current_version,omitempty"`
}

func withUpdateCheckLock(fn func(cache *updateCheckCache, now time.Time) error) error {
	path := lastCheckPath()
	if path == "" {
		return fn(&updateCheckCache{}, time.Now().UTC())
	}
	return config.WithFileLock(path, func() error {
		cache := readUpdateCheckCache(path)
		return fn(cache, time.Now().UTC())
	})
}

func readUpdateCheckCache(path string) *updateCheckCache {
	cache := &updateCheckCache{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return cache
	}
	if err := json.Unmarshal(data, cache); err == nil {
		cache.path = path
		if cache.Channels == nil {
			cache.Channels = map[string]updateCheckRecord{}
		}
		return cache
	}
	// Legacy cache files were a bare RFC3339 timestamp with no channel. Treat
	// them as stale so the first new build re-checks and writes channel-aware
	// metadata instead of letting a prior stable check suppress preview (#1466).
	return cache
}

func shouldCheck(channel string) bool {
	path := lastCheckPath()
	if path == "" {
		return true
	}
	cache := readUpdateCheckCache(path)
	return updateCheckDue(cache, normalizeUpdateChannel(channel), strings.TrimPrefix(version, "v"), time.Now().UTC())
}

func updateCheckDue(cache *updateCheckCache, channel, currentVersion string, now time.Time) bool {
	channel = normalizeUpdateChannel(channel)
	if cache == nil {
		return true
	}
	if cache.LastChannel != "" && cache.LastChannel != channel {
		return true
	}
	rec, ok := cache.Channels[channel]
	if !ok {
		return true
	}
	if rec.CurrentVersion != "" && rec.CurrentVersion != currentVersion {
		return true
	}
	if rec.CheckedAt.IsZero() || rec.CheckedAt.After(now) {
		return true
	}
	return now.Sub(rec.CheckedAt) >= autoUpdateCheckInterval
}

func recordCheck(channel, lastSeenTag, currentVersion string) {
	path := lastCheckPath()
	if path == "" {
		return
	}
	_ = config.WithFileLock(path, func() error {
		return recordCheckLocked(readUpdateCheckCache(path), channel, lastSeenTag, currentVersion, time.Now().UTC())
	})
}

func recordCheckLocked(cache *updateCheckCache, channel, lastSeenTag, currentVersion string, now time.Time) error {
	if cache == nil {
		cache = &updateCheckCache{}
	}
	if cache.path == "" {
		return nil
	}
	channel = normalizeUpdateChannel(channel)
	if cache.Channels == nil {
		cache.Channels = map[string]updateCheckRecord{}
	}
	cache.SchemaVersion = 1
	cache.LastChannel = channel
	cache.Channels[channel] = updateCheckRecord{
		CheckedAt:      now.UTC(),
		LastSeenTag:    lastSeenTag,
		CurrentVersion: strings.TrimPrefix(currentVersion, "v"),
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return config.AtomicWriteFile(cache.path, data, 0644)
}
