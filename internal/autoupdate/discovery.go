// Package autoupdate contains the release discovery, throttle cache, and
// candidate staging primitives shared by interactive and daemon-owned update
// paths. It deliberately does not decide when to check or install a candidate.
package autoupdate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const (
	// ReleaseBaseURL is the GitHub release page used to address versioned
	// release assets.
	ReleaseBaseURL = "https://github.com/sachiniyer/agent-factory/releases"

	// DefaultLatestReleaseAPIURL answers the stable channel. GitHub pins
	// /releases/latest to the newest non-prerelease release.
	DefaultLatestReleaseAPIURL = "https://api.github.com/repos/sachiniyer/agent-factory/releases/latest"
	// DefaultReleasesAPIURL answers the preview channel. One page of 100 is
	// sufficient because releases are created in increasing version order,
	// and the entire page is scanned for the greatest version.
	DefaultReleasesAPIURL = "https://api.github.com/repos/sachiniyer/agent-factory/releases?per_page=100"
)

// Release is the subset of a GitHub release needed to select an update target.
type Release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Discovery resolves release tags from channel-specific GitHub API endpoints.
// Endpoint fields are configurable so callers can exercise the real HTTP path
// against a local server.
type Discovery struct {
	LatestReleaseURL string
	ReleasesURL      string
}

// LatestReleaseTag returns the newest published tag on channel. Stable uses
// /releases/latest so frequent previews cannot push the newest stable release
// off the first page of the release list; preview scans the release list.
func (d Discovery) LatestReleaseTag(channel string, timeout time.Duration) (string, error) {
	if channel == config.UpdateChannelPreview {
		return d.latestPreviewTag(timeout)
	}
	return d.latestStableTag(timeout)
}

func (d Discovery) latestStableTag(timeout time.Duration) (string, error) {
	var release Release
	if err := getJSON(d.LatestReleaseURL, timeout, &release); err != nil {
		return "", err
	}
	parsed := parseVersion(strings.TrimPrefix(release.TagName, "v"))
	if parsed == nil || parsed.preview || release.Prerelease || release.Draft {
		return "", fmt.Errorf("releases/latest returned unusable tag %q for the stable channel", release.TagName)
	}
	return release.TagName, nil
}

func (d Discovery) latestPreviewTag(timeout time.Duration) (string, error) {
	var releases []Release
	if err := getJSON(d.ReleasesURL, timeout, &releases); err != nil {
		return "", err
	}
	tag := PickLatestReleaseTag(config.UpdateChannelPreview, releases)
	if tag == "" {
		return "", unusablePreviewReleases(releases)
	}
	return tag, nil
}

// unusablePreviewReleases says WHICH of the three distinct conditions left the
// preview channel with no usable tag, and how many releases were actually
// observed.
//
// The single sentence this replaces named none of them (#3392 sibling audit),
// and they call for opposite responses: an empty list is a transient API blip
// worth retrying, whereas an unparseable tag means the release tagging itself
// is broken and no amount of retrying will help. Reporting the count matters
// for the same reason — "0 releases" and "12 releases, none parseable" are the
// two ends of that range and read identically today.
func unusablePreviewReleases(releases []Release) error {
	if len(releases) == 0 {
		// AMBIGUOUS, and saying so is the point. getJSON sends no
		// Authorization header, and GitHub does not show draft releases to an
		// unauthenticated caller — so a repository holding nothing but drafts
		// returns [] here and is byte-identical to a transient blip. Claiming
		// "transient, retry" would misdiagnose precisely the state that needs
		// a release published, and the two remedies are opposites: wait,
		// versus publish something (#3392 review).
		//
		// The same asymmetry is why the all-draft branch below is unreachable
		// against the production endpoint: drafts never arrive to be counted.
		// It stays because an authenticated client or an injected list can
		// reach it, and because being wrong there would be silent.
		return fmt.Errorf("the preview channel returned no VISIBLE release (an empty, unauthenticated " +
			"list); af cannot tell which of two causes this is, and they need opposite responses — " +
			"either nothing is published yet (drafts are invisible to an unauthenticated caller, so a " +
			"drafts-only repository looks exactly like this, and a release must be published), or the " +
			"call was a transient blip or rate-limit response and a retry will do. Check whether a " +
			"published preview release exists before assuming a retry is enough")
	}
	published := 0
	for _, release := range releases {
		if !release.Draft {
			published++
		}
	}
	if published == 0 {
		// "Publish one" is only a real remedy if some draft's tag would
		// actually parse once published. A draft tagged `nightly` stays
		// unusable afterwards, so recommending publication there sends the
		// operator after a fix that cannot work and hides the malformed tag
		// (#3392 review). Preview accepts prereleases, so parseability is the
		// only other gate PickLatestReleaseTag applies.
		parseable := 0
		for _, release := range releases {
			if parseVersion(strings.TrimPrefix(release.TagName, "v")) != nil {
				parseable++
			}
		}
		switch {
		case parseable == 0:
			return fmt.Errorf("all %d release(s) on the preview channel are drafts, and none carries a "+
				"parseable version tag; publishing one would not be enough — the tag has to be fixed too",
				len(releases))
		case parseable < len(releases):
			// Tie the remedy to the condition just computed: "publish one"
			// would permit publishing a `nightly` draft, which
			// PickLatestReleaseTag rejects just as firmly once published, so
			// the operator would do the work and see no change (#3392 review).
			return fmt.Errorf("all %d release(s) on the preview channel are drafts; publish one of the %d "+
				"whose tag parses, NOT one of the other %d — an unparseable tag is rejected whether or "+
				"not the release is a draft, so publishing one of those leaves the channel just as unusable",
				len(releases), parseable, len(releases)-parseable)
		default:
			return fmt.Errorf("all %d release(s) on the preview channel are drafts; a draft is never an "+
				"update candidate, so one has to be published before af can move to it", len(releases))
		}
	}
	return fmt.Errorf("none of the %d published release(s) on the preview channel (%d returned in total) "+
		"carries a parseable version tag; the tagging is malformed rather than the release missing, so "+
		"retrying will not help — fix the tag", published, len(releases))
}

