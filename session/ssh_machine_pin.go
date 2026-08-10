package session

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// Pinning a session to a MACHINE rather than to an address (#3122).
//
// #3086/#3118 fixed DNS multiplicity: one NAME resolving to several ADDRESSES,
// where each step resolved independently and could reach a different host. Every
// step now dials one address.
//
// AN L4 LOAD-BALANCER VIP IS ONE ADDRESS THAT IS NOT ONE MACHINE. The balancer
// picks a backend PER TCP CONNECTION, and af opens one per step — each provision
// command, the `ssh -L` tunnel, and the reap. So the pin is SATISFIED and the
// invariant it protects is violated: measured against two sshds behind a
// round-robin L4 balancer, with the #3118 pin fully in effect, consecutive steps
// alternated between backends, the workspace was created on one, the reap ran on
// the other, `rm -rf` reported success having removed nothing, and the real
// workspace leaked. That is #3086's original symptom, reproduced with its fix in
// place.
//
// af cannot tell a VIP from an ordinary address by inspection — nothing in DNS or
// the socket says which it is. But the MACHINE can say who it is: sshd exports
// SSH_CONNECTION, whose third field is the address that machine accepted the
// connection on. So af asks once, at provision, and pins every later step to that
// answer.
//
// WHY THIS AND NOT THE ALTERNATIVES, all measured on the same fixture:
//
//   - Identify the machine by its HOST KEY. Fails in the only case where the bug
//     is silent. A load-balanced fleet shares one host key — that sharing is
//     exactly why the split passes verification instead of erroring — so the key
//     cannot distinguish backends. Where keys DO differ, ssh already fails closed
//     today and there is no silent leak to fix.
//   - One multiplexed connection (ControlMaster/ControlPath). Its happy path
//     works: 8 of 8 steps rode one backend where an unmultiplexed control
//     alternated. But a master is a PROCESS, and killing it left the next step
//     exiting 0 on the OTHER backend — a silent redial to the wrong machine, which
//     is the bug again. Making it fail closed is possible (the relay is invoked
//     once per new TCP connection, so a second invocation IS the redial), but that
//     only converts a silent wrong-machine reap into a permanent leak after a
//     daemon restart, which is why #3086 rejected it. Its ControlPath also has a
//     108-byte sun_path cap that an ordinary deep path exceeds.
//
// THIS APPROACH COSTS NO NEW SSH OPTION. It is #3118's ProxyCommand with a
// different pinned value, so the 7.6 floor is untouched (constraint 1), ssh's
// destination is still the NAME so known_hosts and certificate principals resolve
// as unpinned (constraint 2), and the address is PERSISTED rather than held in a
// live socket, so a reap composed in a fresh daemon reaches the same machine
// (constraint 3). Measured: with `env -i` and nothing in memory, a reap composed
// from the persisted address alone ran on the machine that held the workspace and
// removed it.

// sshMachineProbeTimeout bounds the one extra connection this costs. It is a
// create-path step with no context of its own, so it cannot be unbounded.
const sshMachineProbeTimeout = 30 * time.Second

