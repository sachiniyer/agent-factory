package daemon

import "context"

// The sandbox callback credential's OWNER binding (#3056).
//
// #3012 spent nine review rounds establishing that a route-level allowlist
// cannot carry this credential, and the reason generalises: every route worth
// giving a sandbox is parameterised by a host path or a session id, so what the
// scope needs to express is "the caller's OWN repo/session" — and a bool on a
// route table cannot say that. The routes that take no parameter fail from the
// other side: they answer from global state, because that is the only state they
// have. The table ended EMPTY, which is what this file exists to change.
//
// The registry has always known the answer — sandboxTokenRegistry.sessionFor
// returns the session that owns a presented credential — and the gate threw it
// away after converting it to a bool. This carries it through to the handler.
//
// THE RULE, and every part of this file serves it:
//
//	The flag on the route table admits a route to the SCOPE. It does not
//	authorize a request. A route in the scope must ALSO enforce its own owner
//	constraint. A route admitted before its own constraint exists is a boundary
//	that reads as enforced and is not.
//
// That rule was previously prose in a doc comment, which is how it got broken
// repeatedly. sandboxConstrainedRoutes below makes it a test failure instead.

// sandboxOwnerKey is the context key carrying the owning session id. An
// unexported zero-size struct type, so no other package can collide with it or
// forge a value into a context.
type sandboxOwnerKey struct{}

// withSandboxOwner marks ctx as a request that authenticated with the sandbox
// callback credential belonging to sessionID.
func withSandboxOwner(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sandboxOwnerKey{}, sessionID)
}

// sandboxOwner returns the session owning the credential this request
// authenticated with, and whether the request came from a sandbox at all.
//
// THE BOOL IS THE SAFETY-CRITICAL HALF, not the string. false means the request
// carried the OPERATOR's authority — the trusted unix socket, or the operator's
// own token — which is deliberately unconstrained. So a handler must branch on
// the bool and narrow only when it is true; a handler that instead filtered by
// "whatever id is in the context, empty means match nothing" would invert the
// two and hand a sandbox everything.
//
// The residual risk is the mirror image: a sandbox request that lost its context
// value would read as the operator. Nothing in the gate can lose it — servePosture
// attaches it at the single point where authorize admits a credential — but
// "structurally cannot happen" is what a test is for, so the binding is proved by
// driving a real credentialed request through the real mux rather than by
// inspecting this function.
func sandboxOwner(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(sandboxOwnerKey{}).(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// sandboxConstrainedRoutes names every route whose handler enforces an owner
// constraint, and says what the constraint IS. It is the machine-checkable half
// of the rule at the top of this file: a test asserts this set and the scope
// table (HTTPRoute.sandboxAllowed) are EQUAL in both directions, so
//
//   - opting a route into the scope without giving it a constraint fails, and
//   - deleting a constraint while leaving the route admitted fails.
//
// Prose in a comment could not do that. #3012's scope was governed by a written
// rule and drifted from it in four separate rounds — CreateSession, the repo-path
// oracles, the path enumerations, and finally the parameterless collision oracle
// — each time because a route was judged against the rule by a reader rather than
// by a check.
//
// The value is documentation for the reviewer, not something the code branches
// on: the constraint itself lives in the handler, which is the only place that
// knows what "the caller's own" means for that route.
var sandboxConstrainedRoutes = map[string]string{
	"/v1/Snapshot": "returns ONLY the caller's own session, and no delivery alarms",
}
