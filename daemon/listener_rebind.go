package daemon

// listener_rebind.go holds the release-then-bind machinery of #5140: the
// same-port EADDRINUSE gate helpers, the sibling-conflict marker, the
// single-listener release-and-rollback paths for both managed listeners, and
// the two-way swap that resolves a mutual sibling block. Everything here runs
// under wl.mu from reconcile/bindWebLocked/bindPreviewLocked in
// listener_reload.go.

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// rebindWebSamePortLocked applies a same-port network.listen_addr change whose
// first bind met EADDRINUSE with the old listener still holding the port — the
// overlap shape bind-new-before-close can never survive (#5140). The order
// inverts: retire the old listener, then bind. Retire (not close) is the
// release: its synchronous half frees the port HERE while the drain keeps
// in-flight requests — the config write that moved the address among them —
// flushing on their own goroutine under the usual grace, so releasing first
// does not sever the reply it is answering. The accept gap is the span of one
// bind call.
//
// If the retry still fails — the conflict was a third party's socket all along,
// or one arrived in the gap — the address the daemon was serving on is re-bound
// (webBoundAddr, the resolved socket clients were actually dialling, so a ":0"
// config restores the SAME port rather than a fresh ephemeral one) and the
// returned error reads like a rebind that was never attempted. If the rollback
// itself fails, bound and configured state are cleared synchronously — the
// retired listener's done-watcher cannot take wl.mu until reconcile returns —
// and the error says plainly that no TCP listener remains.
// Caller holds wl.mu.
func (wl *webListeners) rebindWebSamePortLocked(addr string, policy tokenGatePolicy, notice string) error {
	oldConfigAddr := wl.webConfigAddr
	oldBoundAddr := wl.webBoundAddr
	wl.webHandle.retire()
	wl.webHandle = nil
	// The socket stopped accepting at retire(), so the bound half is already a
	// lie — clear it before the gap bind rather than after, or a slow bind
	// (hostname resolution, a contended port) leaves status advertising an
	// address nothing is listening on. Adoption or rollback restores it.
	wl.webBoundAddr = ""
	if wl.manager.lifecycle != nil {
		wl.manager.lifecycle.clearTCPBound()
	}
	handle, info, err := wl.webBind(addr)
	if err != nil {
		return wl.rollbackWebBindLocked(addr, err, oldConfigAddr, oldBoundAddr)
	}
	wl.adoptWebListenerLocked(addr, handle, info)
	// Announce only when the listener actually MOVED: a textual respelling that
	// resolves to the same endpoint (10.0.0.5:43123 → 10.0.0.5:043123) left
	// every sandbox callback URL pointing at exactly this socket — sandbox
	// callback URLs normalize the port numerically — so revoking them would
	// break working callbacks for nothing. Same rule as the rollback path,
	// which skips announce because nothing moved.
	if info.Addr != oldBoundAddr {
		wl.announceWebListenerLocked(addr, info, policy, notice)
	}
	return nil
}

// rollbackWebBindLocked re-binds the address the control listener was serving
// on before a post-release bind failed — shared by the single-listener rebind
// and the two-way swap. A rebound socket is adopted WITHOUT announcing: the
// daemon is back where it started, so nothing "moved" and no credential is
// revoked. When even the rollback fails, nothing is accepting anywhere: bound
// state is cleared HERE while wl.mu is still held — the retired listener's
// done-watcher cannot run its identical cleanup until reconcile returns, and a
// ListenerAddress read in between would report the address this error declares
// dead — the generation bump seals the retired generation, the revoke matches
// what an unexpected listener death does, and the CONFIGURED half moves to the
// requested address because that is what config holds and next start will try.
// Caller holds wl.mu.
func (wl *webListeners) rollbackWebBindLocked(addr string, bindErr error, oldConfigAddr, oldBoundAddr string) error {
	rollback, rbInfo, rbErr := wl.webBind(oldBoundAddr)
	if rbErr != nil {
		if wl.manager.lifecycle != nil {
			wl.manager.lifecycle.setTCPConfigured(addr)
			wl.manager.lifecycle.clearTCPBound()
		}
		wl.webConfigAddr = ""
		wl.webBoundAddr = ""
		wl.webGen++
		if n := wl.manager.sandboxTokens.revokeAll(); n > 0 {
			log.WarningLog.Printf("the control listener on %s is no longer accepting: revoked %d sandbox callback credential(s) issued against it", oldBoundAddr, n)
		}
		return fmt.Errorf("apply network.listen_addr %q: %w — and the rollback re-bind of the previous address %q also failed: %v — the daemon has no TCP listener", addr, bindErr, oldBoundAddr, rbErr)
	}
	wl.adoptWebListenerLocked(oldConfigAddr, rollback, rbInfo)
	log.WarningLog.Printf("network.listen_addr rebind to %q failed after the previous listener was released; restored it on %s", addr, rbInfo.Addr)
	return fmt.Errorf("apply network.listen_addr %q: %w — daemon still serving on the previous address %q", addr, bindErr, oldConfigAddr)
}