// learnPinnedMachine asks the machine a session has just reached which address it
// accepted the connection on, and returns an address/port to pin every later step
// to. It returns ("", 0) whenever pinning to the answer would gain nothing or
// cannot be trusted — in which case the caller keeps the pin it already has.
//
// EVERY FAILURE PATH RETURNS THE EMPTY ANSWER, deliberately. This is an
// IMPROVEMENT on address-pinning, so when it cannot be computed the session must
// still be created exactly as #3118 creates it. A create-time probe that could
// refuse a session would be #3092's shape with a new cause.
func learnPinnedMachine(sshCmd, currentAddr string, currentPort int) (string, int) {
	p := newSSHSandboxProvisioner(ProvisionSpec{Title: "machine-probe"}, sshCmd, "", "")
	// stdout only: SSH_CONNECTION must not be polluted by a banner on stderr.
	out, err := p.Run(sshMachineProbeTimeout, `printf '%s' "$SSH_CONNECTION"`, nil, false)
	if err != nil {
		log.WarningLog.Printf("backend=ssh: could not ask the remote which machine it is (%v); this session "+
			"stays pinned to %s, which behind an L4 load balancer is one ADDRESS and not one MACHINE, so its "+
			"steps could still split across backends (#3122)", err, currentAddr)
		return "", 0
	}

	ip, port, ok := parseSSHConnectionServerAddr(string(out))
	if !ok {
		// sshd has exported SSH_CONNECTION since the 1990s, so an unparseable value
		// means something unusual is in the path (a wrapper, a forced command) rather
		// than an old server. Carry on unpinned-to-machine rather than guess.
		log.WarningLog.Printf("backend=ssh: the remote did not report a usable SSH_CONNECTION (%q); this "+
			"session stays pinned to %s (#3122)", strings.TrimSpace(string(out)), currentAddr)
		return "", 0
	}

	// NOTHING TO GAIN is the common case, and must be silent. When the target is an
	// ordinary host, the machine's own address is the address af already pinned. It
	// is also what an IP-preserving balancer (DSR) reports, where pinning the answer
	// would be a no-op rather than a fix.
	addr := ip.String()
	if addr == currentAddr && port == currentPort {
		return "", 0
	}

	// AN ACCEPTED-SOCKET ADDRESS IS NOT ALWAYS A MACHINE IDENTITY, and this check is
	// what keeps the difference from being catastrophic. SSH_CONNECTION describes the
	// socket the backend accepted, so a balancer that reaches its backends over
	// LOOPBACK — one running on the backend host itself — makes every backend report
	// 127.0.0.1. That address means "me" to whoever evaluates it, and the next thing
	// af does is evaluate it FROM THE DAEMON: the reachability probe below would
	// happily connect to the daemon's own sshd, af would persist 127.0.0.1, and every
	// provision, tunnel and reap would target the daemon's own machine. Where keys and
	// credentials are shared that does not even fail — it runs, including the reap.
	//
	// So only a GLOBALLY UNICAST address is portable enough to be a machine identity.
	// That accepts ordinary private fleet addressing (10/8, 172.16/12, 192.168/16) and
	// rejects loopback, unspecified, link-local and multicast in one predicate.
	// Checked BEFORE the dial, so af never probes an address that means "me".
	if !ip.IsGlobalUnicast() {
		log.WarningLog.Printf("backend=ssh: the remote reports its address as %s, which is not a portable "+
			"machine identity — SSH_CONNECTION describes the socket it accepted, so a balancer reaching its "+
			"backends over loopback reports the same address for all of them. This session stays pinned to %s "+
			"rather than to an address that would mean the daemon's own host (#3122)", addr, currentAddr)
		return "", 0
	}

	// REACHABILITY IS NOT A GIVEN, and this is the limit of the approach. A backend
	// behind a cloud balancer may report a private address the daemon cannot route
	// to — a different VPC, a NAT boundary. Pinning to it would turn a working
	// backend into one where every step fails, so it is proved reachable first, with
	// the same bounded probe #3118 uses to choose an address at all.
	dialer := net.Dialer{Timeout: sshDialProbeTimeout}
	conn, dialErr := dialer.Dial("tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
	if dialErr != nil {
		log.WarningLog.Printf("backend=ssh: the remote identifies itself as %s, but the daemon cannot reach "+
			"that address directly (%v) — a balanced backend on a network af cannot route to. This session "+
			"stays pinned to %s, so its steps could still split across backends behind an L4 load balancer; "+
			"point ssh.host at one machine if that matters (#3122)",
			net.JoinHostPort(addr, strconv.Itoa(port)), dialErr, currentAddr)
		return "", 0
	}
	_ = conn.Close()

	log.InfoLog.Printf("backend=ssh: pinning this session to the machine at %s rather than to %s, so every "+
		"later step reaches the host its workspace is on even behind an L4 load balancer (#3122)",
		net.JoinHostPort(addr, strconv.Itoa(port)), currentAddr)
	return addr, port
}

// parseSSHConnectionServerAddr pulls the SERVER side out of sshd's
// SSH_CONNECTION, whose format is "client_ip client_port server_ip server_port".
//
// The third and fourth fields are the address and port the machine accepted this
// connection ON, which behind a balancer is the backend's own address rather than
// the VIP — measured: two backends behind one VIP reported their own docker
// addresses, matching what the container runtime said they were.
func parseSSHConnectionServerAddr(raw string) (net.IP, int, bool) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) != 4 {
		return nil, 0, false
	}
	// Parsed, not merely non-empty: this value becomes a dial target and, through
	// the ProxyCommand, a token in a composed shell command.
	ip := net.ParseIP(fields[2])
	if ip == nil {
		return nil, 0, false
	}
	port, err := strconv.Atoi(fields[3])
	if err != nil || port <= 0 || port > 65535 {
		return nil, 0, false
	}
	return ip, port, true
}

