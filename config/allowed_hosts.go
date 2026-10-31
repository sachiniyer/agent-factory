package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// network.allowed_hosts: the names, beyond this machine's own, that the daemon's
// browser-facing listeners answer for.
//
// The daemon refuses any request whose Host does not name it, before routing (see
// daemon/hostguard.go). Without that, a web page whose hostname later resolves to
// 127.0.0.1 (DNS rebinding) is same-origin with the daemon in the browser and can
// read and drive the whole tokenless loopback API. localhost, loopback IPs, and
// the listener's own address are always allowed; this key adds the names a
// same-host reverse proxy forwards, such as the https://<machine>.<tailnet>.ts.net
// name `tailscale serve` passes through unchanged as the Host.
//
// An entry is a bare host — a DNS name or an IP literal, never a scheme, path or
// port. It matches that host on any port, which is the same granularity the
// built-in loopback names get: DNS rebinding is an attack on the NAME, and the
// port a browser sends is whatever port the daemon was reached on anyway.
//
// This file owns the one canonical spelling of a host, so the config side (an
// entry) and the daemon side (a request's Host) can never disagree about whether
// two spellings are the same name: lowercased, one trailing root dot dropped, and
// IP literals rendered by net/netip with any IPv4-mapped IPv6 address unmapped.

// AllowedHostsKey is the canonical config key, named in the daemon's refusal and
// in the remediation sentences that tell an operator how to allow a name.
const AllowedHostsKey = "network.allowed_hosts"

// maxHostNameLen is the DNS limit on a name's text form (RFC 1035 §2.3.4, less the
// root dot this canonical form never carries).
const maxDNSNameLen = 253

// CanonicalAllowedHost validates one network.allowed_hosts entry and returns its
// canonical spelling. The error names the entry and, when the operator plainly
// meant a host but wrote a URL or a host:port, the bare spelling that would work —
// so a typo is an actionable message rather than an entry that silently never
// matches.
func CanonicalAllowedHost(entry string) (string, error) {
	bad := func(why string) error {
		return fmt.Errorf("%s entry %q %s", AllowedHostsKey, entry, why)
	}
	if entry == "" {
		return "", bad("is empty; list host names such as af.example.com")
	}
	if strings.ContainsAny(entry, " \t\r\n") {
		return "", bad("contains whitespace; list one host name per entry")
	}
	if strings.Contains(entry, "*") {
		return "", bad("is a wildcard, which is not supported; list each name af is reached by")
	}
	if strings.Contains(entry, "/") {
		if u, err := url.Parse(entry); err == nil && u.Hostname() != "" {
			if host, herr := CanonicalAllowedHost(u.Hostname()); herr == nil {
				return "", bad(fmt.Sprintf("is a URL; write the bare host name (%q) — any port is accepted", host))
			}
		}
		return "", bad("is not a host name; write a bare host name such as af.example.com")
	}
	// A bracketed or bare IPv6 literal. Brackets are the URL spelling, so accept
	// them; a zone (fe80::1%eth0) is not something a browser ever sends in Host.
	if strings.HasPrefix(entry, "[") {
		inner, ok := strings.CutSuffix(entry[1:], "]")
		if !ok {
			return "", bad("is not a host name; write the host without a port — any port is accepted")
		}
		addr, err := netip.ParseAddr(inner)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", bad("is not a valid IPv6 address")
		}
		return addr.Unmap().String(), nil
	}
	if addr, err := netip.ParseAddr(entry); err == nil {
		if addr.Zone() != "" {
			return "", bad("carries an IPv6 zone, which a browser never sends; list the address without it")
		}
		return addr.Unmap().String(), nil
	}
	if strings.Contains(entry, ":") {
		if host, _, err := net.SplitHostPort(entry); err == nil {
			if canon, cerr := CanonicalAllowedHost(host); cerr == nil {
				return "", bad(fmt.Sprintf("has a port; write the bare host (%q) — any port is accepted", canon))
			}
		}
		return "", bad("is not a host name; write the host without a port — any port is accepted")
	}
	name, ok := canonicalHostName(entry)
	if !ok {
		return "", bad("is not a valid host name (letters, digits, '-' and '_' in dot-separated labels)")
	}
	return name, nil
}

// canonicalHostName lowercases a DNS name, drops ONE trailing root dot, and
// checks the label grammar: dot-separated labels of 1-63 letters, digits, '-' or
// '_' (underscore is not valid in a hostname proper, but browsers accept it in a
// URL and container names carry it, so refusing it would only strand users).
// Exactly one trailing dot is the DNS root label ("localhost." is the FQDN of
// "localhost"); a second is an empty label and fails.
func canonicalHostName(s string) (string, bool) {
	name := strings.ToLower(strings.TrimSuffix(s, "."))
	if name == "" || len(name) > maxDNSNameLen {
		return "", false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return "", false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return "", false
			}
		}
	}
	return name, true
}

