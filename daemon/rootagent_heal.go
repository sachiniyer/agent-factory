package daemon

import (
	"os"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// healRootAgentLayers is the safe-direction self-heal for the root-agent
// snapshot's fail-closed latches (#3264): while the snapshot carries unknowns
// — a registry that could not be listed (#3247), personal configs that could
// not be loaded (#3241) — the ensure cadence re-attempts exactly those READS,
// and a success replaces "unknown" with the config's true answer. It can only
// narrow the fail-closed set, never widen or reinterpret it: a still-failing
// read leaves the latch closed, and a now-readable enabled=false resolves to
// a provenanced disable rather than a start.
//
// This is not an exception to restart-to-apply. A config that was READ at
// daemon start stays applied as read, edits and all, until the next start;
// the reads retried here FAILED at start, so the first success IS their
// snapshot read — the one the boot intended to take. The registry likewise
// freezes on its first successful list (rebuilt through the same
// projectRootAgentLayers the boot uses); it is not re-read after that.
//
// Runs on the poll goroutine only (called from EnsureRootAgents), so a plain
// Store publishes each narrowed snapshot; the backoff state keeps a broken
// registry or config from being re-read every poll tick, pacing on the shared
// ensure curve and the injectable clock.
func (m *Manager) healRootAgentLayers() {
	layers := m.rootAgentLayers.Load()
	if !layers.registryUnreadable && len(layers.personalUnreadable) == 0 {
		return
	}
	m.mu.Lock()
	due := !nowFunc().Before(m.rootHealNextAttempt)
	m.mu.Unlock()
	if !due {
		return
	}

	healed := *layers
	changed := false

	if layers.registryUnreadable {
		// A latched registry PROVABLY existed at daemon start — plain absence
		// never sets the latch (ListProjects reports it as zero projects) — so
		// during recovery an ABSENT directory is a transition, not proof of
		// zero projects: a repair mv in flight, a mount blip. ListProjects
		// maps absence to an empty success, and accepting that here would
		// clear the latch onto a frozen EMPTY snapshot — failing open against
		// personal disables that may be back moments later (#3315 review, P1).
		// Require a present registry on both sides of the read; a directory
		// that vanishes in between just waits out one more cadence.
		if projects, err := config.ListProjects(); err == nil && projectRegistryPresent() {
			healed.personal, healed.personalUnreadable, healed.projectRoots, healed.unresolvedRoots = projectRootAgentLayers(projects)
			// Recompute the legacy dedup set too (#3315 review): a legacy path
			// that resolved only after boot must dedup its repo out of the
			// healed singleton sweep, or a failing legacy attempt lets the
			// singleton create the root without the legacy layer.
			healed.legacyRepoIDs = legacyRepoIDSet(m.cfg)
			healed.registryUnreadable = false
			changed = true
			log.InfoLog.Printf("root agent snapshot: project registry is readable again; resuming root-agent resolution with %d personal layer(s), %d project(s) still failing closed", len(healed.personal), len(healed.personalUnreadable))
		}
	} else {
		personal, personalUnreadable, healedCount := retryUnreadablePersonalConfigs(layers)
		if healedCount > 0 {
			healed.personal = personal
			healed.personalUnreadable = personalUnreadable
			changed = true
		}
	}

	if changed {
		m.rootAgentLayers.Store(&healed)
	}
	m.mu.Lock()
	if changed {
		m.rootHealFailures = 0
		m.rootHealNextAttempt = nowFunc()
	} else {
		m.rootHealFailures++
		m.rootHealNextAttempt = nowFunc().Add(rootEnsureBackoffFor(m.rootHealFailures))
	}
	m.mu.Unlock()
}

// retryUnreadablePersonalConfigs re-attempts LoadProjectConfig for every repo
// the snapshot holds fail-closed, returning rebuilt personal maps and how many
// healed. Failures stay in the set silently — the boot-time warning already
// named them, and re-warning on every retry would turn a broken file into log
// spam; only the heal is news.
func retryUnreadablePersonalConfigs(layers *rootAgentSnapshot) (map[string]*config.RootAgentLayer, map[string]string, int) {
	personal := make(map[string]*config.RootAgentLayer, len(layers.personal))
	for repoID, layer := range layers.personal {
		personal[repoID] = layer
	}
	personalUnreadable := make(map[string]string, len(layers.personalUnreadable))
	healedCount := 0
	for repoID, projectID := range layers.personalUnreadable {
		pc, err := config.LoadProjectConfig(projectID)
		if err != nil {
			personalUnreadable[repoID] = projectID
			continue
		}
		healedCount++
		if layer := pc.RootAgentLayer(); layer != nil {
			personal[repoID] = layer
		}
		log.InfoLog.Printf("root agent snapshot: project %s personal config loads again; root-agent resolution for repo %s resumes from config", projectID, repoID)
	}
	return personal, personalUnreadable, healedCount
}

// projectRegistryPresent reports whether the registry directory currently
// exists as a directory. The registry heal gates on it around the read: an
// absent registry during recovery is a transition to wait out, never an empty
// success to freeze.
func projectRegistryPresent() bool {
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}