// rebindPreviewSamePortLocked is rebindWebSamePortLocked for the preview
// listener: the same-port overlap shape (#5140) takes release-then-bind with a
// rollback re-bind of the previous resolved address, because a listener rule
// that holds on one of the pair and not the other is how this file got its
// scars (#3012). Caller holds wl.mu.
func (wl *webListeners) rebindPreviewSamePortLocked(addr string, notice string) error {
	oldConfigAddr := wl.previewConfigAddr
	oldBoundAddr := wl.previewBoundAddr
	wl.previewHandle.retire()
	wl.previewHandle = nil
	// Same stale-bound window as the control listener: the socket stopped
	// accepting at retire(), so the bound half clears before the gap bind and
	// adoption or rollback restores it.
	wl.previewBoundAddr = ""
	if wl.manager.lifecycle != nil {
		wl.manager.lifecycle.clearPreviewBound()
	}
	handle, info, err := wl.previewBind(addr)
	if err != nil {
		return wl.rollbackPreviewBindLocked(addr, err, oldConfigAddr, oldBoundAddr)
	}
	wl.adoptPreviewListenerLocked(addr, handle, info)
	// Same rule as the control listener: a respelling that resolved to the
	// identical endpoint did not move anything, so no banner.
	if info.Addr != oldBoundAddr {
		announcePreviewListenerLocked(info.Addr, notice)
	}
	return nil
}

