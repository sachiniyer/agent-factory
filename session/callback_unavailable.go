package session

// The callback-unavailable reason: ONE field on the Instance, projected onto
// InstanceData and rendered verbatim on every surface (#5571 W3.1).
//
// A sandbox session whose callback credential could not be minted — or whose
// grant died while provisioning — still launches: a session without callback is
// strictly more useful than no session. What it must not be is silent. The
// reason is therefore recorded at create (daemon-side, through
// ProvisionResult.CallbackUnavailable, or in-sandbox through
// InstanceOptions.CallbackUnavailable), persisted with the session record, and
// mirrored onto client projections by ReconcileCallbackUnavailable — the same
// axes ArchiveWarning travels, and for the same reason: the fact must survive
// daemon restarts and thin renderers alike.

// CallbackUnavailablePrefix is the exact prefix every surface renders before
// the reason. The phrase is part of the cross-surface contract — the TUI row,
// the web banner, the picker's notice and the JSON field are all required to
// say "af callback unavailable: <reason>" so a user greps one string and finds
// them all.
const CallbackUnavailablePrefix = "af callback unavailable: "

// callbackUndeliveredReason is the recorded reason when the ABSENCE of a
// callback is what was observed, rather than a refusal — the in-sandbox
// agent-server builds its instance without AF_DAEMON_URL/AF_DAEMON_TOKEN in its
// environment and cannot name the mint's refusal, only the fact that no
// credential arrived.
const callbackUndeliveredReason = "no callback credential was delivered to this session — the daemon could not mint or deliver one, so af commands run from this session cannot reach it"

// ReconcileCallbackUnavailable mirrors the daemon's projected callback-absence
// reason onto an existing client projection. It is independent of liveness —
// the sandbox kept running and merely lost a capability — so, like
// ArchiveWarning, it is reconciled on its own axis rather than folded into the
// status compare.
func (i *Instance) ReconcileCallbackUnavailable(reason string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.callbackUnavailable == reason {
		return false
	}
	i.callbackUnavailable = reason
	return true
}
