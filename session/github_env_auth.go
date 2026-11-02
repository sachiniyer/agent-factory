package session

import (
	"net/url"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/pushcred"
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
	// One predicate for "is this GitHub, and which token names does it use",
	// shared with the push credential (#5571 W7.8), so the clone helper and the
	// push route can never disagree about which origins are GitHub.
	github, enterprise := pushcred.GitHubHost(u.Hostname(), os.Getenv("GH_HOST"))
	if !github {
		return "", "", false
	}
	// Keep a non-default port in the credential scope. Hostname() would broaden
	// https://git.example:8443 to every HTTPS service on git.example.
	host := u.Host
	tokenExpression := `${GH_TOKEN:-${GITHUB_TOKEN:-}}`
	if enterprise {
		tokenExpression = `${GH_ENTERPRISE_TOKEN:-${GITHUB_ENTERPRISE_TOKEN:-}}`
	}
	helper = `!f() { token="` + tokenExpression + `"; ` +
		`if [ "$1" = get ] && [ -n "$token" ]; then ` +
		`printf '%s\n' 'username=x-access-token' "password=$token"; fi; }; f`
	return "credential." + u.Scheme + "://" + host + ".helper", helper, true
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
