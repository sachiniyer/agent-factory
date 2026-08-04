package daemon

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/autoupdate"
	"github.com/sachiniyer/agent-factory/log"
)

// The daemon-owned release check (#2212 R3 part two). A box that runs the daemon
// as an autostart unit — serving the web UI, firing cron and watch tasks — and
// never opens the TUI never reaches the launch-path updater at all, so it never
// learns it is behind. This loop is the daemon's own release check: the first
// piece of the update path the daemon owns rather than borrowing from an
// interactive launch.
//
// It DOES NOT INSTALL ANYTHING. It resolves the newest release on the configured
// channel and reports it. Activation — handing candidate bytes to
// triggerUpgradeActivation, which the transaction engine (R1/R2a/R2b) already
// implements and nothing yet calls — is deliberately a later slice, because
// switching it on requires resolving which of the two installers owns the binary
// (`af upgrade` / launch-time swap in place; the daemon would go through the
// journal + probation + rollback transaction). Two mechanisms racing to replace
// the same executable is a worse outcome than a box that is merely behind, and
// that reconciliation is an open product decision on #2212.
//
// The one property this slice must not break is the launch-path updater, which
// is still the only thing that installs. See windowOpen: this driver reads the
// shared throttle cache and never writes it.

var (
	// updateDriverWakeInterval is how often the loop re-evaluates. It is
	// deliberately far shorter than the six-hour check window: a wake is cheap
	// (a config read and, at most, one small file read), and a short wake is
	// what makes the off switch responsive — an operator who sets
	// auto_update = false is honoured within minutes instead of at the next
	// six-hour boundary.
	updateDriverWakeInterval = 15 * time.Minute
	// updateDriverCheckTimeout bounds the release lookup. Nobody is waiting on
	// this one — unlike the launch path, which is measured against a TUI that
	// has not opened yet — so it can afford `af upgrade`'s patience rather than
	// the launch path's two seconds.
	updateDriverCheckTimeout = 10 * time.Second
	// updateDriverStartupBackoff is how long after start the driver waits before
	// its first check is eligible. It is the whole check interval on purpose: the
	// self-throttle below lives in memory, so a restart resets it, and without a
	// startup floor a daemon that reaches ready and then dies repeatedly could
	// check once per restart — a rate-limit storm assembled out of individually
	// throttled processes. With the floor, a daemon must live a full interval to
	// make one call, so restarts can only ever produce FEWER checks, never more.
	// The cost is that a box which restarts its daemon more often than this never
	// reports; that is the right trade for a surface that only reports.
	updateDriverStartupBackoff = autoupdate.CheckInterval
	// updateDriverGOOS is the platform gate, a var so tests can exercise it.
	updateDriverGOOS = runtime.GOOS
)

// updateCheckOutcome is what one wake decided. Returned so tests can assert the
// branch taken rather than inferring it from a log line.
type updateCheckOutcome int

const (
	// updateCheckSkipped: no network call was made — disabled, not yet due, or
	// another af holds the update lock.
	updateCheckSkipped updateCheckOutcome = iota
	// updateCheckUpToDate: the channel's newest release is not newer than us.
	updateCheckUpToDate
	// updateCheckFailed: the release lookup itself failed.
	updateCheckFailed
	// updateCheckAvailable: a newer release exists. Reported, not installed.
	updateCheckAvailable
)

// updateDriver is the daemon's release-check loop. Every collaborator is a field
// so the whole cycle is drivable in a test without a network, a clock, or a
// Manager.
type updateDriver struct {
	// cachePath is the shared throttle cache (last_update_check), read-only.
	// Empty disables the driver: with no resolvable home there is no shared
	// window to coordinate on, and an uncoordinated checker is exactly what the
	// throttle exists to prevent.
	cachePath string
	// currentVersion is this daemon's build version, without a leading "v".
	currentVersion string
	// config returns the daemon's live config, re-read on every wake so
	// auto_update and update_channel take effect without a daemon restart.
	config func() *config.Config
	// discover resolves the newest release tag on a channel.
	discover func(channel string, timeout time.Duration) (string, error)
	now      func() time.Time

	// nextCheckNotBefore suppresses this driver's OWN checks for one
	// CheckInterval after each one. Because the driver never records into the
	// shared cache (windowOpen), the shared window cannot throttle it — so it
	// throttles itself to the same six hours. The two bounds compose in one
	// direction only: the driver checks at most once per CheckInterval, and only
	// ever when the shared window is also open. It can never check more often
	// than an interactive af would.
	nextCheckNotBefore time.Time
}