// RequestHostName parses an HTTP Host value (uri-host [":" port], RFC 9110
// §7.2) into the canonical host it names, or ok=false when it is malformed and
// therefore names nothing. The port is checked for shape and then dropped. It is
// deliberately stricter than net.SplitHostPort: an unbracketed IPv6 literal
// ("::1"), a non-numeric port, a zone, or a character outside the hostname
// grammar is malformed — no browser sends any of them, so refusing them costs
// nothing and keeps "which name is this" unambiguous. An empty Host is the
// caller's to judge (it is absent, not malformed).
func RequestHostName(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	var name, port string
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return "", false
		}
		name, port = host[1:end], host[end+1:]
		if port != "" {
			p, ok := strings.CutPrefix(port, ":")
			if !ok || !allDigits(p) {
				return "", false
			}
		}
		addr, err := netip.ParseAddr(name)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", false
		}
		return addr.Unmap().String(), true
	}
	name = host
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		name, port = host[:i], host[i+1:]
		if strings.Contains(name, ":") || !allDigits(port) {
			return "", false
		}
	}
	trimmed := strings.TrimSuffix(name, ".")
	if addr, err := netip.ParseAddr(trimmed); err == nil {
		if !addr.Is4() {
			return "", false // an IPv6 literal must be bracketed
		}
		return addr.String(), true
	}
	return canonicalHostName(name)
}

// IsLoopbackHostName reports whether a canonical host (RequestHostName's output)
// can only ever mean this machine to a browser: bare "localhost" or a loopback IP
// literal (127.0.0.0/8, ::1).
//
// A "*.localhost" SUBDOMAIN is deliberately not one. RFC 6761 asks resolvers to
// answer the whole tree with loopback, and Chromium and Firefox do so internally,
// with no DNS query — but Safari before macOS 26 hands such a name to the ordinary
// macOS resolver, which consults /etc/hosts and then the network's DNS servers
// (WebKit bug 160504; macOS 26 moved loopback resolution into the OS frameworks
// below WebKit). On those systems a hostile network's resolver can answer
// foo.localhost with an attacker's address and later rebind it, which is exactly
// the attack this check exists to stop. Bare "localhost" does not share the
// problem in the same way: /etc/hosts maps it on a default install and is
// consulted before DNS, and Chromium and Firefox never resolve it at all. af's own *.localhost names (per-tab preview and editor origins) are
// served by the preview listener, whose rule admits only labels af minted.
func IsLoopbackHostName(canonical string) bool {
	if canonical == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(canonical)
	return err == nil && addr.IsLoopback()
}

// ListenAddrHostName is the canonical host of a listen address — the name the
// listener bound from it answers to as itself — or "" when the address names no
// single host: empty (every interface), an unspecified IP (0.0.0.0, ::), or
// unparseable. A wildcard bind is still answered on the address a client actually
// reached, which the daemon judges from the connection itself rather than from
// this.
func ListenAddrHostName(addr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || host == "" {
		return ""
	}
	if ip, perr := netip.ParseAddr(host); perr == nil {
		if ip.Zone() != "" || ip.IsUnspecified() {
			return ""
		}
		return ip.Unmap().String()
	}
	name, ok := canonicalHostName(host)
	if !ok {
		return ""
	}
	return name
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// validateAllowedHostsValue is the settable-key validator (settableKeySpecs). The
// key is a cfgStringList like cors_allowed_origins — comma-separated on the CLI (a
// host never contains a comma), replacing the whole list, empty clearing it — but
// unlike that key the loader validates it too (sanitizeAllowedHosts): it is a
// security allow-list. Every entry must be a host CanonicalAllowedHost accepts, so
// `af config set` refuses a typo eagerly instead of writing an entry the loader
// then drops.
func validateAllowedHostsValue(_, value string) error {
	for _, entry := range splitListValue(value) {
		if _, err := CanonicalAllowedHost(entry); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeAllowedHosts canonicalizes network.allowed_hosts at load and drops, with
// a warning, any entry that is not a host. Dropping is the fail-closed direction:
// an entry the daemon cannot read admits nothing, which is what a malformed entry
// could only ever have matched, and it keeps one typo in an allow-list from
// refusing the daemon's start the way a hard error would. The refusal a dropped
// name then meets names the exact line to add, and `af config set` rejects the
// same typo before it is written. Duplicates collapse to the first spelling.
func sanitizeAllowedHosts(cfg *Config, prettyConfigPath string) {
	if len(cfg.AllowedHosts) == 0 {
		cfg.AllowedHosts = nil
		return
	}
	seen := make(map[string]struct{}, len(cfg.AllowedHosts))
	out := make([]string, 0, len(cfg.AllowedHosts))
	for _, entry := range cfg.AllowedHosts {
		canon, err := CanonicalAllowedHost(entry)
		if err != nil {
			log.WarningLog.Printf("config %s: ignoring %v", prettyConfigPath, err)
			continue
		}
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	if len(out) == 0 {
		out = nil
	}
	cfg.AllowedHosts = out
}