// swapWebPreviewLocked resolves the two-way sibling block: the control
// listener's request overlaps the preview listener's bound address AND the
// preview request overlaps the control bound address — the swap
// (A:P → B:P while B:P → A:P) a single release can never free. BOTH listeners
// are retired, then each is bound in turn; a bind that still fails rolls that
// listener back to the bound address it just left, so a contested swap defers
// with both listeners restored rather than dropping one.
//
// Bound state clears with the release on both sides — nothing is accepting
// there anymore, and a stale bound string would read as a phantom sibling
// blocker to the binds that follow. Configured state and credentials stay: a
// rollback to exactly these addresses may still be coming. The control
// listener binds FIRST: when a third party holds the target it rolls back
// before the preview step runs, so the preview lands on its old address only
// to find it re-taken and restores too — the whole pair survives the
// squatter instead of the preview inheriting the control listener's home.
// Caller holds wl.mu.
func (wl *webListeners) swapWebPreviewLocked(newCfg *config.Config) (webErr, previewErr error) {
	oldWebConfigAddr, oldWebBoundAddr := wl.webConfigAddr, wl.webBoundAddr
	oldPreviewConfigAddr, oldPreviewBoundAddr := wl.previewConfigAddr, wl.previewBoundAddr
	if wl.webHandle != nil {
		wl.webHandle.retire()
		wl.webHandle = nil
	}
	wl.webBoundAddr = ""
	if wl.previewHandle != nil {
		wl.previewHandle.retire()
		wl.previewHandle = nil
	}
	wl.previewBoundAddr = ""
	if wl.manager.lifecycle != nil {
		wl.manager.lifecycle.clearTCPBound()
		wl.manager.lifecycle.clearPreviewBound()
	}
	// The post-release binds run as RAW binds, not bindWebLocked: both bound
	// addresses are cleared, so the gate inside has nothing to compare, and the
	// announce must be deferred until the swap commits — announcing the control
	// move now would revoke every sandbox credential for a pair-restore that
	// leaves the daemon back on the same endpoint.
	webHandle, webInfo, werr := wl.webBind(newCfg.ListenAddr)
	if werr != nil {
		webErr = fmt.Errorf("apply network.listen_addr %q: %w", newCfg.ListenAddr, werr)
		if oldWebBoundAddr != "" {
			webErr = wl.rollbackWebBindLocked(newCfg.ListenAddr, webErr, oldWebConfigAddr, oldWebBoundAddr)
		}
	} else {
		wl.adoptWebListenerLocked(newCfg.ListenAddr, webHandle, webInfo)
	}
	previewHandle, previewInfo, perr := wl.previewBind(newCfg.PreviewListenAddr)
	if perr != nil {
		previewErr = fmt.Errorf("apply network.preview_listen_addr %q: %w", newCfg.PreviewListenAddr, perr)
		if oldPreviewBoundAddr != "" {
			// A successful control bind may now hold the preview listener's old
			// home — that is the swap. Release it BEFORE preview's rollback so
			// the pair restores together, then put the control side back too: a
			// swap whose second half cannot land must not strand the preview
			// listener.
			controlMoved := wl.webBoundAddr != "" && wl.webBoundAddr != oldWebBoundAddr
			if controlMoved {
				wl.webHandle.retire()
				wl.webHandle = nil
				wl.webBoundAddr = ""
				if wl.manager.lifecycle != nil {
					wl.manager.lifecycle.clearTCPBound()
				}
			}
			previewErr = wl.rollbackPreviewBindLocked(newCfg.PreviewListenAddr, previewErr, oldPreviewConfigAddr, oldPreviewBoundAddr)
			if controlMoved {
				webErr = wl.rollbackWebBindLocked(newCfg.ListenAddr, fmt.Errorf(
					"the swap's preview half could not bind %q and its move was undone to restore the pair", newCfg.PreviewListenAddr),
					oldWebConfigAddr, oldWebBoundAddr)
			}
		}
	} else {
		wl.adoptPreviewListenerLocked(newCfg.PreviewListenAddr, previewHandle, previewInfo)
	}
	// Announce only the halves that committed AND actually moved: the banners
	// and the sandbox-credential revoke belong to a real endpoint change, and a
	// restored side never left home.
	if webErr == nil && webInfo.Addr != oldWebBoundAddr {
		cfg := wl.manager.Config()
		wl.announceWebListenerLocked(newCfg.ListenAddr, webInfo, webListenerPolicy(cfg), config.ListenerExposureNotice(cfg))
	}
	if previewErr == nil && previewInfo.Addr != oldPreviewBoundAddr {
		announcePreviewListenerLocked(previewInfo.Addr, config.PreviewListenerExposureNotice(wl.manager.Config()))
	}
	return webErr, previewErr
}

// rollbackPreviewBindLocked is rollbackWebBindLocked for the preview listener:
// re-bind the address it was serving on before the post-release bind failed —
// adopted without announcing on success, bound and configured state cleared
// synchronously when even the rollback fails. Caller holds wl.mu.
func (wl *webListeners) rollbackPreviewBindLocked(addr string, bindErr error, oldConfigAddr, oldBoundAddr string) error {
	rollback, rbInfo, rbErr := wl.previewBind(oldBoundAddr)
	if rbErr != nil {
		if wl.manager.lifecycle != nil {
			wl.manager.lifecycle.setPreviewConfigured(addr)
			wl.manager.lifecycle.clearPreviewBound()
		}
		wl.previewConfigAddr = ""
		wl.previewBoundAddr = ""
		wl.previewGen++
		return fmt.Errorf("apply network.preview_listen_addr %q: %w — and the rollback re-bind of the previous address %q also failed: %v — the daemon has no preview listener", addr, bindErr, oldBoundAddr, rbErr)
	}
	wl.adoptPreviewListenerLocked(oldConfigAddr, rollback, rbInfo)
	log.WarningLog.Printf("network.preview_listen_addr rebind to %q failed after the previous listener was released; restored it on %s", addr, rbInfo.Addr)
	return fmt.Errorf("apply network.preview_listen_addr %q: %w — daemon still serving preview on the previous address %q", addr, bindErr, oldConfigAddr)
}