// startUpdateDriver runs the release check for the lifetime of the daemon. Called
// once the daemon is fully ready, so a check can never compete with restore or
// delay the readiness barrier.
func startUpdateDriver(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	driver := newUpdateDriver(manager)
	wg.Add(1)
	go func() {
		defer wg.Done()
		driver.run(stopCh)
	}()
}

func newUpdateDriver(manager *Manager) *updateDriver {
	return &updateDriver{
		cachePath:      autoupdate.CheckCachePath(),
		currentVersion: strings.TrimPrefix(Version(), "v"),
		config:         manager.Config,
		discover:       discoverLatestReleaseTag,
		now:            func() time.Time { return time.Now().UTC() },
	}
}

// discoverLatestReleaseTag resolves the newest release on channel through the
// same endpoints and channel rules the launch path uses, so the daemon and an
// interactive af can never disagree about what "newest" means.
func discoverLatestReleaseTag(channel string, timeout time.Duration) (string, error) {
	discovery := autoupdate.Discovery{
		LatestReleaseURL: autoupdate.DefaultLatestReleaseAPIURL,
		ReleasesURL:      autoupdate.DefaultReleasesAPIURL,
	}
	return discovery.LatestReleaseTag(channel, timeout)
}

// run wakes on a jittered schedule until stopCh closes. The two gates at the top
// are permanent for the process, so they end the loop rather than being
// re-evaluated every wake; everything that an operator can change at runtime is
// re-read inside checkOnce.
func (d *updateDriver) run(stopCh <-chan struct{}) {
	// Auto-update has never supported Windows (#1002) and this driver installs
	// nothing that would change that, so a check would only cost a pointless
	// network call.
	if updateDriverGOOS == "windows" {
		log.InfoLog.Printf("auto-update: daemon release checks are not supported on %s", updateDriverGOOS)
		return
	}
	if d.cachePath == "" {
		log.WarningLog.Printf("auto-update: daemon release checks disabled — the agent-factory home could not be resolved, so the shared check window cannot be honoured")
		return
	}
	// A build whose version is not a release version — a bare test binary, a
	// local build with no version stamped — cannot be compared against a release
	// tag. IsNewer would answer false for every tag, so the check could only
	// ever be noise; say so once instead.
	if !autoupdate.IsValidVersion(d.currentVersion) {
		log.InfoLog.Printf("auto-update: daemon release checks disabled — this build reports version %q, which is not a release version", d.currentVersion)
		return
	}

	// Arm the self-throttle from start, not from the first check: see
	// updateDriverStartupBackoff.
	d.nextCheckNotBefore = d.now().Add(updateDriverStartupBackoff)

	timer := time.NewTimer(d.firstWake())
	defer timer.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-timer.C:
		}
		d.checkOnce()
		timer.Reset(updateDriverWakeInterval)
	}
}

