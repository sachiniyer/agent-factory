package config

import (
	"fmt"
	"net"
	"strings"
)

// validateListenAddrValue reports whether value is an acceptable listen_addr.
// It is the FIRST validation this key has ever had: unlike
// default_program / worktree_root / update_channel, listen_addr has no
// validateConfig rule to reuse — the loader accepts any string and the only gate
// is net.Listen at daemon bind time (daemon/tcpserver.go startTCPListener), which
// fails long after `af config set` returned success.
//
// So rather than invent a second, divergent rule, this reproduces net.Listen's
// own ADDRESS-PARSING verdict exactly, using the two stdlib calls net.Listen
// parses a TCP address with. Verified case by case against net.Listen:
//
//	"127.0.0.1:8443", ":8443" (all interfaces), "[::1]:8443", "localhost:8443",
//	"0.0.0.0:0" (random port), "127.0.0.1:" (random port), "localhost:http"
//	                                                     → accepted by both
//	"8443" (no port), "foo:bar" (unknown service), "127.0.0.1:99999"
//	                                                     → rejected by both
//
// Note the two easy-to-get-wrong cases: an EMPTY port ("127.0.0.1:") and a
// service-name port ("localhost:http") are both valid to net.Listen, so a
// hand-rolled "must be a numeric port" rule would reject addresses a hand-edit
// accepts. net.LookupPort resolves both the way net.Listen does.
//
// What it deliberately does NOT check is anything that can only fail at bind
// time and would need the network (or the future) to answer: whether the host
// resolves, whether the port is free, whether we may bind it. "not a host:8443"
// parses fine here and fails at bind — the same as a hand-edit, which is the
// point. A bind failure is logged and skipped without blocking the daemon, so
// the cost of that residue is a warning, not a wedged startup.
//
// An empty value is valid and load-bearing: it is the documented opt-out that
// disables the web server entirely (a pure-unix daemon). See the ListenAddr doc
// comment in config_types.go and docs/configuration.md.
func validateListenAddrValue(value string) error {
	if value == "" {
		return nil
	}
	// Validate the exact bytes that will be written and later handed to
	// net.Listen — no trimming, so `config set` can never accept a value the
	// daemon would then reject.
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("listen_addr must be a host:port address (e.g. 127.0.0.1:8443, 0.0.0.0:8443) "+
			"or \"\" to disable the web server, got %q: %w", value, err)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("listen_addr port is not valid in %q: %w", value, err)
	}
	return nil
}

// IsLoopbackListenAddr reports whether a listen_addr binds ONLY the loopback
// interface (127.0.0.1 / ::1 / localhost). It governs the loopback token
// exemption, and the distinction is load-bearing for security now that the
// recommended way to add TLS is a same-host reverse proxy:
//
// A reverse proxy on the same host (nginx/Caddy terminating TLS) connects to the
// daemon from 127.0.0.1, so EVERY request it forwards has a loopback RemoteAddr —
// indistinguishable from a genuine same-machine user. If the loopback exemption
// applied on a network-bound listener, all proxied traffic would skip the token
// and reach the control plane unauthenticated. So the exemption is safe ONLY when
// the listener itself is loopback-bound (where a loopback RemoteAddr truly is a
// local peer). On a NETWORK bind (0.0.0.0, a routable/Tailscale IP, or an empty
// host = every interface) the exemption is withheld and the token is required for
// all peers, loopback-origin included. An unparseable address fails safe to "not
// loopback" (token enforced).
func IsLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.TrimSpace(addr)
	}
	if host == "" {
		return false // empty host binds every interface — network-reachable
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
