package session

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestGitHubEnvironmentCredentialIsOriginScopedAndValueFree(t *testing.T) {
	key, helper, ok := githubEnvironmentCredential("https://github.com/example/project.git")
	if !ok {
		t.Fatal("HTTPS GitHub origin did not produce a credential helper")
	}
	if key != "credential.https://github.com.helper" {
		t.Fatalf("credential key = %q", key)
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if !strings.Contains(helper, name) {
			t.Fatalf("credential helper omitted %s", name)
		}
	}
	if strings.Contains(helper, "example/project") {
		t.Fatal("credential helper should scope to the host, not persist repository data")
	}
}

func TestGitHubEnvironmentCredentialRejectsNonGitHubAndNonHTTPSOrigins(t *testing.T) {
	t.Setenv("GH_HOST", "github.enterprise.test")
	for _, origin := range []string{
		"git@github.com:example/project.git",
		"file:///tmp/project.git",
		"http://github.com/example/project.git",
		"https://gitlab.example/example/project.git",
		"https://github.enterprise.test.attacker.invalid/example/project.git",
	} {
		if _, _, ok := githubEnvironmentCredential(origin); ok {
			t.Fatalf("untrusted origin %q unexpectedly produced a GitHub token helper", origin)
		}
	}
}

func TestGitHubEnvironmentCredentialAllowsConfiguredEnterpriseHost(t *testing.T) {
	t.Setenv("GH_HOST", "github.enterprise.test")
	key, helper, ok := githubEnvironmentCredential("https://github.enterprise.test/example/project.git")
	if !ok {
		t.Fatal("configured HTTPS GitHub Enterprise origin did not produce a credential helper")
	}
	if key != "credential.https://github.enterprise.test.helper" {
		t.Fatalf("credential key = %q", key)
	}
	if !strings.Contains(helper, "GH_ENTERPRISE_TOKEN") {
		t.Fatal("enterprise credential helper omitted the enterprise token name")
	}
	for _, publicName := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(helper, publicName) {
			t.Fatalf("enterprise credential helper fell back to public token name %s", publicName)
		}
	}
}

func TestGitHubEnvironmentCredentialUsesPublicTokenForEnterpriseCloud(t *testing.T) {
	t.Setenv("GH_HOST", "tenant.ghe.com")
	_, helper, ok := githubEnvironmentCredential("https://tenant.ghe.com/example/project.git")
	if !ok {
		t.Fatal("configured GitHub Enterprise Cloud origin did not produce a credential helper")
	}
	if !strings.Contains(helper, "GH_TOKEN") || strings.Contains(helper, "GH_ENTERPRISE_TOKEN") {
		t.Fatal("GitHub Enterprise Cloud helper did not use only public-host token names")
	}
}

func TestGitHubEnvironmentCredentialDoesNotBroadenConfiguredPort(t *testing.T) {
	t.Setenv("GH_HOST", "github.enterprise.test:8443")
	if _, _, ok := githubEnvironmentCredential("https://github.enterprise.test:9443/example/project.git"); ok {
		t.Fatal("configured Enterprise host authorized a different service port")
	}
	if _, _, ok := githubEnvironmentCredential("https://github.enterprise.test:8443/example/project.git"); !ok {
		t.Fatal("configured Enterprise host and port did not produce a credential helper")
	}
}

func TestDockerGitHubEnvironmentNamesScopeCredentialToCloneOrigin(t *testing.T) {
	source := []string{
		"GH_HOST=github.enterprise.test",
		"GH_TOKEN=public-primary",
		"GITHUB_TOKEN=public-fallback",
		"GH_ENTERPRISE_TOKEN=enterprise-primary",
		"GITHUB_ENTERPRISE_TOKEN=enterprise-fallback",
	}
	tests := []struct {
		name     string
		cloneURL string
		want     []string
	}{
		{name: "public", cloneURL: "https://github.com/example/project.git", want: []string{"GH_TOKEN"}},
		{name: "enterprise", cloneURL: "https://github.enterprise.test/example/project.git", want: []string{"GH_ENTERPRISE_TOKEN", "GH_HOST"}},
		{name: "unrelated host", cloneURL: "https://gitlab.example/example/project.git"},
		{name: "ssh origin unsupported", cloneURL: "git@github.com:example/project.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dockerGitHubEnvironmentNames(source, tt.cloneURL)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("Docker GitHub environment names = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDockerGitHubEnvironmentNamesSkipsHostWithoutUsableToken(t *testing.T) {
	source := []string{"GH_HOST=github.enterprise.test", "GH_ENTERPRISE_TOKEN=", "GH_TOKEN=public-token"}
	if got := dockerGitHubEnvironmentNames(source, "https://github.enterprise.test/example/project.git"); len(got) != 0 {
		t.Fatalf("Enterprise host fell back to public GitHub environment names: %v", got)
	}
}

func TestDockerGitHubEnvironmentNamesUsesCloudTokenForConfiguredGHEHost(t *testing.T) {
	source := []string{
		"GH_HOST=tenant.ghe.com",
		"GH_TOKEN=cloud-token",
		"GH_ENTERPRISE_TOKEN=server-token",
	}
	want := "GH_TOKEN,GH_HOST"
	if got := strings.Join(dockerGitHubEnvironmentNames(source, "https://tenant.ghe.com/example/project.git"), ","); got != want {
		t.Fatalf("Docker GitHub Enterprise Cloud environment names = %q, want %q", got, want)
	}
}

func TestOffHostCloneURLCredentialsRejectHTTPUserinfoOnly(t *testing.T) {
	for _, cloneURL := range []string{
		"https://fixture-token@example.invalid/project.git",
		"https://fixture-user:fixture-secret@example.invalid/project.git",
	} {
		err := validateOffHostCloneURLCredentials(cloneURL)
		if err == nil {
			t.Fatalf("accepted HTTP origin with embedded userinfo")
		}
		if strings.Contains(err.Error(), "fixture-token") || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatal("embedded credential rejection rendered URL userinfo")
		}
	}
	for _, cloneURL := range []string{
		"https://example.invalid/project.git",
		"ssh://git@example.invalid/project.git",
		"git@example.invalid:project.git",
	} {
		if err := validateOffHostCloneURLCredentials(cloneURL); err != nil {
			t.Fatalf("rejected credential-free origin %q: %v", cloneURL, err)
		}
	}
}

func TestGitHubEnvironmentCredentialSpeaksGitCredentialProtocol(t *testing.T) {
	key, helper, ok := githubEnvironmentCredential("https://github.com/example/project.git")
	if !ok {
		t.Fatal("HTTPS GitHub origin did not produce a credential helper")
	}
	cmd := exec.Command("git", "-c", key+"="+helper, "credential", "fill")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GH_TOKEN=unit-test-credential",
	}
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("Git could not invoke the environment credential helper")
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		name, value, found := strings.Cut(line, "=")
		if found {
			fields[name] = value
		}
	}
	if fields["username"] != "x-access-token" {
		t.Fatal("credential helper omitted GitHub's token username")
	}
	if fields["password"] == "" {
		t.Fatal("credential helper omitted the allowlisted token")
	}
}
