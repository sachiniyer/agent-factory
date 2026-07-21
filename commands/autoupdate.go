package commands

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/autoupdate"
	"github.com/sachiniyer/agent-factory/log"
)

// Timeouts differ by who is waiting. The launch check runs synchronously in
// front of the TUI, so it is bounded hard enough that a black-holed network
// costs a blink rather than a stall; `af upgrade` was asked for explicitly
// and can afford to be patient.
var (
	// autoUpdateCheckTimeout bounds the launch-path release lookup.
	autoUpdateCheckTimeout = 2 * time.Second
	// manualCheckTimeout bounds the release lookup for `af upgrade`.
	manualCheckTimeout = 10 * time.Second
	// autoUpdateDownloadBudget bounds the launch-path download. Release
	// tarballs are a few MB, so this clears a slow link with room to spare
	// while capping how long a launch can stall behind a crawling transfer.
	autoUpdateDownloadBudget = 45 * time.Second
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
	githubAPILatestReleaseURL = autoupdate.DefaultLatestReleaseAPIURL
	// githubAPIReleasesURL answers the preview channel: /releases/latest
	// never returns prereleases, so previews must come from the list. One
	// page of 100 suffices: under the release scheme versions are only ever
	// created in increasing order (preview z/base move forward, stable
	// releases are validated strictly greater), so the version-newest
	// release is always among the most recently created — and the whole
	// page is scanned for the max rather than trusting item order.
	githubAPIReleasesURL = autoupdate.DefaultReleasesAPIURL
)

// runtimeGOOS is a variable so tests can override the value reported by
// runtime.GOOS to exercise platform-specific branches (e.g. the Windows
// early-return path below).
var runtimeGOOS = runtime.GOOS

// fetchLatestReleaseTagFn is indirected through a variable so tests can inject
// a fake without hitting the network.
var fetchLatestReleaseTagFn = fetchLatestReleaseTag

