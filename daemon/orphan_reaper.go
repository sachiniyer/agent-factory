package daemon

import (
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// sweepOrphanContainers reaps docker session containers this daemon leaked, at
// startup (#2194 slice 4). Package-level so tests can stub out the docker work.
var sweepOrphanContainers = session.SweepOrphanContainers

// sweepOrphanSandboxTunnels reaps ssh -L transport processes a previous daemon
// generation for this home leaked (#5564). Package-level for the same reason.
var sweepOrphanSandboxTunnels = session.SweepOrphanSandboxTunnels

// sweepStaleTunnelSocketDirs removes tunnel socket dirs a previous daemon
// generation left under <home>/tunnels — only once the per-generation flock
// proves its owner exited, so a live predecessor (an upgrade hand-off) keeps
// its endpoints (#7495). Package-level for the same reason.
var sweepStaleTunnelSocketDirs = session.SweepStaleTunnelSocketDirs

// sweepStaleSetupPayloads removes af-setup/<id> staging dirs whose session id
// has no remaining handle — payloads a ghost kill, a post-hook create
// failure, or a dead daemon generation dropped without reclaiming (#6674
// review). It runs through the same deferred path as the container sweep:
// the skipped-repos defer applies identically, since an incomplete session
// view cannot distinguish a stale payload from one a live rebuild still
// consumes. Package-level so tests can stub the filesystem work.
var sweepStaleSetupPayloads = session.SweepStaleSetupPayloads

// sweepOrphanedStagingDirs removes docker-staging/<slug>-<hex> dirs no live
// session record claims, once no container on this engine can still bind them
// (#7590). It runs in the same deferred pass, under the same createSweepMu
// admission barrier and skipped-repos gate: its keep sets are built in the
// same breath as the container sweep's protected slugs, so a dir a mid-create
// session just staged is protected by its slug exactly as its container would
// be. Package-level so tests can stub the docker and filesystem work.
var sweepOrphanedStagingDirs = session.SweepOrphanedStagingDirs

// stopSandboxTunnels stops every live sandbox tunnel this daemon spawned.
// Registered as a RunDaemon defer at manager construction so every exit path —
// SIGTERM, the Shutdown RPC, an early warm-up return — runs it. A session's own
// teardown deliberately does not run on daemon exit (sessions must survive
// restarts), so without this a clean exit would orphan every live ssh -L
// tunnel exactly like a SIGKILL does. The session-side registry reaches
// tunnels no instance points at yet (a provision still in flight).
var stopSandboxTunnels = session.StopAllSandboxTunnels

// configDirForReap resolves the AF home the orphan sweep scopes its container
// query to. It must match the af.home label runContainer stamps.
var configDirForReap = config.GetConfigDir

// sweepStartupOrphanContainers arms the startup orphan-container sweep; it does
// NOT run it. It used to run the destructive pass right here, before the manager
// readiness barrier opened, which made daemon readiness wait on the Docker
// engine: the pass spends up to 30s on `docker info` and another 30s on
// `docker ps` against an unavailable engine, so a daemon with a hung or slow
// Docker sat in "starting (restoring sessions)" for every client (#5815). A
// housekeeping sweep must not gate readiness on an external dependency.
//
// The pass is armed through the same flag the incomplete-view deferral uses, so
// it runs after readiness on a tracked worker (launchDeferredOrphanSweepIfReady,
// launched by RunDaemon and re-tried by the poll loop). That path takes
// createSweepMu across the protected-slug snapshot AND the container listing, so
// a CreateSession admitted after the barrier opens cannot publish a container the
// stale protected set misses (#2632) — the guarantee the pre-readiness position
// used to give by excluding creates entirely.
//
// When skippedRepos is non-empty the daemon's session view is known-incomplete:
// a repo whose instances.json was corrupted/unreadable, OR parsed but whose
// rows failed to materialize, contributed zero (or a partial set of) rows to
// m.instances, so the protected-slug set omits that repo's still-running
// containers. Running the sweep in that state would force-remove live
// containers the sweep cannot distinguish from genuine orphans — the same
// "cannot distinguish orphaned from live, err toward sparing" principle the
// sweep's slug-collision branch already follows (session/orphan_reaper.go). The
// armed sweep therefore waits until the poll loop drains the skip set (a skipped
// repo whose instances.json now parses AND re-materializes its rows drains the
// set, so the protected set is complete again): genuine orphans survive
// un-reaped only until the view is complete, while a skipped repo's live
// container is never force-killed.
func sweepStartupOrphanContainers(manager *Manager) {
	if _, err := configDirForReap(); err != nil {
		log.WarningLog.Printf("orphan sweep: cannot resolve the AF home; skipping: %v", err)
		return
	}
	manager.mu.Lock()
	skipped := len(manager.skippedRepos)
	manager.deferredOrphanSweepArmed = true
	manager.mu.Unlock()
	if skipped > 0 {
		log.WarningLog.Printf("orphan sweep: deferring the destructive pass; %d repo(s) have an incomplete instances view (corrupted/unreadable instances.json or rows that failed to load) — their live containers cannot be distinguished from genuine orphans. The sweep will re-run once the skip set drains on the next polling refresh.", skipped)
	}
}

// sweepStartupOrphanSandboxTunnels runs beside sweepStartupOrphanContainers,
// before the readiness barrier opens: restore only loads inert backends and
// reprovision is a poll-loop activity, so no provision of THIS daemon can have
// started a tunnel yet — every transport process provably stamped with this
// home is a previous generation's leftover by position, not inference. Unlike
// the container sweep it needs no skippedRepos defer: ownership here is proven
// by the AF_SANDBOX_TUNNEL_HOME environment marker, never inferred from a
// session view that could be incomplete.
func sweepStartupOrphanSandboxTunnels() {
	homeID, err := configDirForReap()
	if err != nil {
		log.WarningLog.Printf("orphan tunnel sweep: cannot resolve the AF home; skipping: %v", err)
		return
	}
	canonical, err := canonicalDir(homeID)
	if err != nil {
		log.WarningLog.Printf("orphan tunnel sweep: cannot canonicalize the AF home; skipping: %v", err)
		return
	}
	sweepOrphanSandboxTunnels(canonical)
	// The socket-dir half: remove dead generations' endpoint paths once their
	// flock proves the owning daemon exited. Same "proof, never a guess" rule
	// as the process sweep — a live predecessor's dir is never touched.
	sweepStaleTunnelSocketDirs()
}

// runDeferredOrphanSweepIfReady runs the startup orphan sweep that was deferred
// because skippedRepos was non-empty, once the polling refresh has repaired
// every skipped repo (drained the skip set to empty). It is the synchronous
// sweep body invoked by launchDeferredOrphanSweepIfReady on a tracked worker
// goroutine, and by tests directly. A no-op when the startup sweep was not
// deferred or the skip set has not yet drained; if any repo is still skipped
// the sweep stays deferred and re-evaluates on the next poll.
//
// Unlike the old synchronous startup pass, this sweep runs AFTER the manager readiness
// barrier has opened (finishInstanceRestore), so a CreateSession
// can be admitted concurrently. A create sets pendingCreates under m.mu and only
// then provisions its container (manager_create.go), so without a guard a create
// admitted after the protected-slug snapshot but before SweepOrphanContainers
// lists containers would publish a container whose slug is absent from the
// stale protected set — the destructive pass would force-reap a live session
// (#2632). Both the startup-armed sweep (#5815) and the poll-time recovery
// use the dedicated createSweepMu admission barrier, held across the protected-slug
// snapshot AND the SweepOrphanContainers call: a create that has not yet set
// pendingCreates blocks on createSweepMu until the sweep releases it, and one
// that already set it is in the protected set. m.mu is released before the
// (potentially long) Docker list/reap so the barrier blocks only create
// admission, not Snapshot/RefreshInstances and the other manager operations
// that take m.mu alone — holding m.mu through external Docker work would stall
// the whole daemon on an unavailable engine. The slug collection is inlined
// because a factored helper would acquire m.mu itself.
func runDeferredOrphanSweepIfReady(manager *Manager) {
	homeID, err := configDirForReap()
	if err != nil {
		return
	}
	manager.createSweepMu.Lock()
	defer manager.createSweepMu.Unlock()
	manager.mu.Lock()
	if !manager.deferredOrphanSweepArmed || len(manager.skippedRepos) > 0 {
		manager.mu.Unlock()
		return
	}
	manager.deferredOrphanSweepArmed = false
	// Collect the protected slugs inline under the already-held m.mu (a factored
	// helper would acquire m.mu itself) so the snapshot and the sweep stay in
	// lockstep under createSweepMu. The setup-payload keep set is built in the
	// same breath — it is complete exactly when the slug set is, and incomplete
	// exactly when it is not.
	titles := make([]string, 0, len(manager.instances)+len(manager.pendingCreates))
	keepIDs := make(map[string]bool, len(manager.instances)+len(manager.pendingCreates))
	keepStaging := make(map[string]bool)
	for _, inst := range manager.instances {
		titles = append(titles, inst.Title)
		keepIDs[inst.ID] = true
		// The docker-staging keep set is per NAME, not per slug: a tombstoned
		// row still owns its <slug>-<hex> dir through the teardown handle,
		// while a slug alone cannot name which of two same-title provisions
		// staged it. Rows whose cleanup carries no staging claim contribute
		// nothing — a dir they never recorded is the orphan this pass exists
		// to reclaim.
		if name := inst.DockerStagingDirClaim(); name != "" {
			keepStaging[name] = true
		}
	}
	for key, pending := range manager.pendingCreates {
		keepIDs[pending.ID] = true
		if _, settled := manager.instances[key]; settled {
			continue
		}
		titles = append(titles, pending.Title)
	}
	slugs := make(map[string]bool, len(titles))
	for _, t := range titles {
		slugs[session.Slugify(t)] = true
	}
	log.InfoLog.Printf("orphan sweep: running the deferred destructive pass; every skipped repo's instances.json now parses AND its rows re-materialize, so the protected set is complete.")
	manager.mu.Unlock()
	sweepOrphanContainers(homeID, slugs)
	// Order matters: the container sweep runs FIRST so a same-slug orphan
	// container is already gone when the staging sweep lists — a dir whose
	// binder was just reaped is reclaimed in the same pass instead of waiting
	// out a spared cycle (#7590).
	sweepOrphanedStagingDirs(homeID, keepStaging, slugs)
	sweepStaleSetupPayloads(keepIDs)
}

// launchDeferredOrphanSweepIfReady starts the deferred orphan sweep on a
// separately tracked worker goroutine so the daemon's only polling goroutine is
// not blocked by the sweep's external Docker list/reap, which can spend 30
// seconds obtaining/listing Docker state and up to another 30 seconds per orphan
// in dockerProvisioner.reap — an unavailable engine or a large orphan set would
// otherwise suspend RefreshStatuses, lost-session recovery, settlement retries,
// and limit resumes for the whole window. The poll loop calls this instead of
// runDeferredOrphanSweepIfReady directly so those maintenance passes proceed
// while the sweep runs in parallel.
//
// A no-op when the startup sweep was not deferred, the skip set has not yet
// drained, or a deferred sweep worker is already in flight (deferredOrphanSweepInFlight);
// in the last case the in-flight worker is still holding the createSweepMu
// admission barrier, so re-launching would only queue a redundant goroutine on
// it. The create/sweep exclusion (#2632) is retained by the worker itself —
// runDeferredOrphanSweepIfReady still holds createSweepMu across the
// protected-slug snapshot and the SweepOrphanContainers call. m.mu guards only
// the launch decision and the in-flight flag; it is released before the worker
// is spawned, so a launch never blocks the poll loop on the manager lock.
//
// The worker is registered with the daemon's shutdown wait group (wg) and
// observes stopCh, so drainDaemon's wg.Wait() joins it rather than returning
// while a destructive Docker reap is still in flight: without registration a
// sweep started just before shutdown would outlive the terminal checkpoint and
// could be interrupted by process exit. stopCh is checked once before the sweep
// begins so a shutdown already in progress does not start a new destructive pass;
// the sweep itself (SweepOrphanContainers) is not cancellable mid-flight, so the
// wg registration — not stopCh — is what covers a sweep already running when
// drainDaemon closes stopCh.
func launchDeferredOrphanSweepIfReady(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	manager.mu.Lock()
	if !manager.deferredOrphanSweepArmed || len(manager.skippedRepos) > 0 || manager.deferredOrphanSweepInFlight {
		manager.mu.Unlock()
		return
	}
	manager.deferredOrphanSweepInFlight = true
	manager.mu.Unlock()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			manager.mu.Lock()
			manager.deferredOrphanSweepInFlight = false
			manager.mu.Unlock()
		}()
		select {
		case <-stopCh:
			return
		default:
		}
		runDeferredOrphanSweepIfReady(manager)
	}()
}
