package autoupdate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestDiscoveryUsesChannelSpecificEndpoints(t *testing.T) {
	previews := make([]Release, 0, 100)
	for i := 100; i >= 1; i-- {
		previews = append(previews, Release{
			TagName:    fmt.Sprintf("v1.9.10-preview-%d", i),
			Prerelease: true,
		})
	}

	listCalls := 0
	latestCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/releases", func(w http.ResponseWriter, _ *http.Request) {
		listCalls++
		if err := json.NewEncoder(w).Encode(previews); err != nil {
			t.Errorf("encode releases: %v", err)
		}
	})
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		latestCalls++
		if err := json.NewEncoder(w).Encode(Release{TagName: "v1.9.9"}); err != nil {
			t.Errorf("encode latest release: %v", err)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	discovery := Discovery{
		LatestReleaseURL: server.URL + "/releases/latest",
		ReleasesURL:      server.URL + "/releases?per_page=100",
	}
	stable, err := discovery.LatestReleaseTag(config.UpdateChannelStable, time.Second)
	if err != nil {
		t.Fatalf("stable discovery: %v", err)
	}
	if stable != "v1.9.9" {
		t.Fatalf("stable tag = %q, want v1.9.9", stable)
	}
	preview, err := discovery.LatestReleaseTag(config.UpdateChannelPreview, time.Second)
	if err != nil {
		t.Fatalf("preview discovery: %v", err)
	}
	if preview != "v1.9.10-preview-100" {
		t.Fatalf("preview tag = %q, want v1.9.10-preview-100", preview)
	}
	if latestCalls != 1 || listCalls != 1 {
		t.Fatalf("endpoint calls latest/list = %d/%d, want 1/1", latestCalls, listCalls)
	}
}

func TestVersionOrderingAndReleaseSelection(t *testing.T) {
	comparisons := []struct {
		latest  string
		current string
		want    bool
	}{
		{latest: "1.0.10", current: "1.0.9", want: true},
		{latest: "1.2.0-preview-10", current: "1.2.0-preview-9", want: true},
		{latest: "1.2.0", current: "1.2.0-preview-10", want: true},
		{latest: "1.2.0-preview-1", current: "1.2.0", want: false},
		{latest: "1.2.0-rc-1", current: "1.0.0", want: false},
	}
	for _, test := range comparisons {
		if got := IsNewer(test.latest, test.current); got != test.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", test.latest, test.current, got, test.want)
		}
	}

	releases := []Release{
		{TagName: "v2.0.0", Draft: true},
		{TagName: "v1.4.0-preview-2", Prerelease: true},
		{TagName: "v1.4.0-preview-1", Prerelease: true},
		{TagName: "v1.3.9"},
	}
	if got := PickLatestReleaseTag(config.UpdateChannelStable, releases); got != "v1.3.9" {
		t.Fatalf("stable selection = %q, want v1.3.9", got)
	}
	if got := PickLatestReleaseTag(config.UpdateChannelPreview, releases); got != "v1.4.0-preview-2" {
		t.Fatalf("preview selection = %q, want v1.4.0-preview-2", got)
	}
}

func TestDownloadURLIsTagAddressed(t *testing.T) {
	got := DownloadURL("v1.2.3-preview-4", "linux", "amd64")
	want := ReleaseBaseURL + "/download/v1.2.3-preview-4/agent-factory-linux-amd64.tar.gz"
	if got != want {
		t.Fatalf("DownloadURL = %q, want %q", got, want)
	}
}

// TestPreviewChannelNamesWhichConditionFailed is the #3392 sibling case, raised
// on the issue: the preview channel collapsed three causes into one sentence
// ("no published release with a parseable version tag found on the preview
// channel"), and they call for opposite responses — an empty list is a
// transient blip worth retrying, an unparseable tag means the tagging is broken
// and retrying cannot help. Each must be identifiable, and each must report how
// many releases were actually observed.
func TestPreviewChannelNamesWhichConditionFailed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		releases []Release
		want     []string
		notWant  string
	}{
		{
			name:     "empty list is transient and says so",
			releases: []Release{},
			want:     []string{"empty release list", "0 releases", "retry"},
			notWant:  "parseable",
		},
		{
			name: "all drafts is named as drafts, with the count",
			releases: []Release{
				{TagName: "v1.9.10-preview-1", Draft: true},
				{TagName: "v1.9.11-preview-1", Draft: true},
			},
			want:    []string{"all 2 release(s)", "drafts"},
			notWant: "parseable",
		},
		{
			name: "unparseable tags are called malformed, not missing",
			releases: []Release{
				{TagName: "nightly"},
				{TagName: "latest"},
				{TagName: "v1.9.10-preview-1", Draft: true},
			},
			want:    []string{"2 published release(s)", "3 returned in total", "parseable", "retrying will not help"},
			notWant: "empty release list",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releases := tc.releases
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if err := json.NewEncoder(w).Encode(releases); err != nil {
					t.Errorf("encode releases: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			discovery := Discovery{ReleasesURL: server.URL}
			_, err := discovery.LatestReleaseTag(config.UpdateChannelPreview, time.Second)
			if err == nil {
				t.Fatal("a preview channel with no usable tag must be an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message must identify the condition.\n got: %q\nwant it to contain: %q", err.Error(), want)
				}
			}
			if strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("message must not blame the wrong condition %q: %q", tc.notWant, err.Error())
			}
		})
	}
}