// siblingBlockedError marks a same-port EADDRINUSE the release gate declined
// because the OTHER managed listener's bound address also overlaps the request:
// releasing could never have made the retry succeed. It is still the plain
// bind error to any reader — the marker only lets reconcile retry the bind once
// after the sibling's own step has run, since a single apply that moves or
// tears down the sibling frees the blocker.
type siblingBlockedError struct{ err error }

func (e siblingBlockedError) Error() string { return e.err.Error() }
func (e siblingBlockedError) Unwrap() error { return e.err }

// listenErrAddr determines the endpoint a failed bind was reaching for, for the
// overlap gate. A LITERAL host — empty, an IP, or a zoned IP — resolves
// deterministically (no DNS), so the requested spelling is authoritative and is
// preferred: the OpError can lose the requested family, since net.Listen on
// "[::]:P" may report the Addr of its final internal bind attempt as 0.0.0.0:P.
// For a HOSTNAME the OpError.Addr is the endpoint the same DNS answer produced
// — a second lookup could see a different (round-robin) answer — so it is used
// first there, with resolution as the fallback for error shapes that carry
// none (the injected test seam).
func listenErrAddr(err error, requested string) *net.TCPAddr {
	if host, _, serr := net.SplitHostPort(requested); serr == nil {
		if _, perr := netip.ParseAddr(host); host == "" || perr == nil {
			if ta, rerr := net.ResolveTCPAddr("tcp", requested); rerr == nil {
				return ta
			}
		}
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		if ta, ok := oe.Addr.(*net.TCPAddr); ok && ta != nil {
			return ta
		}
	}
	ta, _ := net.ResolveTCPAddr("tcp", requested)
	return ta
}

// boundListenerBlocks reports whether the listener bound on boundAddr could be
// one of the sockets an EADDRINUSE on req names — the #5140 gate for whether
// release-then-bind can fix the failure at all. Two binds collide only when
// they share a port AND their address sets intersect, so both halves are
// checked:
//
//   - Ports compare after RESOLUTION, not as text: the config validator accepts
//     every port spelling net.Listen does (service names, leading zeros), so a
//     requested ":http" must measure equal to a bound ":80". boundAddr is the
//     resolved address the listener is accepting on, so a configured ":0"
//     measures against the port the kernel actually chose.
//   - Addresses overlap when either side is the wildcard — it owns every
//     address on its port, which is the reported narrowing/widening shape — or
//     when the resolved IPs are equal. Two DISTINCT specific addresses on one
//     port coexist fine (127.0.0.1:P next to 127.0.0.2:P), so that EADDRINUSE
//     names a third party's socket, not ours — and releasing a healthy listener
//     for it would drop idle connections and open a rollback race for nothing.
//   - Equal IPs must also carry compatible ZONES: [fe80::1%eth0]:P and
//     [fe80::1%eth1]:P are distinct sockets that coexist, which IP.Equal alone
//     cannot see. A zone-free side is compatible with any zone.
//   - And the two must share an ADDRESS FAMILY: a bound 0.0.0.0 socket owns no
//     IPv6 address, so it never blocks [::1]:P, and a specific v6 sibling is no
//     blocker for a v4-specific request. The family sets are computed
//     asymmetrically, though — see requestIPFamilies: a REQUESTED 0.0.0.0 is
//     bound by Go as a dual-stack [::] socket, so it contends for IPv6 space
//     even though a bound 0.0.0.0 means a real AF_INET socket. On a kernel that
//     forces IPv6-only wildcards a valid rebind declines to the same deferred
//     answer it gave before #5140, which errs toward not bouncing a healthy
//     listener.
//
// boundAddr always parses: it is the resolved address a listener is accepting
// on. A nil req (no recoverable bind target) answers false — no reason to
// release a working listener on speculation.
func boundListenerBlocks(boundAddr string, req *net.TCPAddr) bool {
	if req == nil {
		return false
	}
	bound, err := net.ResolveTCPAddr("tcp", boundAddr)
	if err != nil {
		return false
	}
	if bound.Port != req.Port {
		return false
	}
	if bound.Zone != "" && req.Zone != "" && bound.Zone != req.Zone {
		return false
	}
	return listenIPsOverlap(bound.IP, req.IP)
}

