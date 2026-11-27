package session

// The once-per-session notice for operator-set environment names an
// account-scoped session discards (#7668) — one helper every backend
// (docker, local, sandbox/ssh) funnels through, so the shape and wording
// stay the credential-mount warning's: log.WarningLog, names only, sorted,
// and the same remedy.
//
// Two clauses because the remedy must be TRUE (#7668 review): a
// keep-if-set name is restorable — `session_env_passthrough` changes its
// outcome — while an identity pin, fill or selector is the boundary
// itself; forwarding it changes nothing, so those are named under
// "account sessions always ignore these" with no remedy attached.

import (
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// discardedWarningAgent is the agent the notice exists for (#7668): the
// gemini account boundary is where the claimed set is wide enough that a
// silently-dropped operator value was the reported surprise.
const discardedWarningAgent = "gemini"

// logDiscardedAccountEnvNames emits the notice for `named` — the names a
// backend already proved were set in its own source environment AND will
// not be delivered — partitioned by whether the passthrough remedy can
// restore them.
func logDiscardedAccountEnvNames(backend, agent, source string, named []string) {
	if len(named) == 0 {
		return
	}
	restorable := make(map[string]struct{})
	for _, name := range sessionenv.AccountLoaderFilledNames(agent) {
		restorable[name] = struct{}{}
	}
	// The routing and trust names joined the discard set with the docker
	// run_args pins (#7867 R4): a shared routing name is restorable on every
	// account agent — forwarding it always delivers it — while a per-agent
	// routing name is restorable only when the agent does not pin it inside
	// unconditionally (gemini's late-env endpoint names keep the "always
	// ignore" bucket they already had).
	for name := range credentialMountSharedRoutingNames {
		restorable[name] = struct{}{}
	}
	unconditional := make(map[string]struct{})
	for _, name := range sessionenv.AccountDiscardedEnvNames(agent, nil) {
		unconditional[name] = struct{}{}
	}
	for _, name := range sessionenv.AccountLoaderFilledNames(agent) {
		delete(unconditional, name)
	}
	for name := range credentialMountRoutingNames[agent] {
		if _, pinned := unconditional[name]; !pinned {
			restorable[name] = struct{}{}
		}
	}
	var forwardable, ignored []string
	for _, name := range named {
		if _, ok := restorable[name]; ok {
			forwardable = append(forwardable, name)
		} else {
			ignored = append(ignored, name)
		}
	}
	sort.Strings(forwardable)
	sort.Strings(ignored)
	if len(forwardable) > 0 {
		log.WarningLog.Printf(
			"backend=%s: account-scoped session discarded %d env name(s) %s had set for agent %q: %s — forward any the session needs through the global session_env_passthrough",
			backend, len(forwardable), source, agent, strings.Join(forwardable, ", "))
	}
	if len(ignored) > 0 {
		log.WarningLog.Printf(
			"backend=%s: account sessions always ignore these %d env name(s) %s had set for agent %q: %s",
			backend, len(ignored), source, agent, strings.Join(ignored, ", "))
	}
}

// discardSetForWarning is the membership universe a backend intersects its
// source environment against — names af's boundary discards for THIS
// launch, computed from the live tables, so a report can only ever name
// what this session's own classification produced. The set stays
// gemini-scoped: it is the shared launch-boundary half (the local and
// sandbox paths intersect their own source environments with it), and the
// docker path adds its own run_args routing pins on top
// (warnDiscardedAccountEnvNames).
func discardSetForWarning(agent string, passthrough []string) map[string]struct{} {
	set := make(map[string]struct{})
	if agent != discardedWarningAgent {
		return set
	}
	for _, name := range sessionenv.AccountDiscardedEnvNames(agent, passthrough) {
		set[name] = struct{}{}
	}
	return set
}

// warnDiscardedAccountEnvNames is the account-scoped half of the same
// notice (#7668): names the image's ENV or docker.run_args actually set
// that THIS launch's pipeline will not deliver — the identity pins and
// fills plus every keep-if-set name the operator did not forward, plus
// the routing and trust names the post-run_args pins now neutralize on
// every account agent (#7867 R4) — minus the proxy case twins reseated to
// a forwarded sibling's value (the operator's own value still arrives).
// Reported once, names only, with the same remedy the mount path gives.
// The caller already printed the inspect-failure line, which says run_args
// coverage alone is partial.
func (p *dockerProvisioner) warnDiscardedAccountEnvNames(imageEnv []string, inspected bool) {
	_ = inspected // the caller's single inspect-failure line covers the account case too
	if p.spec.Account.Dir == "" {
		return
	}
	agent := p.spec.Account.Agent
	discarded := discardSetForWarning(agent, p.spec.SessionEnvPassthrough)
	// The routing pins are computed from the same helper the argv assembly
	// uses so the notice can never drift from what `docker run` emits —
	// pins and reseats alike; a reseated twin is filtered out below the
	// same way the launch-boundary notice drops it.
	for _, entry := range accountDockerRoutingEnvEntries(agent, p.spec.SessionEnvPassthrough, os.Environ()) {
		name, _, _ := strings.Cut(entry, "=")
		discarded[name] = struct{}{}
	}
	if len(discarded) == 0 {
		return
	}
	var set []string
	for name := range discarded {
		set = append(set, name)
	}
	named := credentialMountOverriddenWarning(imageEnv, p.runArgs, set)
	var kept []string
	for _, name := range named {
		// An unforwarded proxy spelling whose case twin was forwarded
		// reseats to the twin's daemon-env value at the boundary — an
		// operator value is delivered, so it is not a discard.
		if sib, twin := sessionenv.ProxyEnvCaseTwin(name); twin &&
			slices.Contains(p.spec.SessionEnvPassthrough, sib) && os.Getenv(sib) != "" {
			continue
		}
		kept = append(kept, name)
	}
	logDiscardedAccountEnvNames("docker", agent, "the image or docker.run_args", kept)
}
