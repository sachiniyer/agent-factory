package pushcred

import "testing"

func TestParseOriginAcceptsOnlyHTTPSGitHubOwnerRepo(t *testing.T) {
	for _, tc := range []struct {
		url, ghHost string
		want        Origin
	}{
		{"https://github.com/Owner/Repo.git", "", Origin{Host: "github.com", Path: "owner/repo"}},
		{"https://github.com/owner/repo", "", Origin{Host: "github.com", Path: "owner/repo"}},
		{"https://github.com/owner/repo/", "", Origin{Host: "github.com", Path: "owner/repo"}},
		{"https://x-access-token@github.com/owner/repo.git", "", Origin{Host: "github.com", Path: "owner/repo"}},
		{"https://ghe.example:8443/team/svc.git", "ghe.example", Origin{Host: "ghe.example:8443", Path: "team/svc", Enterprise: true}},
		{"https://tenant.ghe.com/team/svc.git", "tenant.ghe.com", Origin{Host: "tenant.ghe.com", Path: "team/svc"}},
	} {
		got, ok := ParseOrigin(tc.url, tc.ghHost)
		if !ok || got != tc.want {
			t.Errorf("ParseOrigin(%q, %q) = %+v, %v; want %+v", tc.url, tc.ghHost, got, ok, tc.want)
		}
	}
}

func TestParseOriginRefusesEverythingElse(t *testing.T) {
	for _, url := range []string{
		"git@github.com:owner/repo.git",
		"ssh://git@github.com/owner/repo.git",
		"http://github.com/owner/repo.git",
		"file:///srv/repo.git",
		"https://gitlab.com/owner/repo.git",
		"https://github.com.attacker.invalid/owner/repo.git",
		"https://user:secret@github.com/owner/repo.git",
		"https://github.com/owner",
		"https://github.com/owner/repo/extra",
		"https://github.com/owner/../repo",
		"https://github.com//repo",
		"",
	} {
		if got, ok := ParseOrigin(url, ""); ok {
			t.Errorf("ParseOrigin(%q) = %+v; want refused", url, got)
		}
	}
	// A GH_HOST that does not match is not a licence for any other host.
	if _, ok := ParseOrigin("https://ghe.example/team/svc.git", "other.example"); ok {
		t.Error("an enterprise host other than GH_HOST was accepted")
	}
}

func TestOriginMatchesOnlyItsOwnRepository(t *testing.T) {
	o, ok := ParseOrigin("https://github.com/owner/repo.git", "")
	if !ok {
		t.Fatal("origin did not parse")
	}
	for _, tc := range []struct {
		protocol, host, path string
		want                 bool
	}{
		{"https", "github.com", "owner/repo.git", true},
		{"https", "GitHub.com", "Owner/Repo", true},
		{"https", "github.com", "/owner/repo.git/", true},
		{"https", "github.com", "owner/repo.git/info/lfs", true},
		{"https", "github.com", "owner/other.git", false},
		{"https", "github.com", "other/repo.git", false},
		{"https", "github.com", "owner/repo/info/lfs", false},
		{"https", "github.com", "", false}, // useHttpPath off: no path, no answer
		{"https", "github.com", "owner/repo.git/../../other/x", false},
		{"https", "github.com:443", "owner/repo.git", false},
		{"https", "api.github.com", "owner/repo.git", false},
		{"http", "github.com", "owner/repo.git", false},
		{"", "github.com", "owner/repo.git", false},
	} {
		if got := o.Matches(tc.protocol, tc.host, tc.path); got != tc.want {
			t.Errorf("Matches(%q, %q, %q) = %v; want %v", tc.protocol, tc.host, tc.path, got, tc.want)
		}
	}
	if (Origin{}).Matches("https", "", "") {
		t.Error("the zero origin matched an empty request")
	}
}

func TestOriginHostnameDropsThePort(t *testing.T) {
	if got := (Origin{Host: "ghe.example:8443"}).Hostname(); got != "ghe.example" {
		t.Fatalf("Hostname() = %q", got)
	}
}

func TestGitHubHostMatchesTheCloneHelpersTokenNames(t *testing.T) {
	for _, tc := range []struct {
		host, ghHost   string
		ok, enterprise bool
	}{
		{"github.com", "", true, false},
		{"GITHUB.COM", "", true, false},
		{"ghe.example", "ghe.example", true, true},
		{"ghe.example", "https://ghe.example/", true, true},
		{"tenant.ghe.com", "tenant.ghe.com", true, false},
		{"tenant.ghe.com", "", false, false},
		{"gitlab.com", "", false, false},
		{"ghe.example", "", false, false},
	} {
		ok, enterprise := GitHubHost(tc.host, tc.ghHost)
		if ok != tc.ok || enterprise != tc.enterprise {
			t.Errorf("GitHubHost(%q, %q) = %v, %v; want %v, %v", tc.host, tc.ghHost, ok, enterprise, tc.ok, tc.enterprise)
		}
	}
}