// listenIPsOverlap reports whether a listener bound on bound owns address
// space a bind REQUEST for req contends for. The family test is asymmetric:
// bound is the resolved address an EXISTING socket reports, while req is a
// spelling about to be handed to net.Listen — and the two mean different
// things for an IPv4 wildcard (requestIPFamilies). nil is Go's dual-stack
// wildcard spelling — an empty host in the config binds every interface of
// both families.
func listenIPsOverlap(bound, req net.IP) bool {
	b4, b6 := listenIPFamilies(bound)
	r4, r6 := requestIPFamilies(req)
	if !((b4 && r4) || (b6 && r6)) {
		return false
	}
	if bound == nil || bound.IsUnspecified() || req == nil || req.IsUnspecified() {
		return true
	}
	return bound.Equal(req)
}

// requestIPFamilies reports the address families a bind REQUEST for ip
// contends for — deliberately asymmetric with listenIPFamilies. Go resolves a
// requested "0.0.0.0:P" to an IPv4 wildcard and then binds it as a dual-stack
// [::] socket on the platforms af supports (the listener's Addr reports
// [::]:P), so the request still collides with IPv6-specific sockets like
// [::1]:P even though it was spelled IPv4. Any requested wildcard therefore
// owns BOTH families; only a specific requested IP is single-family.
func requestIPFamilies(ip net.IP) (v4, v6 bool) {
	switch {
	case ip == nil || ip.IsUnspecified():
		return true, true
	case ip.To4() != nil:
		return true, false
	default:
		return false, true
	}
}

// listenRequestsCoexist reports whether two ATTEMPTED bind endpoints could be
// held simultaneously — the preflight a two-way swap needs: a mutual sibling
// block is a realizable exchange only when the requests do not overlap each
// other. The endpoints come from listenErrAddr — the same DNS answer the
// failed binds reached for, not a fresh lookup that could return a different
// (round-robin) address than the one that produced the siblingBlockedError.
// Both sides get requestIPFamilies semantics (a requested wildcard is a
// dual-stack bind) on equal ports, with the same zone rule boundListenerBlocks
// applies. A nil endpoint answers false: an unverifiable request is no reason
// to retire both listeners.
func listenRequestsCoexist(ra, rb *net.TCPAddr) bool {
	if ra == nil || rb == nil {
		return false
	}
	if ra.Port != rb.Port {
		return true
	}
	if ra.Zone != "" && rb.Zone != "" && ra.Zone != rb.Zone {
		return true
	}
	a4, a6 := requestIPFamilies(ra.IP)
	b4, b6 := requestIPFamilies(rb.IP)
	if !((a4 && b4) || (a6 && b6)) {
		return true
	}
	if ra.IP == nil || ra.IP.IsUnspecified() || rb.IP == nil || rb.IP.IsUnspecified() {
		return false
	}
	return !ra.IP.Equal(rb.IP)
}

// listenIPFamilies reports the address families a socket already BOUND on ip
// owns — the resolved address it reports, not the request that made it. An
// explicit IPv4 wildcard (0.0.0.0, or any v4 spelling including v4-mapped) in
// this position means a real AF_INET socket — a dual-stack bind reports
// [::]:P instead — so it covers IPv4 only; a bound "::"-family wildcard is the
// dual-stack socket and owns both families.
func listenIPFamilies(ip net.IP) (v4, v6 bool) {
	switch {
	case ip == nil:
		return true, true
	case ip.IsUnspecified() && ip.To4() != nil:
		return true, false
	case ip.IsUnspecified():
		return true, true
	case ip.To4() != nil:
		return true, false
	default:
		return false, true
	}
}
