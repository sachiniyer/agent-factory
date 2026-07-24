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