// sshPinnedCleanupTarget reads the machine a persisted handle pins to.
//
// DialEndpoint wins when present, because it is the only field that carries a
// port; DialAddress alone means "#3118's address pin, on the configured port",
// which is every handle written before machine-pinning and every handle whose
// pinned port matched anyway. A malformed endpoint falls back to the address
// rather than refusing: a teardown that will not compose leaks the workspace it
// exists to remove (the #3044 lesson).
func sshPinnedCleanupTarget(d *SSHRuntimeCleanupData) (string, int) {
	if d == nil {
		return "", 0
	}
	if ep := strings.TrimSpace(d.DialEndpoint); ep != "" {
		if host, portText, err := net.SplitHostPort(ep); err == nil {
			if port, convErr := strconv.Atoi(portText); convErr == nil && port > 0 && port <= 65535 &&
				strings.TrimSpace(host) != "" {
				return host, port
			}
		}
	}
	return d.DialAddress, 0
}

// sshRecordPinnedMachine fills in the pin fields of a cleanup handle so that BOTH
// this daemon and one rolled back to a release without DialEndpoint read it
// safely. See DialEndpoint for why that asymmetry exists.
func sshRecordPinnedMachine(cleanup *SSHRuntimeCleanupData, dialAddr string, dialPort, configuredPort int) {
	if strings.TrimSpace(dialAddr) == "" {
		return
	}
	effective := dialPort
	if effective == 0 {
		effective = configuredPort
	}
	cleanup.DialEndpoint = net.JoinHostPort(dialAddr, strconv.Itoa(effective))
	if effective == configuredPort {
		// Only here can an older daemon, which appends the configured port itself,
		// reach the same machine — so it keeps a pin it reads correctly.
		cleanup.DialAddress = dialAddr
		return
	}

	// OTHERWISE, FENCE THE RECORD so an older reader REFUSES it rather than reaping
	// with it. Leaving DialAddress empty is not enough and was the first attempt:
	// that reader then sees an unpinned handle and dials the NAME, which behind a
	// multi-backend VIP is the silent wrong-machine reap this whole issue is about —
	// `rm -rf` succeeds having removed nothing, the tombstone is retired, and the
	// real workspace leaks permanently. Writing DialAddress is no better: that reader
	// appends the CONFIGURED port and can reach a different fleet member.
	//
	// Neither encoding can be read safely, so the record must not be read at all. A
	// reader without DialEndpoint validates `RemotePID != "" && !positivePID(...)`
	// and turns a failure into unavailableRuntimeCleanup — ErrWorkspaceStateUnknown,
	// which RETAINS the record and retries. So RemotePID carries a deliberately
	// non-numeric sentinel and the real pid moves to AgentPID, which that reader
	// ignores. Retained-and-retried beats silently-wrong-and-retired; the same
	// fail-closed shape docker's EngineID uses for a legacy tombstone.
	//
	// This daemon reads the pid through sshCleanupRemotePID and is unaffected.
	cleanup.AgentPID = cleanup.RemotePID
	cleanup.RemotePID = sshRollbackFencePID
}

// sshRollbackFencePID is the sentinel that makes a machine-pinned record
// unreadable to a daemon predating #3122. It is deliberately non-numeric so
// positivePID refuses it, and self-describing so a person reading the record or a
// bug report sees why.
const sshRollbackFencePID = "machine-pinned-see-3122"

// sshCleanupRemotePID is the remote agent-server pid, from wherever this record
// keeps it. AgentPID wins because its presence means RemotePID holds the rollback
// fence rather than a pid.
func sshCleanupRemotePID(d *SSHRuntimeCleanupData) string {
	if d == nil {
		return ""
	}
	if pid := strings.TrimSpace(d.AgentPID); pid != "" {
		return pid
	}
	return d.RemotePID
}
