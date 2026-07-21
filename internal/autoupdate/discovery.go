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
	// AuthToken is optional GitHub API authentication. CI supplies its scoped
	// workflow token here so release discovery does not share the runner IP's
	// tiny unauthenticated quota with unrelated jobs (#2262).
	AuthToken string
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
	if err := getJSON(d.LatestReleaseURL, d.AuthToken, timeout, &release); err != nil {
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
	if err := getJSON(d.ReleasesURL, d.AuthToken, timeout, &releases); err != nil {
		return "", err
	}
	tag := PickLatestReleaseTag(config.UpdateChannelPreview, releases)
	if tag == "" {
		return "", fmt.Errorf("no published release with a parseable version tag found on the preview channel")
	}
	return tag, nil
}

func getJSON(url, authToken string, timeout time.Duration, out any) error {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
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
