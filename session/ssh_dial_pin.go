package session

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/config"
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
func pinForProvision(cfg config.SSHConfig, host string, port int, posture string) string {
	// FIRST, because it is a precondition rather than a preference, and checked
	// before the probe so a session that cannot be pinned costs no dial at all.
	if !sshPinIsCheckable(cfg, posture) {
		log.InfoLog.Printf("backend=ssh: not pinning this session's address under ssh_host_key_verification=%q, "+
			"because ssh would not refuse a host key it has not been told to expect — so a pin landing on a "+
			"machine this host's known_hosts entry was not recorded against would be accepted silently. A host "+
			"with several addresses can therefore still split this session across machines (#3086); point "+
			"ssh.host at one machine, or use the default strict posture", posture)
		return ""
	}
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

// sshPinIsCheckable reports whether ssh will check the machine af pinned against
// a host key it ALREADY trusts. It is the precondition for pinning at all.
//
// FIRST, THE THING THAT NARROWS THIS. With a ProxyCommand, ssh does not resolve
// the name — measured in ssh.c, where the resolution site is guarded by
// `if (addrs == NULL && options.proxy_command == NULL)`, and the only other site
// is gated on CanonicalizeHostname, which `-F none` leaves off. The relay makes
// the connection. So there is no live "af's resolver vs ssh's resolver"
// disagreement during a session: every step goes where af pinned, consistently,
// which is exactly what #3086 asked for.
//
// What remains is narrower and static: a known_hosts entry recorded EARLIER — by a
// plain `ssh`, or a previous af session — may have been written against whichever
// machine getaddrinfo/NSS selected, while af pins one Go's resolver selected. If
// those differ, the pinned machine presents a key the entry does not match.
//
// WHY THAT DECIDES WHERE AF MAY PIN. Under `strict`, ssh refuses any key it has
// not been told to expect, so a mismatch is LOUD: the create fails before
// authenticating, names the host, and nothing runs anywhere. Under `accept-new`
// with an entry already present, a mismatch is loud in the same way, because
// accept-new still refuses a CHANGED key. But under `accept-new` with NO entry for
// the name, there is no signal at all: ssh adds whatever key the pinned machine
// presents, the session provisions THERE, and the name is now bound to that
// machine's key — silent, and durable, since the intended host then looks changed
// forever. `insecure` verifies nothing, so it has no signal either.
//
// So af pins only where a wrong pin would be refused. That is a precondition, not
// a preference: it is what makes "a wrong pin fails closed" true by construction
// rather than a claim that happens to hold for one posture.
//
// AN EARLIER ATTEMPT INFERRED THIS FROM AN ERROR instead — retry unpinned when the
// create failed host-key verification — and it could not work. The error channel is
// overloaded (a changed key means both "wrong machine" and "genuine key change")
// and sometimes silent (accept-new with no entry produces no error at all, which is
// precisely the case with the worst consequence). It was deleted rather than given
// a third condition.
func sshPinIsCheckable(cfg config.SSHConfig, posture string) bool {
	switch posture {
	case config.SSHHostKeyInsecure:
		return false
	case config.SSHHostKeyAcceptNew:
		return acceptNewStoreKnowsHost(cfg)
	default:
		// strict, and any unrecognised value — sshHostKeyOptions fails safe to strict,
		// and TestPinningFollowsTheVerifyingPostureExactly asserts the two agree rather
		// than a comment claiming they do.
		return true
	}
}

// acceptNewStoreKnowsHost reports whether the accept-new store already holds a key
// for this host, which is what turns a wrong pin from silent into refused.
//
// It asks `ssh-keygen -F`, and does not parse known_hosts itself, because the file
// may be HASHED — Debian and Ubuntu ship HashKnownHosts=yes — and a hand-rolled
// parser would have to reproduce the HMAC-SHA1 keying, plus @cert-authority,
// @revoked and wildcard lines, to reach the same answer. Measured: `ssh-keygen -F`
// resolves plain and hashed entries alike and distinguishes the `[host]:port` key
// form that a non-default port requires.
//
// ANY DOUBT MEANS NO PIN. A missing ssh-keygen, an unreadable store, a non-zero
// exit: all answer "not known", so af declines to pin rather than pinning without
// the check that makes it safe. ssh-keygen ships beside ssh on Debian/Ubuntu but is
// a separate package on Alpine, so its absence is a real case and must degrade
// rather than break — the same rule the relay binary follows.
func acceptNewStoreKnowsHost(cfg config.SSHConfig) bool {
	host, port, err := resolveSSHHostPort(cfg.Host, cfg.Port)
	if err != nil {
		return false
	}
	if port == 0 {
		port = sshDefaultPort
	}
	store, err := acceptNewKnownHostsPathFor(cfg)
	if err != nil {
		return false
	}
	bin, err := lookPath("ssh-keygen")
	if err != nil {
		log.InfoLog.Printf("backend=ssh: ssh-keygen is not on PATH, so af cannot tell whether %q is already in "+
			"the accept-new host-key store; not pinning this session's address (#3086)", host)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshKnownHostsLookupTimeout)
	defer cancel()
	// -F searches, and exits non-zero when the host is absent.
	return exec.CommandContext(ctx, bin, "-F", knownHostsLookupName(host, port), "-f", store).Run() == nil
}

// sshKnownHostsLookupTimeout bounds the one `ssh-keygen -F`. It reads a local file,
// so this is a stuck-filesystem guard rather than a network one — but it runs
// in-process during a create, which carries no context of its own.
const sshKnownHostsLookupTimeout = 10 * time.Second

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
	// EFFECTIVE executability, not "some execute bit is set somewhere".
	//
	// An earlier version tested `Perm()&0o111 != 0`, which asks whether ANY class
	// may execute the file — so a root-owned 0700 replacement of the daemon binary
	// passed while every ProxyCommand then failed with EACCES. That is worse than no
	// check at all: the guard exists to fall back to the name-based command, and a
	// guard that says yes when exec will say no converts the intended fallback into
	// an unusable backend.
	//
	// faccessat(X_OK) asks the kernel the question that actually matters, with
	// AT_EACCESS so it answers for the EFFECTIVE identity the exec will run under.
	// Hand-rolled uid/gid arithmetic would have to reimplement supplementary groups,
	// ACLs and capabilities to get the same answer.
	if err := unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS); err != nil {
		return fmt.Errorf("%s is not executable by this user (mode %s): %w", path, info.Mode(), err)
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