// The release-lookup retry policy. The lookup is deliberately unauthenticated
// (#2306 pins that), so a shared runner IP or NAT'd LAN can be over GitHub's
// per-IP quota through no fault of af — and a 403/429 is the only shape that
// has. Those answers are retried; everything else returns on first sight.
const (
	// maxReleaseLookupAttempts bounds the whole loop: one try plus retries.
	maxReleaseLookupAttempts = 4
	// releaseLookupRetryBase is the wait before the first retry when the
	// response carries no timing hint; it doubles each attempt.
	releaseLookupRetryBase = 500 * time.Millisecond
	// maxReleaseLookupWait caps a single wait. These calls run in front of
	// `af upgrade` and the launch path, so a wait measured in minutes is
	// reported to the user, never slept through.
	maxReleaseLookupWait = 20 * time.Second
)

// Injectable for tests: the retry loop's clock and sleep.
var (
	releaseLookupNow   = time.Now
	releaseLookupSleep = time.Sleep
)

// getJSON fetches url into out. timeout bounds the WHOLE lookup — every
// attempt and every wait — because the launch path has a blink's worth of
// patience and `af upgrade` a user's.
//
// A 403 or 429 is a quota answer, not necessarily a final one: the
// unauthenticated budget is per-source-IP, so retrying is the only in-process
// remedy. The wait honours Retry-After and X-RateLimit-Reset up to
// maxReleaseLookupWait, and a server-named wait beyond that cap is reported
// rather than slept — GitHub's primary quota resets on an hourly boundary no
// interactive command can wait out, so the honest answer is the reset time,
// not a clamped nap that retries into a quota that is still empty.
func getJSON(url string, timeout time.Duration, out any) error {
	deadline := releaseLookupNow().Add(timeout)
	var status int
	for attempt := 0; ; attempt++ {
		if attempt > 0 && deadline.Sub(releaseLookupNow()) <= 0 {
			return fmt.Errorf("GitHub API returned %d — still rate-limited when the %s lookup budget ran out; retry `af upgrade` shortly", status, timeout)
		}
		remaining := deadline.Sub(releaseLookupNow())
		var header http.Header
		var err error
		status, header, err = getOnce(url, remaining, out)
		switch {
		case err == nil:
			return nil
		case status != http.StatusForbidden && status != http.StatusTooManyRequests:
			return err
		case attempt+1 >= maxReleaseLookupAttempts:
			return fmt.Errorf("GitHub API returned %d — still rate-limited after %d attempts; "+
				"the release lookup is unauthenticated by design and shares GitHub's per-IP quota, "+
				"so retry `af upgrade` after the limit resets", status, attempt+1)
		}
		now := releaseLookupNow()
		wait, hinted := releaseLookupRetryDelay(header, attempt, now)
		if hinted && wait > maxReleaseLookupWait {
			return fmt.Errorf("GitHub API returned %d — rate-limited with a server-named reset about %s out; retry `af upgrade` after the reset", status, wait.Round(time.Second))
		}
		if wait > maxReleaseLookupWait {
			wait = maxReleaseLookupWait
		}
		if wait >= deadline.Sub(now) {
			return fmt.Errorf("GitHub API returned %d — still rate-limited; retry `af upgrade` shortly", status)
		}
		releaseLookupSleep(wait)
	}
}

