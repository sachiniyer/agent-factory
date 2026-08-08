package session

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/sachiniyer/agent-factory/log"
)

// Resolving `ssh.host` to ONE address, once, so every later step of the session
// dials that same address (#3086).
//
// THAT IS AN ADDRESS, NOT A MACHINE, and the distinction is load-bearing rather
// than pedantic. It closes DNS multiplicity — a round-robin record, several A
// records, a dual-stack host — where each step resolving independently could reach
// a different box. It does NOT close load-balancer multiplicity: an L4 VIP picks a
// backend PER TCP CONNECTION, and af opens a separate connection for every
// provision command, for the tunnel, and for the reap, so those can still land on
// different machines. af cannot tell a VIP from an ordinary address, so there is
// nothing to detect and nothing to warn about; docs/backends.md documents it as a
// live limitation with the same workaround, and #3086 stays open for it.
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

// sshDialProbeTimeout bounds the one dial resolvePinnedSSHDialAddress performs.
//
// IT IS THE ONE PART OF THIS PIN THAT RUNS IN-PROCESS, which is why it needs a
// deadline at all when the relay's own dial deliberately has none. The relay is a
// child in a process group its caller SIGKILLs on the step's deadline, so matching
// ssh's no-timeout behaviour is safe there. This runs inside sshRuntime.Provision,
// which carries no context — so an unbounded dial to an address that silently
// drops packets would hold a session create for the kernel's SYN-retry window
// (~130s on Linux), where an unpinned create fails at the first ssh step's own 30s
// deadline. Giving up here costs only the pin: the composer falls back to the
// name-based command and that step reports the real error.
//
// A var so a test can prove the bound is APPLIED without spending it —
// TestTheDialProbeIsBounded shrinks it and separately asserts the shipped value.
var sshDialProbeTimeout = sshDialTimeout

// pinForProvision is the CREATE path's whole pin decision: the address to pin
// this session to, or "" to connect by name exactly as af always has.
//
// Both ways of answering "" are fail-soft on purpose, and both are the same
// lesson. A pin is an IMPROVEMENT on dialling by name; when af cannot compute one,
// the session must still be created the way it was before rather than not at all.
// #3092 is what the other choice looks like in production — a mechanism that
// failed totally instead of partially, and took backend=ssh away from every Ubuntu
// 20.04 and Debian 11 user until it was reverted.
//
// It lives here, as one function, so that decision is testable without a repo, a
// config and a real sshd — see TestAnUnusableRelayBinaryFallsBackToDiallingTheName.
func pinForProvision(host string, port int) string {
	dialAddr := resolvePinnedSSHDialAddress(host, port)
	if dialAddr == "" {
		return ""
	}
	if err := verifySSHRelayBinary(); err != nil {
		log.WarningLog.Printf("backend=ssh: not pinning this session to %s because af cannot run itself as "+
			"ssh's ProxyCommand relay (%v); connecting by name instead, so a host with several addresses "+
			"could still split this session across machines (#3086)", dialAddr, err)
		return ""
	}
	return dialAddr
}

// verifySSHRelayBinary reports whether af can actually run itself as the
// ProxyCommand relay.
//
// IT EXISTS SO A BROKEN PIN COSTS THE PIN, NOT THE BACKEND. A ProxyCommand that
// cannot exec does not degrade: ssh has no transport at all, so every step dies on
// its deadline with a timeout that names neither the relay nor the cause. That is
// the shape of #3092 — a mechanism whose failure mode was total rather than
// partial, discovered by users rather than by CI — and the lesson is not "avoid
// that one option", it is that anything af adds to this command must fall back to
// what worked before.
//
// So the CREATE path checks first and simply does not pin when the answer is no.
// The TEARDOWN path deliberately does NOT get this treatment: there, a record
// already knows which machine holds its workspace, so composing a name-based
// command instead could reap a different one and retire the tombstone. It errors
// and is retried. Fail-soft on the way in, fail-loud on the way out.
//
// os.Executable is reliable here in the case that actually happens — Go trims
// Linux's " (deleted)" suffix, so a daemon whose binary was REPLACED in place by
// an upgrade still names a path that exists and is the new af. A binary deleted
// outright is what this catches.
func verifySSHRelayBinary() error {
	path, err := sshRelayBinary()
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (mode %s)", path, info.Mode())
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable (mode %s)", path, info.Mode())
	}
	return nil
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
	// BOUNDED, unlike the relay's own dial, and the difference is where each one
	// runs. The relay is a child in a process group the caller SIGKILLs on its
	// deadline, so ssh's own no-timeout behaviour is safe there. This probe runs
	// IN-PROCESS, inside a Provision that carries no context — so an unbounded dial
	// to a black-holed address would block the create for the kernel's SYN-retry
	// window (~130s on Linux) where the first ssh step would have failed at its own
	// 30s deadline. Timing out here just means no pin, which composes the ordinary
	// name-based command and lets that step report the real error.
	dialer := net.Dialer{Timeout: sshDialProbeTimeout}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
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