// checkOnce runs one wake of the loop.
func (d *updateDriver) checkOnce() updateCheckOutcome {
	cfg := d.config()
	if cfg == nil {
		// No config means no honest answer about enablement or channel. Skip
		// rather than assume the defaults: a check nobody asked for is worse
		// than a check that waits for the next wake.
		return updateCheckSkipped
	}
	// Re-read on every wake, and honour AGENT_FACTORY_AUTO_UPDATE the same way
	// the launch path does (autoupdate.Enabled). This is the complete off
	// switch: disabled means no release lookup, no network call, nothing.
	if !autoupdate.Enabled(cfg) {
		return updateCheckSkipped
	}
	channel := autoupdate.NormalizeChannel(cfg.UpdateChannel)

	now := d.now()
	if now.Before(d.nextCheckNotBefore) {
		return updateCheckSkipped
	}
	// Probing the cache takes the shared file lock, and that path MkdirAll's the
	// lock file's parent — so on a home that has been deleted out from under this
	// daemon, a read-only probe would recreate it. That is not a cosmetic stray
	// directory: watchDaemonHome exits only after consecutive missing
	// observations and resets its counter on any successful stat, so recreating
	// the home keeps an abandoned daemon alive indefinitely (#1093, the leaked
	// daemon that fired a cron for 23 days) and resurrects the state the deletion
	// was meant to remove. Nothing about a release check is worth that; stand
	// down and let the home watcher own the shutdown.
	if !d.homeExists() {
		return updateCheckSkipped
	}
	due, acquired, err := d.windowOpen(channel, now)
	switch {
	case err != nil:
		// The shared window is unreadable, so there is nothing to coordinate
		// with — and checking anyway is exactly the uncoordinated behaviour the
		// throttle exists to prevent. Back off like any other outcome this wake
		// cannot act on, which also stops a permanently broken home (an
		// unwritable AF directory) from logging this every fifteen minutes
		// forever.
		d.nextCheckNotBefore = now.Add(autoupdate.CheckInterval)
		log.WarningLog.Printf("auto-update: daemon could not read the release-check cache: %v", err)
		return updateCheckSkipped
	case !acquired || !due:
		// Either another af owns the check right now or one happened recently.
		// Neither is this driver's outcome to back off for: the next wake should
		// find the window as it is then, not as it was now.
		return updateCheckSkipped
	}
	// Set BEFORE the lookup and on every outcome below. A failing GitHub, a
	// black-holed network, or a channel with no parseable release must all cost
	// one call per six hours, never one per wake — the retry-storm half of the
	// argument #459 → #1466 → #1861 settled, which moving the check off the
	// launch path does not change.
	d.nextCheckNotBefore = now.Add(autoupdate.CheckInterval)

	tag, err := d.discover(channel, updateDriverCheckTimeout)
	if err != nil {
		log.WarningLog.Printf("auto-update: daemon release check failed on the %s channel: %v", channel, err)
		return updateCheckFailed
	}
	latest := strings.TrimPrefix(tag, "v")
	// Never report a downgrade: a preview user switching back to stable resolves
	// an older tag here, and the same IsNewer ordering the installer uses keeps
	// this loop from calling that an update.
	if !autoupdate.IsNewer(latest, d.currentVersion) {
		log.InfoLog.Printf("auto-update: daemon release check found nothing newer than %s on the %s channel", d.currentVersion, channel)
		return updateCheckUpToDate
	}
	log.InfoLog.Printf("auto-update: %s is available on the %s channel · this daemon is running %s · the daemon does not install updates yet, so an interactive af launch or af upgrade applies it", latest, channel, d.currentVersion)
	return updateCheckAvailable
}

// windowOpen reports whether the SHARED six-hour window is open for channel, and
// whether the update lock was free.
//
// It is a strictly read-only use of the throttle cache, and that is the load-
// bearing decision in this slice. Recording a check closes the window for every
// other update trigger on the box — including the launch path, which is still
// the only thing that installs. A daemon that consumed the window without
// installing would leave a box whose af launches stop updating (the daemon wins
// the window on its own cadence; the interactive launch arrives to find it
// closed) in exchange for a driver that installs nothing: strictly worse than
// today, and exactly the "a window where neither upgrades" outcome #2212 rules
// out. So the invariant is: never close a window on behalf of an install you did
// not perform. When this driver gains activation it installs, and it becomes a
// full Due/Record participant under the same lock, failure closing the window
// included.
//
// Reading under the same non-blocking lock still buys the coordination that
// matters: the daemon defers to a check another af just made, and stands down
// while one is mid-flight. The lock is released before the network call, so this
// driver can never make a launching af skip its own check. Taking that lock does
// create the sibling .lock file, as every other user of the shared cache does —
// "never writes" is about the cache record, which is the thing that closes the
// window.
func (d *updateDriver) windowOpen(channel string, now time.Time) (due, acquired bool, err error) {
	acquired, err = autoupdate.TryWithCheckCache(d.cachePath, func(cache *autoupdate.CheckCache, _ time.Time) error {
		due = cache.Due(channel, d.currentVersion, now)
		return nil
	})
	if err != nil {
		return false, false, err
	}
	return due, acquired, nil
}

// homeExists reports whether the AF home still exists. Derived from cachePath
// rather than resolved separately, so the directory checked is exactly the one
// the cache lock would create. A stat error that is not "missing" (a permission
// problem, say) counts as present: the check below must only ever suppress work
// on a positive observation of absence, never on an inconclusive one.
func (d *updateDriver) homeExists() bool {
	_, err := os.Stat(filepath.Dir(d.cachePath))
	return err == nil || !os.IsNotExist(err)
}

// firstWake spreads the first check of a fleet across the wake interval. Every
// autostart daemon on every box starts at login or boot, so an unjittered first
// wake would point them all at the GitHub API within seconds of each other.
// Seeded by the home path rather than randomly so a given box's offset is stable
// across restarts and reproducible in a test.
func (d *updateDriver) firstWake() time.Duration {
	return updateDriverJitter(d.cachePath, updateDriverWakeInterval)
}

func updateDriverJitter(seed string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(seed))
	return time.Duration(hash.Sum64() % uint64(window))
}
