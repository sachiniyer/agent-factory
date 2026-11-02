// Package pushcred is the daemon-side half of push credentials on listed hosts
// (#5571 W7.8): which origins a GitHub credential may be answered for, where the
// daemon box's own GitHub login comes from, and how `gh` is located without
// letting a working directory choose it.
//
// The credential is never stored on a sandbox host. A session's in-sandbox git
// credential helper (and its gh shim) asks the daemon over the session's own
// scoped af callback each time it needs one; the daemon answers only for that
// session's origin, read from the daemon's checkout, never from anything the
// host reports. This package holds the parts of that answer that do not depend on
// the daemon's session state, so they are testable on their own.
package pushcred

import (
	"net/url"
	"strings"
)

// Origin is a GitHub HTTPS origin a push credential may be answered for.
type Origin struct {
	// Host is the URL's host, port included when non-default, lowercased: the
	// exact `host` field git's credential protocol sends for it.
	Host string
	// Path is "owner/repo", lowercased, without a leading slash or a ".git"
	// suffix. GitHub resolves owner and repository names case-insensitively.
	Path string
	// Enterprise is true when the token comes from the enterprise variables
	// (GH_ENTERPRISE_TOKEN / GITHUB_ENTERPRISE_TOKEN) rather than the public
	// ones: a configured GH_HOST that is not github.com or a *.ghe.com tenant.
	Enterprise bool
}

// Hostname is Host without a port, the name `gh --hostname` and GH_HOST take.
func (o Origin) Hostname() string {
	u := url.URL{Host: o.Host}
	return u.Hostname()
}

// ConfiguredGitHubHostname returns the hostname a GH_HOST value names, accepting
// a bare host or a URL. Empty when raw is empty or unparseable.
func ConfiguredGitHubHostname(raw string) string {
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

// GitHubHost reports whether hostname is a GitHub host whose token af knows how
// to find — github.com, or the host GH_HOST configures — and whether that token is
// read from the enterprise variables. It is the one predicate both the value-free
// clone helper (session.githubEnvironmentCredential) and the push credential use,
// so the two cannot disagree about which origins are GitHub.
func GitHubHost(hostname, ghHost string) (ok, enterprise bool) {
	publicGitHub := strings.EqualFold(hostname, "github.com")
	configured := ConfiguredGitHubHostname(ghHost)
	enterpriseGitHub := configured != "" && strings.EqualFold(hostname, configured)
	if !publicGitHub && !enterpriseGitHub {
		return false, false
	}
	// GitHub Enterprise Cloud tenants (*.ghe.com) authenticate with the public
	// token variables, as gh itself does.
	if publicGitHub || strings.HasSuffix(strings.ToLower(hostname), ".ghe.com") {
		return true, false
	}
	return true, true
}

// ParseOrigin parses a clone URL into the Origin a push credential may be
// answered for. ok is false for anything else: a non-HTTPS scheme (ssh origins
// authenticate with the host's own keys), a host that is not GitHub, a URL that
// embeds a password, or a path that is not exactly owner/repo.
func ParseOrigin(cloneURL, ghHost string) (Origin, bool) {
	u, err := url.Parse(strings.TrimSpace(cloneURL))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "" {
		return Origin{}, false
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		// A URL carrying a password is a credential af did not choose; answering
		// for it would mix two identities on one remote.
		return Origin{}, false
	}
	ok, enterprise := GitHubHost(u.Hostname(), ghHost)
	if !ok {
		return Origin{}, false
	}
	path, ok := normalizeRepoPath(u.Path)
	if !ok {
		return Origin{}, false
	}
	return Origin{Host: strings.ToLower(u.Host), Path: path, Enterprise: enterprise}, true
}

// Matches reports whether a git credential request — the protocol, host and path
// fields git sends with credential.useHttpPath — names exactly this origin. The
// request comes from the host and is untrusted; it is only ever compared.
func (o Origin) Matches(protocol, host, path string) bool {
	if protocol != "https" || o.Host == "" || o.Path == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(host), o.Host) {
		return false
	}
	got, ok := normalizeRepoPath(path)
	return ok && got == o.Path
}

// lfsSuffix is the path git-lfs appends when it asks for the same repository's
// credential for its batch API.
const lfsSuffix = "/info/lfs"

// normalizeRepoPath turns a URL or credential path into "owner/repo": leading and
// trailing slashes removed, one ".git" suffix (and git-lfs's "/info/lfs" after it)
// removed, lowercased. Anything that is not exactly two plain segments is refused,
// so no dot-segment, empty segment or extra component can make two different
// repositories compare equal.
func normalizeRepoPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "/")
	if strings.HasSuffix(p, ".git"+lfsSuffix) {
		p = strings.TrimSuffix(p, lfsSuffix)
	}
	p = strings.TrimSuffix(p, ".git")
	parts := strings.Split(p, "/")
	if len(parts) != 2 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\%?#\x00\n\r\t ") {
			return "", false
		}
	}
	return strings.ToLower(parts[0] + "/" + parts[1]), true
}
