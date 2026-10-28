package session

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// dialUsesSystemResolver is nativeResolverIsSystem as a var so a test can drive
// the static-binary path from a cgo test binary.
var dialUsesSystemResolver = nativeResolverIsSystem

// dialPinCandidate is the per-address probe dial; a var so a test can stall one.
var dialPinCandidate = func(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, timeout)
}

// pinHostIsIPLiteral reports whether host needs no resolution at all, in which
// case Go and ssh trivially agree on it.
func pinHostIsIPLiteral(host string) bool {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host) != nil
}

// resolvePinnedViaGetent is resolvePinnedSSHDialAddress for a NAME in a binary
// whose Go resolver is not the system's (#5750). It never resolves the name in
// Go: it asks the host's own resolver through `getent ahosts` (getaddrinfo, so
// NSS modules answer exactly as they do for ssh), then dials those addresses in
// order, each with an equal share of the remaining budget, and pins the first
// that answers, keeping the try-each fallback of the native path. Where getent is missing, fails or yields nothing it returns "":
// the session connects by name and ssh resolves it itself.
//
// One deadline covers getent and every dial, matching the in-process bound the
// native path documents.
func resolvePinnedViaGetent(host string, port int) string {
	bin, err := lookPath("getent")
	if err != nil {
		log.WarningLog.Printf("backend=ssh: not pinning %q: this af build cannot see NSS-only names and "+
			"getent is not on PATH to ask the system resolver; ssh will resolve the name itself (#5750)", host)
		return ""
	}
	deadline := time.Now().Add(sshDialProbeTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "ahosts", host).Output()
	if err != nil {
		log.WarningLog.Printf("backend=ssh: not pinning %q: getent could not resolve it (%v); ssh will "+
			"resolve the name itself (#5750)", host, err)
		return ""
	}
	// Collect first so the budget can be split: the answer order is kept (it is
	// the order getaddrinfo, and so ssh, would try), but a blackholed record must
	// not spend the whole deadline before a later one is tried (#5769).
	var ips []net.IP
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || seen[fields[0]] {
			continue
		}
		// Parsed, not merely non-empty: the value becomes a token in the composed
		// ProxyCommand.
		if ip := net.ParseIP(fields[0]); ip != nil {
			seen[fields[0]] = true
			ips = append(ips, ip)
		}
	}
	for n, ip := range ips {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// Each address gets an equal share of what is left, so one dead record
		// costs 1/len(ips) of the budget and the rest still get their turn.
		share := remaining / time.Duration(len(ips)-n)
		conn, derr := dialPinCandidate(net.JoinHostPort(ip.String(), strconv.Itoa(port)), share)
		if derr != nil {
			continue
		}
		_ = conn.Close()
		return ip.String()
	}
	log.WarningLog.Printf("backend=ssh: no address getent returned for %q answered on port %d; ssh will "+
		"resolve the name itself (#5750)", host, port)
	return ""
}
