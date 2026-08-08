package session

import (
	"net"
	"os"
	"strconv"

	"github.com/sachiniyer/agent-factory/log"
)

// Resolving `ssh.host` to ONE address, once, so every later step of the session
// reaches the same machine (#3086).
//
// See internal/sshrelay for why the address is applied as a ProxyCommand rather
// than as ssh's destination, and what the two previous attempts measured.

// sshRelayBinary resolves the `af` binary af runs LOCALLY as its ProxyCommand
// relay.
//
// SEPARATE FROM sshSelfBinary ON PURPOSE, even though production sets both from
// os.Executable. They are different roles: sshSelfBinary is the binary STREAMED
// ONTO THE REMOTE and must match the remote's architecture, this one is EXECUTED
// ON THE DAEMON HOST and must match the daemon's. One variable serving both would
// break the relay the first time someone points the remote one at a
// cross-compiled build.
//
// It is resolved FRESH on every composition and never persisted. A daemon that
// restarted into an upgraded binary composes a teardown naming the binary it is
// actually running, which a path recorded at create time would not.
var sshRelayBinary = os.Executable

// SetSSHRelayBinaryForTest overrides the local relay binary and returns a restore
// function.
func SetSSHRelayBinaryForTest(path string) func() {
	prev := sshRelayBinary
	sshRelayBinary = func() (string, error) { return path, nil }
	return func() { sshRelayBinary = prev }
}

// resolvePinnedSSHDialAddress picks the ONE address this session will use, by
// dialling the configured name once and reporting where the connection actually
// landed.
//
// IT DIALS RATHER THAN LOOKING UP, and that is the whole point. A plain
// LookupIPAddr plus "take the first" would pin an address nothing has proved is
// reachable, which is precisely the regression #3090 shipped: pinning removed
// net.Dialer's try-each-consecutive-address behaviour, so a dual-stack host with a
// black-holed IPv6 route failed every provision where plain `ssh` falls through to
// IPv4. net.Dialer already implements that fallback — including RFC 6555 fast
// fallback across families — so dialling once and reading RemoteAddr yields an
// address that DEMONSTRABLY answered, and keeps the behaviour pinning had
// removed. The connection is closed immediately; it never authenticates.
//
// AN EMPTY RETURN IS NOT AN ERROR, it is "carry on exactly as before". A failure
// here means af could not determine an address, and the honest response is to
// compose the same name-based command af has always composed rather than to
// refuse. Two reasons, both learned the hard way:
//
//   - Refusing would make a probe af added a hard precondition for the whole
//     backend. `-F none` means ssh does a plain TCP connect to the same host and
//     port, so the two agree in every ordinary case — but Go's pure resolver
//     (CGO_ENABLED=0) does not consult nsswitch modules that getaddrinfo does, so
//     a name served only by mDNS or an LDAP module resolves for ssh and not for
//     af. Turning that into a dead backend is #3092's mistake with a new cause.
//   - The next thing af does is connect anyway. When the host is genuinely
//     unreachable, ssh's own error names the cause far better than a probe can.
//
// So the pin is an IMPROVEMENT over dialling by name, and where it cannot be
// computed af degrades to what it did before rather than to nothing.
func resolvePinnedSSHDialAddress(host string, port int) string {
	conn, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		log.WarningLog.Printf("backend=ssh: could not resolve %q to a single address to pin this session to "+
			"(%v); every step will resolve the name independently, so a host with several addresses could still "+
			"split this session across machines (#3086)", host, err)
		return ""
	}
	defer func() { _ = conn.Close() }()

	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || addr.IP == nil {
		return ""
	}
	// The zone is part of the identity of a link-local address: fe80::1 on eth0 is
	// not fe80::1 on eth1, and TCPAddr.IP.String() drops it. Both net.Dial and
	// net.JoinHostPort round-trip the `%zone` form, and the composer escapes the
	// `%` for ssh's own percent expansion — see sshPinnedProxyCommand.
	if addr.Zone != "" {
		return addr.IP.String() + "%" + addr.Zone
	}
	return addr.IP.String()
}
