package session

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

var browserHostIDNA = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
	idna.BidiRule(),
)

// NormalizeWebTabURL validates and normalizes a web-tab target into an absolute
// http(s) URL. It accepts a full URL ("http://localhost:3000", "https://x.com/y")
// or a bare host[:port] ("localhost:3000", "127.0.0.1:5173"), defaulting a
// missing scheme to http:// (the common dev-server case). It rejects a blank
// target, a non-http(s) scheme, or a URL with no host — the target must be
// something a browser can load. The returned URL is what the tab stores and what
// both the daemon proxy (loopback targets) and the web UI (external targets) act
// on, so there is one canonical form.
func NormalizeWebTabURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("a web tab requires a target URL (--url or --port)")
	}
	// A bare host[:port] with no scheme (localhost:3000) parses with an empty
	// Scheme and the whole thing landing in Path, so default the scheme first.
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid web tab URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("web tab URL must be http or https, got scheme %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("web tab URL %q has no host", raw)
	}
	// Browsers apply nontransitional UTS #46 processing before deciding where to
	// navigate. Persist the same ASCII hostname so width mappings, Unicode domain
	// separators, and IDNs cannot make browser and daemon classifications disagree.
	// Rebuild only the authority: mapping across raw would also change path/query
	// data. IP literals already have their canonical URL syntax and are not IDNs.
	host := u.Hostname()
	canonicalHost := host
	if net.ParseIP(host) == nil && !strings.Contains(host, ":") {
		canonicalHost, err = browserHostIDNA.ToASCII(host)
		if err != nil {
			return "", fmt.Errorf("invalid web tab URL hostname %q: %w", host, err)
		}
	}
	// The URL Standard treats a domain ending in a number as a legacy IPv4
	// candidate. A failed parse is not an ordinary DNS name: browsers reject the
	// URL outright (for example 09 or 1.2.3.999). Canonicalize successful parses
	// too, so the stored target is exactly the dotted host the browser will use.
	if browserHostEndsInNumber(canonicalHost) {
		address, ok := parseBrowserIPv4Address(canonicalHost)
		if !ok {
			return "", fmt.Errorf("invalid web tab URL hostname %q: browser rejects malformed numeric host", host)
		}
		canonicalHost = net.IPv4(
			byte(address>>24), byte(address>>16), byte(address>>8), byte(address),
		).String()
	}
	if canonicalHost != host {
		port := u.Port()
		u.Host = canonicalHost
		if port != "" {
			u.Host = net.JoinHostPort(canonicalHost, port)
		}
	}
	return u.String(), nil
}

// WebTabURLForPort builds the loopback URL a `--port N` convenience flag targets.
func WebTabURLForPort(port int) (string, error) {
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("web tab port must be between 1 and 65535, got %d", port)
	}
	return fmt.Sprintf("http://localhost:%d", port), nil
}

// IsLoopbackWebTarget reports whether rawURL points at a loopback host
// (localhost, 127.0.0.0/8, ::1). Only loopback targets are reverse-proxied by
// the daemon; every other host is treated as external and iframed directly by
// the web UI (never proxied — the daemon must not become an open proxy / SSRF
// vector). A URL that does not parse is treated as non-loopback (fail closed).
func IsLoopbackWebTarget(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

// isLoopbackHost reports whether host is the loopback name or a loopback IP.
func isLoopbackHost(host string) bool {
	// A single trailing dot is the DNS root label: "localhost." and "127.0.0.1."
	// are the rooted-FQDN forms of the same loopback host and resolve identically,
	// so strip one before comparing (#2004). Only one dot is stripped — a doubled
	// dot ("localhost..") is malformed and stays non-loopback, failing closed.
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
