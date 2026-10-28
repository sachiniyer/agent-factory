package session

import (
	"net/url"
	"os"
	"strings"
)

// githubEnvironmentCredential returns a Git config key/value that teaches Git
// to use an allowlisted GitHub token for one exact HTTPS origin. The helper
// string contains variable NAMES only; the shell reads the value when Git asks
// for a credential, so neither a docker/ssh argv nor .git/config stores it.
func githubEnvironmentCredential(cloneURL string) (key, helper string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(cloneURL))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", "", false
	}
	publicGitHub := strings.EqualFold(u.Hostname(), "github.com")
	enterpriseGitHub := strings.EqualFold(u.Hostname(), configuredGitHubHostname(os.Getenv("GH_HOST")))
	if !publicGitHub && !enterpriseGitHub {
		return "", "", false
	}
	// Keep a non-default port in the credential scope. Hostname() would broaden
	// https://git.example:8443 to every HTTPS service on git.example.
	host := u.Host
	tokenExpression := `${GH_ENTERPRISE_TOKEN:-${GITHUB_ENTERPRISE_TOKEN:-}}`
	if publicGitHub || strings.HasSuffix(strings.ToLower(u.Hostname()), ".ghe.com") {
		tokenExpression = `${GH_TOKEN:-${GITHUB_TOKEN:-}}`
	}
	helper = `!f() { token="` + tokenExpression + `"; ` +
		`if [ "$1" = get ] && [ -n "$token" ]; then ` +
		`printf '%s\n' 'username=x-access-token' "password=$token"; fi; }; f`
	return "credential." + u.Scheme + "://" + host + ".helper", helper, true
}

func configuredGitHubHostname(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func gitCloneCommand(cloneURL, destination string) string {
	command := "git "
	if key, helper, ok := githubEnvironmentCredential(cloneURL); ok {
		command += "-c " + shellQuote(key+"="+helper) + " "
	}
	return command + "clone " + shellQuote(cloneURL) + " " + shellQuote(destination)
}

func gitPersistCredentialCommand(cloneURL, repoPath string) string {
	key, helper, ok := githubEnvironmentCredential(cloneURL)
	if !ok {
		return ""
	}
	return "git -C " + shellQuote(repoPath) + " config --local " + shellQuote(key) + " " + shellQuote(helper)
}
