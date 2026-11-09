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

// retryClock stubs the lookup's clock and sleep so the retry loop runs
// instantly and deterministically: the fake now only moves when the loop
// sleeps, and every wait is recorded.
func retryClock(t *testing.T) (sleeps *[]time.Duration, now *time.Time) {
	t.Helper()
	recorded := []time.Duration{}
	fake := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	prevSleep, prevNow := releaseLookupSleep, releaseLookupNow
	releaseLookupSleep = func(d time.Duration) {
		recorded = append(recorded, d)
		fake = fake.Add(d)
	}
	releaseLookupNow = func() time.Time { return fake }
	t.Cleanup(func() {
		releaseLookupSleep = prevSleep
		releaseLookupNow = prevNow
	})
	return &recorded, &fake
}

// TestGetJSONRetriesQuotaResponses is the #6983 product half: an
// unauthenticated release lookup that hits a 403/429 quota answer must retry
// with backoff instead of failing on the first response — a shared runner IP
// can be over budget through no fault of af.
func TestGetJSONRetriesQuotaResponses(t *testing.T) {
	sleeps, _ := retryClock(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if err := json.NewEncoder(w).Encode(Release{TagName: "v1.9.9"}); err != nil {
			t.Errorf("encode latest release: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	discovery := Discovery{LatestReleaseURL: srv.URL}
	tag, err := discovery.LatestReleaseTag(config.UpdateChannelStable, 30*time.Second)
	if err != nil {
		t.Fatalf("403, 403, 200 must resolve, got error: %v", err)
	}
	if tag != "v1.9.9" {
		t.Fatalf("tag = %q, want v1.9.9", tag)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if len(*sleeps) != 2 || (*sleeps)[0] != releaseLookupRetryBase || (*sleeps)[1] != 2*releaseLookupRetryBase {
		t.Fatalf("unhinted retries must back off exponentially, got waits %v", *sleeps)
	}
}

func TestGetJSONQuotaRetriesAreBounded(t *testing.T) {
	retryClock(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	discovery := Discovery{LatestReleaseURL: srv.URL}
	_, err := discovery.LatestReleaseTag(config.UpdateChannelStable, 30*time.Second)
	if err == nil {
		t.Fatal("an always-429 lookup must fail")
	}
	if calls != maxReleaseLookupAttempts {
		t.Fatalf("calls = %d, want the bounded %d", calls, maxReleaseLookupAttempts)
	}
	// The lifecycle harness classifies the failure from this text: keep the
	// "GitHub API returned <status>" contract and say what to do next.
	if !strings.Contains(err.Error(), "GitHub API returned 429") {
		t.Fatalf("error must keep the 'GitHub API returned <status>' contract, got: %v", err)
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Fatalf("a quota-exhausted error must say what to do next, got: %v", err)
	}
}

func TestGetJSONHonoursRetryAfter(t *testing.T) {
	sleeps, _ := retryClock(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if err := json.NewEncoder(w).Encode(Release{TagName: "v1.9.9"}); err != nil {
			t.Errorf("encode latest release: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	discovery := Discovery{LatestReleaseURL: srv.URL}
	if _, err := discovery.LatestReleaseTag(config.UpdateChannelStable, 30*time.Second); err != nil {
		t.Fatalf("Retry-After then 200 must resolve, got: %v", err)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 3*time.Second {
		t.Fatalf("Retry-After must set the wait, got %v", *sleeps)
	}
}

func TestGetJSONHonoursRateLimitReset(t *testing.T) {
	sleeps, now := retryClock(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(7*time.Second).Unix()))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if err := json.NewEncoder(w).Encode(Release{TagName: "v1.9.9"}); err != nil {
			t.Errorf("encode latest release: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	discovery := Discovery{LatestReleaseURL: srv.URL}
	if _, err := discovery.LatestReleaseTag(config.UpdateChannelStable, 30*time.Second); err != nil {
		t.Fatalf("X-RateLimit-Reset then 200 must resolve, got: %v", err)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 7*time.Second {
		t.Fatalf("X-RateLimit-Reset must set the wait, got %v", *sleeps)
	}
}

// A server-named reset beyond the cap must fail fast naming the reset, not
// clamp to a shorter nap that retries into a still-empty quota.
func TestGetJSONRefusesABeyondCapReset(t *testing.T) {
	_, now := retryClock(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(40*time.Minute).Unix()))
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	discovery := Discovery{LatestReleaseURL: srv.URL}
	_, err := discovery.LatestReleaseTag(config.UpdateChannelStable, 30*time.Second)
	if err == nil {
		t.Fatal("a reset 40m out must not be waited through")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 — an unwaitable reset must fail without retrying", calls)
	}
	if !strings.Contains(err.Error(), "reset") {
		t.Fatalf("the error must tell the user about the reset, got: %v", err)
	}
}

// A wait that cannot fit inside the caller's budget must not be slept: the
// launch path's lookup cannot stall a blink longer than it did before.
func TestGetJSONNeverWaitsPastTheBudget(t *testing.T) {
	retryClock(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	discovery := Discovery{LatestReleaseURL: srv.URL}
	// 3s budget: the first Retry-After fits (2<3), the second does not.
	_, err := discovery.LatestReleaseTag(config.UpdateChannelStable, 3*time.Second)
	if err == nil {
		t.Fatal("persistent 403s inside a 3s budget must fail")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 — a wait past the budget must end the loop", calls)
	}
}

func TestGetJSONDoesNotRetryOtherStatuses(t *testing.T) {
	retryClock(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	discovery := Discovery{LatestReleaseURL: srv.URL}
	if _, err := discovery.LatestReleaseTag(config.UpdateChannelStable, 30*time.Second); err == nil {
		t.Fatal("a 404 must remain an error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 — a 404 is a broken release, not a quota answer", calls)
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
			// An unauthenticated list hides drafts, so [] cannot be called
			// transient: a drafts-only repository is byte-identical to a blip
			// and needs the opposite response (#3392 review).
			name:     "empty list is reported as ambiguous, not as transient",
			releases: []Release{},
			want: []string{
				"no VISIBLE release", "unauthenticated",
				"nothing is published yet", "transient blip",
			},
			notWant: "",
		},
		{
			name: "all drafts is named as drafts, with the count",
			releases: []Release{
				{TagName: "v1.9.10-preview-1", Draft: true},
				{TagName: "v1.9.11-preview-1", Draft: true},
			},
			want: []string{"all 2 release(s)", "drafts", "published"},
			// It must not claim the TAGS are broken: these parse fine, so
			// publishing one really is the whole remedy here.
			notWant: "tag has to be fixed too",
		},
		{
			// The review case (#3392 review): drafts whose tags would STILL
			// not parse after publication. Recommending "publish one" there
			// sends the operator after a fix that cannot work.
			name: "all drafts with unparseable tags must not promise publication is enough",
			releases: []Release{
				{TagName: "nightly", Draft: true},
				{TagName: "latest", Draft: true},
			},
			want:    []string{"all 2 release(s)", "drafts", "parseable version tag", "tag has to be fixed too"},
			notWant: "empty release list",
		},
		{
			// #3392 review: "publish one" permits publishing `nightly`, which
			// PickLatestReleaseTag rejects just as firmly once published.
			name: "mixed drafts must say WHICH draft to publish",
			releases: []Release{
				{TagName: "v1.9.10-preview-1", Draft: true},
				{TagName: "nightly", Draft: true},
			},
			want:    []string{"all 2 release(s)", "publish one of the 1", "NOT one of the other 1"},
			notWant: "tag has to be fixed too",
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
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("message must not blame the wrong condition %q: %q", tc.notWant, err.Error())
			}
		})
	}
}
