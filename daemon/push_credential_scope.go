package daemon

import (
	"context"
	"net"
	"net/http"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/internal/peercred"
	"github.com/sachiniyer/agent-factory/session"
)

// Who may be answered with a push credential, and through what (#5571 W7.8). Two
// properties live here, each answering a finding of the independent security
// review of PR #6433:
//
//  1. ELIGIBILITY COMES ONLY FROM AN OPERATOR DECISION. A sandbox credential can
//     act on other sessions of its project (W3.5: create, prompt, archive, tasks).
//     Through those it must never move push credentials somewhere the operator
//     did not put them. So only a session the operator created is eligible
//     (durable, positive provenance on its row: a sandbox-created row and one of
//     unknown provenance both fail), and a session that is not eligible cannot
//     prompt, schedule a task for, or push-archive one that is.
//     An eligible caller gains nothing by doing so — its peers in the same
//     project share its origin — so it keeps those verbs.
//  2. A CREDENTIAL IS ACCEPTED ONLY THROUGH ITS OWN FORWARD, on every route.
//     Every session's -R forward ends on the one dedicated endpoint, and the
//     token alone names the owner, so a token copied out of a session directory
//     would otherwise work through ANY live session's forward — and, through
//     CreateTab, SendPrompt or AddTask, run code inside the owner. The endpoint
//     records the peer of each connection — the ssh client of one session's
//     supervised tunnel, which leads its own process group — and the auth gate
//     (authorizeSandbox) admits a sandbox credential only when that tunnel
//     carries the credential's own callback identity. GitCredential checks it
//     once more against the row's runtime.

// --- the endpoint: marker and carrier ------------------------------------------

// sandboxEndpointKey marks a request that arrived on the dedicated sandbox
// callback endpoint (W3.4). carrierPGIDKey carries the process group of the
// connection's peer there. Unexported zero-size types: nothing outside this
// package can forge either into a context.
type (
	sandboxEndpointKey struct{}
	carrierPGIDKey     struct{}
)

// markSandboxEndpoint wraps the dedicated endpoint's handler so every request it
// serves carries the marker. Nothing else sets it.
func markSandboxEndpoint(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sandboxEndpointKey{}, true)))
	})
}

func viaSandboxEndpoint(ctx context.Context) bool {
	v, _ := ctx.Value(sandboxEndpointKey{}).(bool)
	return v
}

// connPeerPGID is the process group of a unix connection's peer. A package var
// so a test can stand in for a peer it did not spawn.
var connPeerPGID = func(c net.Conn) (int, error) {
	pid, err := peercred.ConnPID(c)
	if err != nil {
		return 0, err
	}
	return unix.Getpgid(pid)
}

// carrierConnContext is the endpoint server's ConnContext: it records the
// process group of the connection's peer, once, when the connection is accepted.
// A peer whose group cannot be read records nothing, and the route refuses it.
func carrierConnContext(ctx context.Context, c net.Conn) context.Context {
	pgid, err := connPeerPGID(c)
	if err != nil || pgid <= 0 {
		return ctx
	}
	return context.WithValue(ctx, carrierPGIDKey{}, pgid)
}

func carrierPGID(ctx context.Context) int {
	pgid, _ := ctx.Value(carrierPGIDKey{}).(int)
	return pgid
}

// callbackCarrierRef names the callback identity a live tunnel carries; a
// package var so a test can place a carrier without spawning a tunnel.
var callbackCarrierRef = session.CallbackCarrierRef

// carriedByOwnForward reports whether the connection that brought this request
// is the owner runtime's own forward: its peer leads the tunnel whose callback
// identity is the owner's.
func carriedByOwnForward(pgid int, ownerRef string) bool {
	if pgid <= 0 || ownerRef == "" {
		return false
	}
	ref, ok := callbackCarrierRef(pgid)
	return ok && ref == ownerRef
}

// --- eligibility ------------------------------------------------------------------

// pushEligible reports whether inst could be answered with a push credential by
// the operator's decision alone: an off-box row the operator created, on a
// placement the live config licenses. It reads no origin (no git run) and no
// other row (the host-sharing rule), so it is safe under an op lock; both
// omissions only make the escalation guard that uses it more conservative.
func (m *Manager) pushEligible(inst *session.Instance) bool {
	if inst == nil || !inst.OperatorCreated() {
		return false
	}
	backend := instanceBackendType(inst)
	if backend != "ssh" && backend != "sandbox" {
		return false
	}
	placement, ok := instancePushPlacement(inst)
	return ok && placement.Licensed(m.Config(), backend)
}

const refusePushEscalationText = "a session without push credentials cannot prompt, schedule a task for, or push-archive a session that has them"

// refusePushEscalation is property 1's guard for a scoped verb that acts on
// target on behalf of owner: refused when target is another session that is
// eligible and owner is not. owner's row may be gone (a task outliving its
// creator), which is ineligible.
func (m *Manager) refusePushEscalation(owner string, target *session.Instance) error {
	if target == nil || target.ID == owner || !m.pushEligible(target) {
		return nil
	}
	m.mu.Lock()
	caller, _ := m.trackedSessionByIDLocked(owner)
	m.mu.Unlock()
	if m.pushEligible(caller) {
		return nil
	}
	return sandboxRefusal(refusePushEscalationText)
}
