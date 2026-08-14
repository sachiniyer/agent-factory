package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// The reason-bearing verdict behind every root-agent consumer refusal (#3264):
// one authority answers "will the ensure loop (re-)create this repo's root,
// and if not, why not", and one renderer turns a refusing verdict into the
// clause a consumer shows the user. Split from rootagent.go (#1145
// file-length lint) along that concern.

// rootAgentMaterializeReason names the answer repoRootAgentWillMaterialize
// gives — and, for a refusal, the cause a consumer can put in front of the
// user (#3264). A refusal without a reason reads as a bug: the pre-#3264
// messages guessed at causes ("unconfigured, unresolved, or its project may
// be deleted") that omitted every fail-closed state the daemon can actually
// be in.
type rootAgentMaterializeReason int

const (
	// rootAgentWillMaterialize: the ensure loop will (re-)create this root.
	rootAgentWillMaterialize rootAgentMaterializeReason = iota
	// rootAgentProjectDeleted: the project was deleted at runtime (#1735); the
	// ensure loop suppresses the root for the rest of this daemon's life.
	rootAgentProjectDeleted
	// rootAgentNotConfigured: no legacy root_agents entry and no registered
	// project — nothing opts this repo in.
	rootAgentNotConfigured
	// rootAgentProjectUnresolved: the repo's registered project is configured
	// and resolves to enabled, but its recorded root did not resolve to a git
	// repository at daemon start and no legacy entry exists, so nothing can
	// create the root this run (#3247 arm 2). Calling this "not configured"
	// would send the user to add config that already exists (#3264 review).
	rootAgentProjectUnresolved
	// rootAgentDisabled: the layered config resolved and says enabled=false.
	rootAgentDisabled
	// rootAgentRegistryUnreadable: the project registry could not be listed at
	// daemon start (#3247); no repo can be proven un-disabled.
	rootAgentRegistryUnreadable
	// rootAgentPersonalUnreadable: this repo's personal config exists but
	// could not be loaded at daemon start (#3241).
	rootAgentPersonalUnreadable
)

// rootAgentMaterializeVerdict pairs the reason with what a message needs to
// name it: the project whose personal config failed to load, or the layer
// that decided a disable.
type rootAgentMaterializeVerdict struct {
	reason    rootAgentMaterializeReason
	projectID string
	// disabledSource is set for rootAgentDisabled: the layer that supplied the
	// effective enabled=false. The built-in source means NO layer enabled the
	// repo (a registered project with no root-agent config anywhere) — a
	// materially different remedy from an explicit disable, and naming an
	// "explicit enabled=false" there would be a false cause (#3304 review).
	disabledSource config.RootAgentSource
}

// rootAgentMaterializeVerdictFor is the single authority for "will the ensure
// loop (re-)create this repo's root, and if not, why not". It applies the
// same checks as the ensure loop, in the same order the daemon's policy ranks
// them: a runtime deletion outlives every config claim; an unreadable
// registry or personal config makes the decision unknown (fail closed,
// #3241/#3247) before any layer is consulted — also skipping the git-forking
// legacy lookup below, whose result could not change the answer; then
// candidacy; then the layered resolution the ensure sweeps share.
func (m *Manager) rootAgentMaterializeVerdictFor(repoID string) rootAgentMaterializeVerdict {
	m.mu.Lock()
	_, deleted := m.deletedRootRepos[repoID]
	m.mu.Unlock()
	if deleted {
		return rootAgentMaterializeVerdict{reason: rootAgentProjectDeleted}
	}
	if m.rootAgentRegistryUnreadable {
		return rootAgentMaterializeVerdict{reason: rootAgentRegistryUnreadable}
	}
	if projectID, unreadable := m.rootAgentPersonalUnreadable[repoID]; unreadable {
		return rootAgentMaterializeVerdict{reason: rootAgentPersonalUnreadable, projectID: projectID}
	}
	legacy := m.legacyRootAgentForRepo(repoID)
	_, isProject := m.rootAgentProjectRoots[repoID]
	_, isUnresolved := m.rootAgentUnresolvedRoots[repoID]
	if legacy == nil && !isProject && !isUnresolved {
		return rootAgentMaterializeVerdict{reason: rootAgentNotConfigured}
	}
	resolution := m.resolvedRootAgentFor(repoID, legacy)
	if !resolution.Enabled {
		return rootAgentMaterializeVerdict{reason: rootAgentDisabled, disabledSource: resolution.EnabledSource}
	}
	if legacy == nil && !isProject {
		// Enabled on paper, but the recorded root did not resolve at daemon
		// start and no legacy entry's per-tick retry covers the repo: nothing
		// can create this root until a daemon start where the path resolves.
		return rootAgentMaterializeVerdict{reason: rootAgentProjectUnresolved}
	}
	return rootAgentMaterializeVerdict{reason: rootAgentWillMaterialize}
}

// rootAgentUnavailableDetail renders a refusing verdict as the clause a
// consumer appends to its message: what stops the root, and what fixes it.
// Empty for a verdict that will materialize.
func rootAgentUnavailableDetail(v rootAgentMaterializeVerdict) string {
	switch v.reason {
	case rootAgentProjectDeleted:
		// Suppression is installed when a delete BEGINS (suppressRootAgent), so
		// this covers a completed delete — which also durably removed the
		// root_agents opt-in, meaning re-registering alone leaves the root on
		// the built-in default — and a delete that failed partway or targeted
		// an already-unknown project, where the config may still be intact.
		// Say what is certain (the suppression and its trigger) and hedge the
		// rest (#3264 review).
		return "its project was deleted this daemon run (a completed delete also removes the root_agents opt-in); confirm the project's registration and root-agent enable are as intended, then restart the daemon"
	case rootAgentNotConfigured:
		return "no root agent is configured for this repo — add a root_agents entry or a registered project's [root_agent], then restart the daemon"
	case rootAgentProjectUnresolved:
		return "its registered project's root-agent config enables it, but the recorded project root does not currently resolve to a git repository; bring the path back and restart the daemon (all root-agent config, including any root_agents entry you add, loads at daemon start — an entry present at start then retries the path every tick)"
	case rootAgentDisabled:
		if v.disabledSource == config.RootAgentSourceBuiltIn || v.disabledSource == "" {
			// No layer enabled it: a registered project with no root-agent
			// config anywhere defaults to disabled. There is no enabled=false
			// to point at, so do not invent one.
			return "no root_agent layer enables this repo (registered projects default to disabled); enable it in the project's personal [root_agent] or the global [root_agent], or add a root_agents entry, then restart the daemon"
		}
		return fmt.Sprintf("its root agent resolves to disabled — an explicit enabled=false in the %s layer wins; enable it and restart the daemon", v.disabledSource)
	case rootAgentRegistryUnreadable:
		registry := config.ProjectRegistryDirName
		if dir, err := config.ProjectRegistryDir(); err == nil {
			registry = dir
		}
		return fmt.Sprintf("the project registry %s could not be listed at daemon start, so af fails every root agent closed rather than start one a personal config may disable; repair the registry and restart the daemon", registry)
	case rootAgentPersonalUnreadable:
		path := "its personal project config"
		if v.projectID != "" {
			if p, err := config.ProjectConfigTomlPath(v.projectID); err == nil {
				path = p
			}
		}
		return fmt.Sprintf("its personal config %s exists but cannot be loaded, so af fails closed rather than ignore a disable it cannot read; fix or remove that file and restart the daemon", path)
	}
	return ""
}
