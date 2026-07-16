package config

import (
	"net"
	"strings"
	"testing"
)

// TestValidateListenAddrValue pins the accept/reject verdict for
// `af config set listen_addr`. The cases are not invented: each one was checked
// against net.Listen (see the listen_addr.go doc comment), because this key has
// no loader validator to inherit and net.Listen at daemon bind time is the only
// real gate. The two easy-to-get-wrong cases are called out below.
func TestValidateListenAddrValue(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty disables the web server", "", false},
		{"loopback default", "127.0.0.1:8443", false},
		{"all interfaces", "0.0.0.0:8443", false},
		{"host omitted binds every interface", ":8443", false},
		{"ipv6 loopback", "[::1]:8443", false},
		{"hostname", "localhost:8443", false},
		{"port zero picks a free port", "127.0.0.1:0", false},
		// net.Listen accepts an EMPTY port (it binds a random one) and a
		// SERVICE-NAME port. A hand-rolled "must be a numeric port" rule would
		// reject both, making `af config set` stricter than a hand-edit — the
		// divergence this validator exists to avoid.
		{"empty port is valid to net.Listen", "127.0.0.1:", false},
		{"service-name port is valid to net.Listen", "localhost:http", false},

		{"no port at all", "8443", true},
		{"unknown service name", "foo:bar", true},
		{"port out of range", "127.0.0.1:99999", true},
		{"negative port", "127.0.0.1:-1", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateListenAddrValue(c.value)
			if c.wantErr && err == nil {
				t.Fatalf("validateListenAddrValue(%q) = nil, want an error", c.value)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validateListenAddrValue(%q) = %v, want nil", c.value, err)
			}
			if err != nil && !strings.Contains(err.Error(), "listen_addr") {
				t.Errorf("error should name the key so the user knows what to fix, got: %v", err)
			}
		})
	}
}

// TestValidateListenAddrMatchesNetListenParsing is the anti-divergence lock: for
// every case above, our verdict must equal net.Listen's own ADDRESS-PARSING
// verdict. Without it, the validator could quietly drift into rejecting values a
// hand-edited config.toml accepts (or vice versa) — the exact failure mode that
// makes `af config set` less capable than the file it writes.
//
// It compares against parse errors only. A bind-time failure (port already in
// use, permission denied, host does not resolve) is NOT a parse verdict: those
// depend on the machine and the moment, cannot be known when `config set` runs,
// and are handled by the daemon logging and skipping the web listener.
func TestValidateListenAddrMatchesNetListenParsing(t *testing.T) {
	// "" is af's own opt-out, not an address net.Listen would ever see, so it is
	// excluded from the comparison.
	addrs := []string{
		"127.0.0.1:8443", "0.0.0.0:8443", ":8443", "[::1]:8443", "localhost:8443",
		"127.0.0.1:0", "127.0.0.1:", "localhost:http",
		"8443", "foo:bar", "127.0.0.1:99999", "127.0.0.1:-1",
	}

	for _, addr := range addrs {
		t.Run(addr, func(t *testing.T) {
			ourErr := validateListenAddrValue(addr)

			// Ask net.Listen for its verdict, then discard any listener we got.
			var listenParseErr bool
			l, err := net.Listen("tcp", addr)
			if err == nil {
				_ = l.Close()
			} else {
				// A *net.OpError wrapping a syscall error is a BIND failure
				// (address in use, permission denied) — the address parsed fine.
				// An AddrError / unknown-port / DNS error is a parse-or-resolve
				// failure. Only address parsing is in our contract, and a DNS
				// failure needs the network, so treat "no such host" as
				// out-of-scope too.
				msg := err.Error()
				listenParseErr = strings.Contains(msg, "missing port") ||
					strings.Contains(msg, "unknown port") ||
					strings.Contains(msg, "invalid port")
			}

			if listenParseErr && ourErr == nil {
				t.Errorf("net.Listen rejects %q at parse time but validateListenAddrValue accepts it: "+
					"`af config set` would write a listen_addr the daemon cannot bind", addr)
			}
			if !listenParseErr && ourErr != nil {
				t.Errorf("net.Listen parses %q fine but validateListenAddrValue rejects it (%v): "+
					"`af config set` is stricter than a hand-edit", addr, ourErr)
			}
		})
	}
}
