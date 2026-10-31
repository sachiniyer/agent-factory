package session

import "slices"

// The skipped agent-config list: ONE field on the Instance, projected onto
// InstanceData and rendered on every surface that shows the session's warnings
// (#6337) — the same transport the callback-absence reason already travels
// (#5571 W3.1), for the same reason.
//
// A sandbox session whose declared sandbox_agent_config file could not be read
// — missing, unreadable, non-regular, unparsable, or over
// agentConfigMaxFileBytes — still provisions: an agent without its
// personalization is strictly more useful than no session. What it must not be
// is silent: until now the only record was a daemon-side log line, so a session
// could appear fully provisioned while running a bare agent. The skip list is
// therefore recorded at provision (through ProvisionResult.AgentConfigSkipped),
// refreshed by every account swap that stages a fresh scope (through
// OffBoxAccountSwap.AgentConfigSkipped and recordOffBoxAccount), persisted with
// the session record, and mirrored onto client projections by
// ReconcileAgentConfigSkipped — the same axes ArchiveWarning travels. It clears
// when a later placement copies every declared file: the newest provision
// result is the freshest evidence about what the live scope actually holds.
//
// AgentConfigSkippedPrefix is the exact prefix every surface renders before the
// joined entries. The phrase is part of the cross-surface contract — the TUI
// row, the web banner, and the handoff warning all say
// "af agent config skipped: <agent:file>: <reason>[; …]" so a user greps one
// string and finds them all.
const AgentConfigSkippedPrefix = "af agent config skipped: "

// ReconcileAgentConfigSkipped mirrors the daemon's projected skip list onto an
// existing client projection. It is independent of liveness — a (re)provision
// or an account swap can rewrite the session's account scope while the row
// stays Ready/Running — so, like ArchiveWarning, it is reconciled on its own
// axis rather than folded into the status compare. The list is cloned on the
// way in: a projection must never alias the transport slice it was handed.
func (i *Instance) ReconcileAgentConfigSkipped(skipped []string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if slices.Equal(i.agentConfigSkipped, skipped) {
		return false
	}
	i.agentConfigSkipped = append([]string(nil), skipped...)
	return true
}
