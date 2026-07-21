package session

import (
	"errors"
	"net/url"
	"os"
	"strings"
)

// validateOffHostCloneURLCredentials rejects HTTP userinfo before an off-host
// runtime can copy the URL into a shell command, process argv, or diagnostic.
// A username-only userinfo component is rejected too: token-bearing HTTPS
// remotes commonly store the token in that slot. SSH URL usernames are normal
// routing identity and remain valid.
func validateOffHostCloneURLCredentials(cloneURL string) error {
	trimmed := strings.TrimSpace(cloneURL)
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return nil
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return errors.New("origin HTTP URL cannot be parsed safely; remove embedded credentials and use an environment credential or credential helper")
	}
	if u.User != nil {
		return errors.New("origin URL contains embedded HTTP credentials; remove the URL userinfo and use an environment credential or credential helper")
	}
	return nil
}

// githubEnvironmentCredential returns a Git config key/value that teaches Git
// to use an allowlisted GitHub token for one exact HTTPS origin. The helper
// string contains variable NAMES only; the shell reads the value when Git asks
// for a credential, so neither a docker/ssh argv nor .git/config stores it.
func githubEnvironmentCredential(cloneURL string) (key, helper string, ok bool) {
	return githubEnvironmentCredentialForHost(cloneURL, os.Getenv("GH_HOST"))
}

func githubEnvironmentCredentialForHost(cloneURL, configuredHost string) (key, helper string, ok bool) {
	scope, ok := resolveGitHubCredentialScope(cloneURL, configuredHost)
	if !ok {
		return "", "", false
	}
	tokenExpression := `${GH_ENTERPRISE_TOKEN:-${GITHUB_ENTERPRISE_TOKEN:-}}`
	if scope.tokenFamily == githubCloudToken {
		tokenExpression = `${GH_TOKEN:-${GITHUB_TOKEN:-}}`
	}
	helper = `!f() { token="` + tokenExpression + `"; ` +
		`if [ "$1" = get ] && [ -n "$token" ]; then ` +
		`printf '%s\n' 'username=x-access-token' "password=$token"; fi; }; f`
	return "credential.https://" + scope.authority + ".helper", helper, true
}

type githubTokenFamily uint8

const (
	githubCloudToken githubTokenFamily = iota
	githubServerToken
)

type githubCredentialScope struct {
	authority     string
	tokenFamily   githubTokenFamily
	forwardGHHost bool
}

// resolveGitHubCredentialScope is the single host classifier used both to
// construct Git's helper and to choose the one environment token Docker may
// forward. github.com and configured *.ghe.com hosts use the GitHub Cloud token
// family; another configured host is GitHub Enterprise Server and never falls
// back to a public token.
func resolveGitHubCredentialScope(cloneURL, configuredHost string) (githubCredentialScope, bool) {
	u, err := url.Parse(strings.TrimSpace(cloneURL))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return githubCredentialScope{}, false
	}
	hostname := strings.ToLower(u.Hostname())
	publicGitHub := hostname == "github.com"
	configuredGitHub := strings.EqualFold(u.Host, configuredGitHubAuthority(configuredHost))
	if !publicGitHub && !configuredGitHub {
		return githubCredentialScope{}, false
	}
	family := githubServerToken
	if publicGitHub || strings.HasSuffix(hostname, ".ghe.com") {
		family = githubCloudToken
	}
	// Keep a non-default port in the credential scope. Hostname() would broaden
	// https://git.example:8443 to every HTTPS service on git.example.
	return githubCredentialScope{
		authority:     u.Host,
		tokenFamily:   family,
		forwardGHHost: !publicGitHub,
	}, true
}

// dockerGitHubEnvironmentNames returns only the first credential the Git helper
// would use for this exact HTTPS GitHub origin. A host can carry public and
// enterprise credentials simultaneously; forwarding all of them would turn an
// image grant for one repository into authority over an unrelated account.
func dockerGitHubEnvironmentNames(source []string, cloneURL string) []string {
	configuredHost, _ := sourceEnvironmentValue(source, "GH_HOST")
	scope, ok := resolveGitHubCredentialScope(cloneURL, configuredHost)
	if !ok {
		return nil
	}
	candidates := []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
	if scope.tokenFamily == githubCloudToken {
		candidates = []string{"GH_TOKEN", "GITHUB_TOKEN"}
	}
	for _, name := range candidates {
		value, present := sourceEnvironmentValue(source, name)
		if !present || value == "" {
			continue
		}
		if scope.forwardGHHost {
			return []string{name, "GH_HOST"}
		}
		return []string{name}
	}
	return nil
}

func sourceEnvironmentValue(source []string, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range source {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

func configuredGitHubAuthority(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return u.Host
}

func gitCloneCommand(cloneURL, destination string) string {
	return gitCloneCommandForHost(cloneURL, destination, os.Getenv("GH_HOST"))
}

func gitCloneCommandForHost(cloneURL, destination, configuredHost string) string {
	command := "git "
	if key, helper, ok := githubEnvironmentCredentialForHost(cloneURL, configuredHost); ok {
		command += "-c " + shellQuote(key+"="+helper) + " "
	}
	return command + "clone " + shellQuote(cloneURL) + " " + shellQuote(destination)
}

func gitPersistCredentialCommand(cloneURL, repoPath string) string {
	return gitPersistCredentialCommandForHost(cloneURL, repoPath, os.Getenv("GH_HOST"))
}

func gitPersistCredentialCommandForHost(cloneURL, repoPath, configuredHost string) string {
	key, helper, ok := githubEnvironmentCredentialForHost(cloneURL, configuredHost)
	if !ok {
		return ""
	}
	return "git -C " + shellQuote(repoPath) + " config --local " + shellQuote(key) + " " + shellQuote(helper)
}
