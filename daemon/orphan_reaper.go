package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// sweepOrphanContainers reaps docker session containers this daemon leaked, at
// startup (#2194 slice 4). Package-level so tests can stub out the docker work.
var sweepOrphanContainers = session.SweepOrphanContainers

// configDirForReap resolves the AF home the orphan sweep scopes its container
// query to. It must match the af.home label runContainer stamps.
var configDirForReap = config.GetConfigDir

// sweepStartupOrphanContainers runs after instance restore, but before the
// manager readiness barrier opens. Therefore every af.home-scoped container the
// sweep can see predates create admission; a new CreateSession cannot manufacture
// a candidate midway through the destructive pass (#2632).
//
// When skippedRepos is non-empty the daemon's session view is known-incomplete: a
// repo whose instances.json was corrupted/unreadable contributed zero rows to
// m.instances, so the protected-slug set omits that repo's still-running
// containers. Running the sweep in that state would force-remove live
// containers the sweep cannot distinguish from genuine orphans — the same
// "cannot distinguish orphaned from live, err toward sparing" principle the
// sweep's slug-collision branch already follows (session/orphan_reaper.go). The
// sweep is deferred instead, and runDeferredOrphanSweepIfReady re-runs it once
// the poll loop drains the skip set (a skipped repo whose instances.json now
// parses re-materializes its rows, so the protected set is complete again). This
// bounds the #2194 regression to one polling interval: genuine orphans survive
// un-reaped only until the view is complete, while a skipped repo's live
// container is never force-killed.
func sweepStartupOrphanContainers(manager *Manager) {
	homeID, err := configDirForReap()
	if err != nil {
		log.WarningLog.Printf("orphan sweep: cannot resolve the AF home; skipping: %v", err)
		return
	}
	manager.mu.Lock()
	skipped := len(manager.skippedRepos)
	if skipped > 0 {
		manager.deferredOrphanSweepArmed = true
		manager.mu.Unlock()
		log.WarningLog.Printf("orphan sweep: deferring the destructive pass; %d repo(s) have unreadable instances.json — their live containers cannot be distinguished from genuine orphans. The sweep will re-run once the skip set drains on the next polling refresh.", skipped)
		return
	}
	manager.mu.Unlock()
	sweepOrphanContainers(homeID, manager.dockerReapProtectedSlugs())
}

// runDeferredOrphanSweepIfReady runs the startup orphan sweep that was deferred
// because skippedRepos was non-empty, once the polling refresh has repaired
// every skipped repo (drained the skip set to empty). It is called from the
// instance poll loop after RefreshInstances, the only recurring trigger, so the
// deferred sweep runs with no daemon restart. A no-op when the startup sweep was
// not deferred or the skip set has not yet drained; if any repo is still
// skipped the sweep stays deferred and re-evaluates on the next poll.
//
// The sweep runs outside m.mu just as the startup path does: it calls
// dockerReapProtectedSlugs, which acquires m.mu itself for the short
// title-collection. Once the skip set drains, every previously-skipped repo's
// sessions are in m.instances, so their container slugs are in the protected set
// — the sweep is safe (complete view) and a re-corruption mid-life keeps the
// re-hydrated rows in m.instances, so the slugs stay protected regardless.
func runDeferredOrphanSweepIfReady(manager *Manager) {
	homeID, err := configDirForReap()
	if err != nil {
		return
	}
	manager.mu.Lock()
	if !manager.deferredOrphanSweepArmed || len(manager.skippedRepos) > 0 {
		manager.mu.Unlock()
		return
	}
	manager.deferredOrphanSweepArmed = false
	manager.mu.Unlock()
	log.InfoLog.Printf("orphan sweep: running the deferred destructive pass; every skipped repo's instances.json now parses, so the protected set is complete.")
	sweepOrphanContainers(homeID, manager.dockerReapProtectedSlugs())
}
