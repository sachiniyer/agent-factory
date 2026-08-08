package session

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// Pinning one resolved address for a session's whole lifetime (#3086).
//
// THE FAILURE THIS PREVENTS is silent, which is what makes it worth code. The
// shared transport runs every step as its own `ssh` invocation — each provision
// command, the `ssh -L` tunnel, and the reap — so each one resolves the target
// independently. When `ssh.host` names something with several addresses (a
// round-robin record, a load balancer, a pair of A records), the per-session
// directory can be created on host A while the clone, the agent-server or the
// tunnel lands on host B. NOTHING ERRORS: every step succeeds against a valid,
// authorized, correctly-verified machine. The session simply is not on one
// machine. The reap then removes B's directory, reports success, and the
// agent-server and workspace on A leak with nothing left pointing at them.
//
// THE ONLY FORM WHOSE MEANING DOES NOT DEPEND ON WHO RESOLVES IT IS A LITERAL
// ADDRESS. That is the same lesson as #2999, where a bind address was not a
// dialable client URL: a name is an instruction to look something up, and every
// separate lookup is entitled to a different answer. So af resolves ONCE, at
// provision, and every later invocation dials the literal address — which is
// achieved by putting it in the composed command, so there is no step that could
// forget.
//
// A RESOLUTION FAILURE IS EXPLICIT, never a fallback to the name. Falling back
// would restore exactly the behaviour this exists to remove, at the moment DNS is
// least trustworthy, and it would do so invisibly.
//
// The host-key guarantee survives intact because the command also carries
// `-o HostKeyAlias=<the name known_hosts is keyed by>` — see sshCommandForConfig.

// sshResolveTimeout bounds the one lookup. A resolver that cannot answer in this
// long is a failure worth reporting, not worth waiting out: the caller is about
// to run a multi-minute provision and needs its target settled first.
const sshResolveTimeout = 10 * time.Second

// lookupSSHHostAddrs is the resolver seam. A package var so a test can drive the
// multi-address and failure branches without depending on DNS.
var lookupSSHHostAddrs = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// SetSSHAddressResolverForTest replaces the resolver and returns a restore
// function. Mirrors SetLookPathForTest / SetSSHSelfBinaryForTest.
func SetSSHAddressResolverForTest(f func(context.Context, string) ([]net.IPAddr, error)) func() {
	prev := lookupSSHHostAddrs
	lookupSSHHostAddrs = f
	return func() { lookupSSHHostAddrs = prev }
}

// resolveSSHDialAddress turns a configured ssh.host into the ONE literal address
// every step of this session will dial.
//
// A host that is already a literal is returned unchanged — there is nothing to
// pin and nothing that could fail, and re-resolving it would be a lookup of a
// name that is not one.
//
// When several addresses come back, the FIRST is taken. Which one is chosen
// matters far less than that the same one is used for every step; the resolver
// has already applied the system's own ordering (RFC 6724 address selection),
// so this is the address a plain `ssh` would most likely have used anyway.
func resolveSSHDialAddress(host string) (string, error) {
	h := strings.TrimSpace(host)
	if h == "" {
		return "", fmt.Errorf("backend=ssh: no ssh host was given")
	}
	if ip := net.ParseIP(h); ip != nil {
		return h, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), sshResolveTimeout)
	defer cancel()
	addrs, err := lookupSSHHostAddrs(ctx, h)
	if err != nil {
		return "", fmt.Errorf("backend=ssh: cannot resolve ssh.host %q to an address: %w "+
			"(af resolves the host once and pins that address for every step of the session, so a "+
			"multi-address host cannot split the session across machines; it will not fall back to "+
			"re-resolving per step)", h, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("backend=ssh: ssh.host %q resolved to no addresses", h)
	}

	first := addrs[0]
	if first.Zone != "" {
		// A link-local address is meaningless without its scope, and ssh accepts
		// the fe80::1%eth0 form.
		return first.IP.String() + "%" + first.Zone, nil
	}
	return first.IP.String(), nil
}
