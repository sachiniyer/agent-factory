package session

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// The single rule for turning an ssh address into a host and a port.
//
// WHY THIS IS SHARED CODE AND NOT A CONVENTION. The ssh runtime and hook
// provisioning (#2847) both accept an address that may already carry ":port"
// alongside a separate port field, and they resolved the conflict in OPPOSITE
// directions: ssh let the embedded port win, hook let the separate field win. The
// same record therefore reached a different port depending on which backend read
// it (#3044) — in a feature whose entire justification was that af owns transport
// ONE way.
//
// A "mirrors the ssh backend" comment would not have prevented that and would not
// prevent the next one: such a claim is unenforced, and false the moment either
// side is edited (#2097). One function cannot disagree with itself, so both call
// this.
//
// A genuine conflict is REFUSED rather than silently resolved. Two different
// ports in one configuration is not a preference to be ranked — one of them is a
// mistake, and picking either silently sends the session somewhere the operator
// did not ask for. The error names both values so it is obvious which to delete.
// Agreement is fine: "10.0.0.7:2222" with port 2222 says one thing twice.
//
// A port with NO host (":22") is refused for the same reason (#3303). It is a
// valid net.SplitHostPort input, so the port survives the split and the empty
// host used to come back as success — and an empty host is not "no machine", it
// is LOCALHOST: net.JoinHostPort("", 22) is ":22", which the OS dials on the
// operator's own machine, while `ssh-keygen -F ""` matches nothing, so pinning
// and cleanup degraded silently instead of erroring. A colon with nothing in
// front of it is not a preference for localhost; it is a mistake, and the error
// names the input so it is obvious what to fix.
//
// A returned port of 0 means "not specified"; the caller applies its own default
// (the ssh runtime uses 22, and the hook path lets the ssh binary decide).
func resolveSSHHostPort(address string, port int) (string, int, error) {
	host := strings.TrimSpace(address)
	if host == "" {
		return "", 0, fmt.Errorf("no ssh host was given")
	}

	embeddedHost, embeddedPort, err := splitEmbeddedPort(host)
	if err != nil {
		return "", 0, err
	}
	if embeddedPort == 0 {
		// No ":port" on the address, so the separate field is the only source.
		if port < 0 || port > 65535 {
			return "", 0, fmt.Errorf("ssh port %d is out of range", port)
		}
		return host, port, nil
	}
	if embeddedHost == "" {
		return "", 0, fmt.Errorf("ssh address %q has a port but no host; an empty host means localhost to "+
			"the OS, and af will not guess that was meant — name the machine in front of the colon",
			address)
	}
	if port != 0 && port != embeddedPort {
		return "", 0, fmt.Errorf("ssh address %q embeds port %d but the port field says %d; "+
			"they must agree, so remove one — af will not guess which was meant",
			address, embeddedPort, port)
	}
	return embeddedHost, embeddedPort, nil
}

// normalizeLegacySSHAddress resolves an address the OLD way — an embedded port
// wins, a conflict is never an error — and returns an unambiguous host/port.
//
// It exists for exactly one caller: replaying a PERSISTED cleanup handle. A
// config written before conflicts were refused may embed one port and carry
// another, and that handle is how af reaps a workspace it already provisioned.
// Refusing there protects nothing — there is no new session to keep off the
// wrong port — and costs everything: the reap fails before reaching the host on
// every retry, the tombstone is retained forever, and the remote process and
// workspace leak. So the stored address is normalized once, at restore, and
// everything downstream sees a config that cannot conflict.
func normalizeLegacySSHAddress(address string, port int) (string, int) {
	host := strings.TrimSpace(address)
	if h, embedded, err := splitEmbeddedPort(host); err == nil && embedded != 0 {
		return h, embedded // the precedence those configs were written against
	}
	if port < 0 || port > 65535 {
		return host, 0
	}
	return host, port
}

// splitEmbeddedPort returns the host and port of an "address:port" value. A port
// of 0 means the address carried none.
//
// A bare IPv6 literal ("::1") is deliberately NOT a host:port — SplitHostPort
// rejects it for having too many colons, which is the answer wanted here: an
// address with no port. A bracketed one ("[::1]:22") splits normally.
//
// A malformed embedded port is an ERROR rather than a pass-through. Both old
// paths let one through — ssh handed the text straight to the ssh binary, hook
// silently kept "host:abc" as the whole hostname — and each failed later with a
// message about the address rather than about the port the operator mistyped.
func splitEmbeddedPort(address string) (host string, port int, err error) {
	h, p, splitErr := net.SplitHostPort(address)
	if splitErr != nil || p == "" {
		return "", 0, nil // no embedded port; not an error
	}
	parsed, convErr := strconv.Atoi(p)
	if convErr != nil {
		// A SERVICE NAME, not a mistake. `ssh.Dial` handed "host:ssh" to the
		// standard resolver, which reads /etc/services and yields 22, so configs
		// spelling the port that way worked — including ones now living only in a
		// persisted cleanup handle, where refusing would leak the workspace. Resolve
		// it the same way rather than inventing a stricter rule than the code this
		// replaced.
		service, lookupErr := net.LookupPort("tcp", p)
		if lookupErr != nil {
			return "", 0, fmt.Errorf("ssh address %q has port %q, which is neither a number nor a known service name", address, p)
		}
		parsed = service
	}
	if parsed <= 0 || parsed > 65535 {
		return "", 0, fmt.Errorf("ssh address %q has port %d, which is out of range", address, parsed)
	}
	return h, parsed, nil
}
