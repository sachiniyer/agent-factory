package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The `[user@]host[:port]` (or `ssh://[user@]host[:port]`) destination grammar
// every host-adding surface shares (#5571 W10.5). It used to live in
// commands/hostscmd.go as `af hosts add`'s private parser; it moved here when
// the TUI config pane's Hosts section grew an add form, because two surfaces
// parsing one grammar is how a UI comes to accept a destination the writer
// would reject — the same argument the web form's field split makes in
// web/src/hosts.ts. `af hosts add` and the pane now call this one parser; the
// web keeps its field split because it cannot share Go code at all.
//
// The stored host is always the bare hostname or address: brackets around an
// IPv6 literal are URL syntax, not part of the host (ssh takes
// `-p 2222 user@2001:db8::1`, which is how af composes its argv), and the port
// is split out.

// ParseHostDestination splits `[user@]host[:port]`, or an
// `ssh://[user@]host[:port]` URL, into the entry's fields.
func ParseHostDestination(dest string) (HostConfig, error) {
	dest = strings.TrimSpace(dest)
	if strings.HasPrefix(dest, "ssh://") {
		return parseSSHURLDestination(dest)
	}
	var h HostConfig
	if user, rest, ok := strings.Cut(dest, "@"); ok {
		h.User, dest = user, rest
	}
	dest = strings.TrimSuffix(dest, "/")
	if strings.HasPrefix(dest, "[") {
		return finishBracketedHost(h, dest)
	}
	if strings.Count(dest, ":") == 1 {
		host, port, _ := strings.Cut(dest, ":")
		n, err := hostPort(port, dest)
		if err != nil {
			return h, err
		}
		h.Port, dest = n, host
	}
	h.Host = dest
	return h, nil
}

// finishBracketedHost reads `[address]` or `[address]:port` (no scheme).
func finishBracketedHost(h HostConfig, dest string) (HostConfig, error) {
	end := strings.IndexByte(dest, ']')
	if end < 0 {
		return h, fmt.Errorf("%q has an opening [ with no closing ]", dest)
	}
	h.Host = dest[1:end]
	switch rest := dest[end+1:]; {
	case rest == "":
	case strings.HasPrefix(rest, ":"):
		n, err := hostPort(rest[1:], dest)
		if err != nil {
			return h, err
		}
		h.Port = n
	default:
		return h, fmt.Errorf("%q: only a :port may follow the closing ]", dest)
	}
	return h, nil
}

// parseSSHURLDestination parses an ssh:// URL with net/url, which owns the
// bracketed-IPv6 and port rules.
func parseSSHURLDestination(dest string) (HostConfig, error) {
	var h HostConfig
	if err := refuseUnbracketedIPv6URL(dest); err != nil {
		return h, err
	}
	u, err := url.Parse(dest)
	if err != nil {
		return h, fmt.Errorf("%q is not a valid ssh:// URL: %w", dest, err)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return h, fmt.Errorf("%q: an ssh:// destination is [user@]host[:port], with no path, query or fragment", dest)
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return h, fmt.Errorf("%q: put no password in a host destination; ssh authenticates with keys", dest)
		}
		h.User = u.User.Username()
	}
	h.Host = u.Hostname()
	if port := u.Port(); port != "" {
		n, err := hostPort(port, dest)
		if err != nil {
			return h, err
		}
		h.Port = n
	}
	return h, nil
}

// refuseUnbracketedIPv6URL refuses an ssh:// destination whose host part has a
// colon outside brackets and more than one colon overall. RFC 3986 puts an IPv6
// literal in brackets, so `ssh://user@2001:db8::1` is not a URL authority:
// net/url would read the last group as a port and af would save, and probe, a
// different machine (#6067). Refusing is the only reading that cannot guess
// wrong. A single colon (`host:port`) is still a port, and the scheme-less
// `user@2001:db8::1` keeps its documented whole-address reading.
func refuseUnbracketedIPv6URL(dest string) error {
	authority := strings.TrimPrefix(dest, "ssh://")
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	userinfo := ""
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		userinfo, authority = authority[:i+1], authority[i+1:]
	}
	if strings.HasPrefix(authority, "[") || strings.Count(authority, ":") < 2 {
		return nil
	}
	return fmt.Errorf("%q has an IPv6 address outside brackets, which a URL cannot express; "+
		"put it in brackets and add the port after them, for example ssh://%s[%s]:22", dest, userinfo, authority)
}

// DescribeHost renders an entry as `af hosts list`'s `user@host:port` — the
// one-line destination summary every host surface shows beside the name. The
// web mirrors it in web/src/hosts.ts describeHost.
func DescribeHost(h HostConfig) string {
	s := h.Host
	if h.User != "" {
		s = h.User + "@" + s
	}
	if h.Port != 0 {
		s += ":" + strconv.Itoa(h.Port)
	}
	return s
}

func hostPort(port, dest string) (int, error) {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("the port in %q must be a number from 1 to 65535", dest)
	}
	return n, nil
}
