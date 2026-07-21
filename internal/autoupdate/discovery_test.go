package autoupdate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