// getOnce performs a single GET, returning the status and response headers so
// the retry loop can read quota hints. Non-200 bodies are not decoded.
func getOnce(url string, timeout time.Duration, out any) (int, http.Header, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, resp.Header, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}
	return resp.StatusCode, resp.Header, json.NewDecoder(resp.Body).Decode(out)
}

// releaseLookupRetryDelay decides the wait before retrying a quota-class
// response. Server timing hints win: Retry-After (seconds or HTTP-date), then
// X-RateLimit-Reset (unix epoch). hinted reports whether the wait came from
// the server — a hinted wait beyond maxReleaseLookupWait is surfaced to the
// user rather than slept, so the caller needs to know which case it is in.
func releaseLookupRetryDelay(header http.Header, attempt int, now time.Time) (wait time.Duration, hinted bool) {
	if retryAfter := strings.TrimSpace(header.Get("Retry-After")); retryAfter != "" {
		if secs, err := strconv.Atoi(retryAfter); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
		if when, err := http.ParseTime(retryAfter); err == nil {
			return max(when.Sub(now), 0), true
		}
	}
	if epoch, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Reset")), 10, 64); err == nil {
		return max(time.Unix(epoch, 0).Sub(now), 0), true
	}
	return releaseLookupRetryBase << attempt, false
}

// PickLatestReleaseTag returns the version-newest non-draft release tag on
// channel, or "" when no release qualifies. Stable excludes a release when
// either GitHub or the tag itself marks it as a preview.
func PickLatestReleaseTag(channel string, releases []Release) string {
	best := ""
	for _, release := range releases {
		if release.Draft {
			continue
		}
		version := strings.TrimPrefix(release.TagName, "v")
		parsed := parseVersion(version)
		if parsed == nil {
			continue
		}
		if channel != config.UpdateChannelPreview && (release.Prerelease || parsed.preview) {
			continue
		}
		if best == "" || IsNewer(version, strings.TrimPrefix(best, "v")) {
			best = release.TagName
		}
	}
	return best
}

// DownloadURL returns the version-addressed release asset URL for a platform.
// Preview assets must use their tag because releases/latest/download always
// resolves to a stable release.
func DownloadURL(tag, goos, goarch string) string {
	return fmt.Sprintf("%s/download/%s/%s-%s-%s.tar.gz",
		ReleaseBaseURL, tag, CandidateBinaryName, goos, goarch)
}

// releaseVersion is a parsed release version under the two-channel scheme:
// MAJOR.MINOR.PATCH with an optional "-preview-N" suffix.
type releaseVersion struct {
	nums          [3]int
	preview       bool
	previewNumber int
}

// IsNewer reports whether latest is strictly newer than current. Stable
// releases outrank previews of the same base; previews order by their number.
func IsNewer(latest, current string) bool {
	latestVersion := parseVersion(latest)
	currentVersion := parseVersion(current)
	if latestVersion == nil || currentVersion == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if latestVersion.nums[i] != currentVersion.nums[i] {
			return latestVersion.nums[i] > currentVersion.nums[i]
		}
	}
	if latestVersion.preview != currentVersion.preview {
		return currentVersion.preview
	}
	return latestVersion.preview && latestVersion.previewNumber > currentVersion.previewNumber
}

// IsValidVersion reports whether value follows the release version scheme.
func IsValidVersion(value string) bool {
	return parseVersion(value) != nil
}

// parseVersion parses "X.Y.Z" or "X.Y.Z-preview-N". Other shapes return nil
// so an unrecognized tag is never selected as an update target.
func parseVersion(value string) *releaseVersion {
	core := value
	var preview bool
	var previewNumber int
	if i := strings.IndexByte(value, '-'); i >= 0 {
		core = value[:i]
		number, ok := strings.CutPrefix(value[i+1:], "preview-")
		if !ok {
			return nil
		}
		parsed, err := strconv.Atoi(number)
		if err != nil || parsed < 0 {
			return nil
		}
		preview = true
		previewNumber = parsed
	}

	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return nil
	}
	version := &releaseVersion{preview: preview, previewNumber: previewNumber}
	for i, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return nil
		}
		version.nums[i] = parsed
	}
	return version
}

// NormalizeChannel treats preview as the sole opt-in channel and defaults all
// other values to stable.
func NormalizeChannel(channel string) string {
	if channel == config.UpdateChannelPreview {
		return config.UpdateChannelPreview
	}
	return config.UpdateChannelStable
}
