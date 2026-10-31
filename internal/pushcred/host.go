package pushcred

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The host half (#5571 W7.8): what the in-sandbox git credential helper and gh
// shim share. Both run as `af internal …` from the session's own staged binary,
// and both read only files af wrote into the session's own directory.

const (
	// CallbackEnvFile is the 0600 file in the session dir that holds the
	// session's AF_DAEMON_URL / AF_DAEMON_TOKEN (session.sshCallbackEnvName).
	CallbackEnvFile = "af-callback.env"
	// OriginFile is the non-secret record of the origin the session may ask for,
	// written at provision from the daemon's own clone URL. The gh shim reads it,
	// because gh, unlike git, does not say which repository it is about to use.
	OriginFile = "push-origin.json"
	// ShimFile is the gh shim's name in the session dir, which is first on the
	// pane PATH.
	ShimFile = GHProgramName

	// maxRequestBytes bounds what the helper reads from git, and maxEnvBytes the
	// callback env file. Both are a few hundred bytes in practice.
	maxRequestBytes = 64 << 10
	maxEnvBytes     = 16 << 10
)

// CredentialRequest is the part of a git credential-protocol request the helper
// relays: what git wants a credential FOR.
type CredentialRequest struct {
	Protocol, Host, Path string
}

// ReadCredentialRequest parses git's credential-protocol input — key=value lines
// up to a blank line or EOF — reading at most maxRequestBytes. Unknown keys
// (capability[], wwwauth[], username) are ignored; a repeated known key or an
// oversize input is refused.
func ReadCredentialRequest(r io.Reader) (CredentialRequest, error) {
	var req CredentialRequest
	seen := map[string]bool{}
	sc := bufio.NewScanner(io.LimitReader(r, maxRequestBytes+1))
	sc.Buffer(make([]byte, 0, 4096), maxRequestBytes+1)
	read := 0
	for sc.Scan() {
		line := sc.Text()
		read += len(line) + 1
		if read > maxRequestBytes {
			return CredentialRequest{}, errors.New("credential request is over the size bound")
		}
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return CredentialRequest{}, fmt.Errorf("credential request line has no '=': %q", line)
		}
		var dst *string
		switch key {
		case "protocol":
			dst = &req.Protocol
		case "host":
			dst = &req.Host
		case "path":
			dst = &req.Path
		default:
			continue
		}
		if seen[key] {
			return CredentialRequest{}, fmt.Errorf("credential request repeats %q", key)
		}
		seen[key] = true
		*dst = value
	}
	if err := sc.Err(); err != nil {
		return CredentialRequest{}, fmt.Errorf("reading the credential request: %w", err)
	}
	return req, nil
}

// WriteCredential writes the credential-protocol answer git reads back. A value
// that is not a single plausible token line is refused rather than written, so a
// daemon answer can never inject a second attribute.
func WriteCredential(w io.Writer, username, password string) error {
	if !plausibleToken(username) || !plausibleToken(password) {
		return errors.New("the credential is not a single printable line")
	}
	_, err := fmt.Fprintf(w, "username=%s\npassword=%s\n", username, password)
	return err
}

// WriteQuit tells git to stop: consult no further helper and do not prompt. It
// is the answer to every refusal, so git fails fast instead of waiting on the
// agent's terminal for a username.
func WriteQuit(w io.Writer) error {
	_, err := io.WriteString(w, "quit=1\n")
	return err
}

// ParseCallbackEnv reads AF_DAEMON_URL and AF_DAEMON_TOKEN from the session's
// callback env file: KEY='value' lines, each value single-quoted the way the
// provisioner writes them (session.shellQuote: an embedded single quote is
// closed, backslash-escaped and reopened). Anything else is refused rather than
// guessed at.
func ParseCallbackEnv(data []byte) (daemonURL, token string, err error) {
	if len(data) > maxEnvBytes {
		return "", "", errors.New("the callback env file is over the size bound")
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, quoted, ok := strings.Cut(line, "=")
		if !ok {
			return "", "", errors.New("the callback env file has a line without '='")
		}
		value, err := unquoteSingle(quoted)
		if err != nil {
			return "", "", fmt.Errorf("the callback env file's %s: %w", key, err)
		}
		vals[key] = value
	}
	daemonURL, token = vals["AF_DAEMON_URL"], vals["AF_DAEMON_TOKEN"]
	if daemonURL == "" || token == "" {
		return "", "", errors.New("the callback env file names no daemon URL and token")
	}
	return daemonURL, token, nil
}

// unquoteSingle undoes POSIX single quoting as session.shellQuote produces it:
// one or more '…' segments, joined by \' for an embedded quote.
func unquoteSingle(s string) (string, error) {
	var b strings.Builder
	for len(s) > 0 {
		switch {
		case strings.HasPrefix(s, "'"):
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return "", errors.New("unterminated quote")
			}
			b.WriteString(s[1 : 1+end])
			s = s[2+end:]
		case strings.HasPrefix(s, `\'`):
			b.WriteByte('\'')
			s = s[2:]
		default:
			return "", errors.New("value is not single-quoted")
		}
	}
	return b.String(), nil
}

// ShimTokenEnv marks an environment whose token variable the gh shim set: the
// OPERATOR's token, handed to one gh process. A git that gh runs inherits it,
// and must never mistake it for a token the host user set (EnvironmentToken).
const ShimTokenEnv = "AF_PUSH_CREDENTIAL_FROM_GH_SHIM"

// EnvironmentToken is the token a host user's own environment holds for a GitHub
// host: GH_TOKEN / GITHUB_TOKEN for github.com and *.ghe.com tenants, or
// GH_ENTERPRISE_TOKEN / GITHUB_ENTERPRISE_TOKEN for the host GH_HOST configures.
// These are the names, in the order, the value-free clone helper
// (session.githubEnvironmentCredential) expanded, so a host authenticated that
// way before af wired the workspace keeps that login. Nothing when the gh shim
// set the variable: that token is the operator's, not the host's.
func EnvironmentToken(getenv func(string) string, hostname, ghHost string) (string, bool) {
	if getenv(ShimTokenEnv) != "" {
		return "", false
	}
	ok, enterprise := GitHubHost(hostname, ghHost)
	if !ok {
		return "", false
	}
	names := publicTokenNames
	if enterprise {
		names = enterpriseTokenNames
	}
	for _, name := range names {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v, plausibleToken(v)
		}
	}
	return "", false
}

// tokenVars are every variable gh reads a token from.
var tokenVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// GHEnv is the environment the gh shim execs gh with: the pane's own, with every
// token variable removed and exactly ONE set for the origin's host kind —
// GH_TOKEN for github.com and *.ghe.com, GH_HOST plus GH_ENTERPRISE_TOKEN for an
// enterprise host. gh then authenticates as the operator to that host only; a
// host env token is never mixed in.
func GHEnv(environ []string, o Origin, token string) []string {
	drop := map[string]bool{ShimTokenEnv: true}
	for _, name := range tokenVars {
		drop[name] = true
	}
	if o.Enterprise {
		drop["GH_HOST"] = true
	}
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			out = append(out, kv)
		}
	}
	out = append(out, ShimTokenEnv+"=1")
	if o.Enterprise {
		return append(out, "GH_HOST="+o.Host, "GH_ENTERPRISE_TOKEN="+token)
	}
	return append(out, "GH_TOKEN="+token)
}
