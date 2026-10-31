package daemon

import "github.com/sachiniyer/agent-factory/config"

// authPostureRelaxed reports whether the incoming config admits a peer old
// would have rejected: the token requirement dropping, or the loopback-token
// requirement dropping while require_token held. The loopback switch is only
// a gate when require_token enforces something — under tokenDisabled it is
// inert (a tokenless posture already admits everyone), so a combined apply
// that enables require_token AND flips require_loopback_token off is a
// TIGHTENING from all-free to tokened-with-loopback-exempt, not a relaxation
// (#5137 review — judging either boolean in isolation misclassifies it, and a
// mistaken pin would hide the strengthen from draining generations). While
// require_token holds, though, the flag is exactly what exempts loopback
// peers from the token, so its true→false transition relaxes the gate for
// loopback traffic whether or not require_token also moved. It is the
// kind-freeze trigger for NON-refusal applies: a generation retired by a
// rebind or opt-out is invisible to the serving-socket refusal check —
// webHandle names only the newest generation — yet its draining handlers
// still read the live posture, which the publish is about to weaken beneath
// them. Tightening deliberately does not match: a stronger posture must reach
// drains live (#2480), so only the weakening direction freezes.
func authPostureRelaxed(old, incoming *config.Config) bool {
	if old == nil || incoming == nil {
		return false
	}
	return (old.RequireToken && !incoming.RequireToken) ||
		(old.RequireToken && old.RequireLoopbackToken && !incoming.RequireLoopbackToken)
}

// refusalPinPosture tightens the served config into the value a refusal pin
// freezes drains onto. A refusal retires a socket BECAUSE its posture is no
// longer sanctioned — most pointedly an opted-in tokenless listener whose
// allow_unauthenticated_network the apply just revoked — so pinning the raw
// served config would keep drain-window handlers answering unauthenticated on
// the very socket being torn down (#5137 review). Raising both token flags to
// their strictest interpretation of the served posture closes that: a late
// handler with no token can no longer cross the gate. A served config that
// already requires tokens everywhere pins unchanged.
func refusalPinPosture(prev *config.Config) *config.Config {
	if prev == nil || (prev.RequireToken && prev.RequireLoopbackToken) {
		return prev
	}
	tightened := *prev
	tightened.RequireToken = true
	tightened.RequireLoopbackToken = true
	return &tightened
}