// autoUpdateForChannel runs one auto-update cycle for channel: throttle, then
// check, then download and install anything newer. It reports the version it
// installed, or "" when it skipped, found nothing newer, or failed — so the
// launch path knows whether there is a new binary worth re-execing into.
//
// checkTimeout bounds the release lookup and downloadBudget the tarball fetch;
// both are parameters because `af upgrade` and the launch path have very
// different patience (see the timeout vars above).
//
// The throttle window covers failures as well as successes. This has been
// argued both ways: #459 throttled failures to stop a host that can't reach
// api.github.com from burning its 60-req/hr unauthenticated budget on every
// launch, then #1466 un-throttled them so a blocked API couldn't hide behind
// the then-24-hour window for a full day. Two things settle it here. The
// window is now 6h, so a swallowed transient failure costs hours, not a day.
// And the check moved onto the launch's critical path — an un-throttled
// failure no longer costs a background goroutine, it costs every single `af`
// the full checkTimeout before the TUI opens. Retrying that eagerly is worse
// for the user than being briefly behind, so #459's position wins.
func autoUpdateForChannel(channel string, checkTimeout, downloadBudget time.Duration) (installed string, err error) {
	channel = normalizeUpdateChannel(channel)
	// Auto-update is not supported on Windows, so skip before any network
	// operations — there is nothing to fetch, download, or install (#1002).
	// Treat the platform skip as a successful startup decision so Windows
	// launch loops do not re-enter this branch every time (#262).
	if runtimeGOOS == "windows" {
		return "", withUpdateCheckLock(func(cache *updateCheckCache, now time.Time) error {
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

	err = withUpdateCheckLock(func(cache *updateCheckCache, now time.Time) error {
		currentVersion := strings.TrimPrefix(version, "v")
		if !updateCheckDue(cache, channel, currentVersion, now) {
			return nil
		}
		// Close the throttle window on every outcome below, successful or
		// not: the record says "we tried at `now`", not "we succeeded".
		// Recording the *current* version on a failure keeps the entry
		// truthful — nothing was installed — while still suppressing the
		// retry until the window rolls over.
		throttleFailure := func(tag string) {
			if recErr := recordCheckLocked(cache, channel, tag, currentVersion, now); recErr != nil {
				log.WarningLog.Printf("auto-update: failed to record check: %v", recErr)
			}
		}

		latestTag, downloadURL, err := latestDownloadURL(channel, goos, goarch, checkTimeout)
		if err != nil {
			throttleFailure("")
			return err
		}

		// Strip leading "v" from tags for comparison.
		latestVersion := strings.TrimPrefix(latestTag, "v")
		// Never downgrade: a preview user switching back to stable resolves
		// an older tag here, and installing it would roll them backwards.
		if !isNewer(latestVersion, currentVersion) {
			return recordCheckLocked(cache, channel, latestTag, currentVersion, now)
		}

		log.InfoLog.Printf("auto-update: updating from %s to %s", version, latestVersion)
		// Say why the launch is pausing before the download stalls it for a
		// few seconds. An unexplained wait in front of the TUI reads as a
		// hang; this line is the difference between "af is broken" and "af is
		// updating".
		autoUpdateNotice("Updating af to v%s…\n", latestVersion)

		binary, err := downloadBinaryFn(downloadURL, downloadBudget)
		if err != nil {
			throttleFailure(latestTag)
			return err
		}

		execPath, err := osExecutableFn()
		if err != nil {
			throttleFailure(latestTag)
			return fmt.Errorf("failed to find executable: %w", err)
		}
		// Resolve symlinks so we replace the real binary, not the symlink
		// pointing to it (e.g. on macOS Homebrew installs).
		resolvedPath, err := filepath.EvalSymlinks(execPath)
		if err != nil {
			throttleFailure(latestTag)
			return fmt.Errorf("failed to resolve executable path: %w", err)
		}

		if err := config.AtomicWriteFile(resolvedPath, binary, 0755); err != nil {
			throttleFailure(latestTag)
			return fmt.Errorf("failed to write new binary: %w", err)
		}

		unitRefreshErr := refreshAutostartUnitFn()
		if unitRefreshErr != nil {
			// The new binary is already on disk, but stopping through a stale
			// control-group unit is worse than leaving the old daemon running:
			// it can take every daemon-spawned tmux pane with it (#2176).
			log.WarningLog.Printf("auto-update: updated to %s but left the running daemon alone because its autostart unit could not be made restart-safe: %v; run af daemon install, then af daemon restart", latestVersion, unitRefreshErr)
		}

		// Same rationale as `af upgrade` (#498/#1386): restart the running daemon
		// immediately from the freshly written binary. Quiet on the no-daemon path
		// since this runs on every launch. Pre-#501 daemons don't speak the
		// Shutdown RPC; RequestShutdown falls back to PID-file-based SIGTERM (#504).
		if unitRefreshErr == nil {
			result, restartErr := restartDaemonFromPath(resolvedPath)
			switch {
			case restartErr != nil:
				log.WarningLog.Printf("auto-update: updated to %s but failed to restart daemon: %v", latestVersion, restartErr)
			case result == daemon.ShutdownViaRPC:
				log.InfoLog.Printf("auto-update: updated to %s and restarted running daemon", latestVersion)
			case result == daemon.ShutdownViaSIGTERM:
				log.InfoLog.Printf("auto-update: updated to %s and restarted pre-#501 running daemon via SIGTERM fallback", latestVersion)
			default:
				log.InfoLog.Printf("auto-update: updated to %s", latestVersion)
			}
		}
		// The binary on disk is now latestVersion; record it as such so the
		// re-exec'd process sees a fresh, matching entry and skips its own
		// check instead of looping back through this path.
		installed = latestVersion
		return recordCheckLocked(cache, channel, latestTag, latestVersion, now)
	})
	// installed is reported even alongside an error: the only way to reach
	// here with both set is a successful install whose bookkeeping write
	// failed, and the new binary is on disk either way. Swallowing that would
	// strand the user on the old image for no reason.
	return installed, err
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
func latestDownloadURL(channel, goos, goarch string, timeout time.Duration) (tag, url string, err error) {
	tag, err = fetchLatestReleaseTagFn(channel, timeout)
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch latest release: %w", err)
	}
	url = autoupdate.DownloadURL(tag, goos, goarch)
	return tag, url, nil
}

// releaseEntry is the subset of the GitHub release object needed to pick an
// update target.
type releaseEntry = autoupdate.Release

// fetchLatestReleaseTag queries the GitHub API for the newest release tag on
// the given channel (#1041): the stable channel resolves directly through
// /releases/latest, the preview channel through the release list (see the
// endpoint docs above for why each channel needs its own endpoint).
func fetchLatestReleaseTag(channel string, timeout time.Duration) (string, error) {
	discovery := autoupdate.Discovery{
		LatestReleaseURL: githubAPILatestReleaseURL,
		ReleasesURL:      githubAPIReleasesURL,
	}
	return discovery.LatestReleaseTag(channel, timeout)
}

// pickLatestReleaseTag returns the version-newest non-draft release tag on
// the given channel, or "" when none qualifies. On the stable channel a
// release is skipped when either GitHub's prerelease flag or the tag's
// -preview-N suffix marks it as a preview; the preview channel includes
// everything.
func pickLatestReleaseTag(channel string, releases []releaseEntry) string {
	return autoupdate.PickLatestReleaseTag(channel, releases)
}

// isNewer returns true if latest is strictly newer than current under the
// two-channel scheme (#1041). MAJOR.MINOR.PATCH compare numerically; on a
// tie a stable release is newer than any of its previews (standard semver
// precedence), and previews order by their preview number:
// 1.2.0 < 1.2.1-preview-1 < 1.2.1-preview-2 < 1.2.1.
func isNewer(latest, current string) bool {
	return autoupdate.IsNewer(latest, current)
}

func lastCheckPath() string {
	return autoupdate.CheckCachePath()
}

func autoUpdateEnabled(cfg *config.Config) bool {
	return autoupdate.Enabled(cfg)
}

func normalizeUpdateChannel(channel string) string {
	return autoupdate.NormalizeChannel(channel)
}

type updateCheckCache = autoupdate.CheckCache

// withUpdateCheckLock runs fn against the throttle cache under the update
// lock. The lock is taken without waiting: when another `af` is already
// mid-check its peers skip rather than queue, because a second launch has
// nothing to gain by waiting out someone else's download — and everything to
// lose, since this now sits in front of the TUI and a blocking wait would read
// as a hang for as long as that download takes.
func withUpdateCheckLock(fn func(cache *updateCheckCache, now time.Time) error) error {
	acquired, err := autoupdate.TryWithCheckCache(lastCheckPath(), fn)
	if err != nil {
		return err
	}
	if !acquired {
		log.InfoLog.Printf("auto-update: another af holds the update lock; skipping this launch")
	}
	return nil
}

func updateCheckDue(cache *updateCheckCache, channel, currentVersion string, now time.Time) bool {
	return cache.Due(channel, currentVersion, now)
}

func recordCheckLocked(cache *updateCheckCache, channel, lastSeenTag, currentVersion string, now time.Time) error {
	return cache.Record(channel, lastSeenTag, currentVersion, now)
}
