package daemon

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// stubSweepOrphanContainers replaces sweepOrphanContainers with a recording
// stub so tests can verify whether the destructive pass ran and inspect the
// protected slugs it was handed. Restored on cleanup. The stub is goroutine-safe
// because the poll loop and the deferred sweep run on the poll goroutine while a
// test's own goroutines may inspect the recorded state.
func stubSweepOrphanContainers(t *testing.T) *sweepRecorder {
	t.Helper()
	rec := &sweepRecorder{}
	prev := sweepOrphanContainers
	sweepOrphanContainers = func(homeID string, slugs map[string]bool) session.OrphanSweepResult {
		rec.record(homeID, slugs)
		return session.OrphanSweepResult{}
	}
	t.Cleanup(func() { sweepOrphanContainers = prev })
	return rec
}

// sweepRecorder captures every call to the stubbed sweepOrphanContainers.
type sweepRecorder struct {
	mu    sync.Mutex
	calls []sweepCall
}

type sweepCall struct {
	homeID string
	slugs  map[string]bool
}

func (r *sweepRecorder) record(homeID string, slugs map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, sweepCall{homeID: homeID, slugs: slugs})
}

func (r *sweepRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *sweepRecorder) lastCall() (sweepCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return sweepCall{}, false
	}
	return r.calls[len(r.calls)-1], true
}

// stubConfigDirForReap pins configDirForReap to a fixed home so tests don't
// depend on the host's AGENT_FACTORY_HOME. Restored on cleanup.
func stubConfigDirForReap(t *testing.T, homeID string) {
	t.Helper()
	prev := configDirForReap
	configDirForReap = func() (string, error) { return homeID, nil }
	t.Cleanup(func() { configDirForReap = prev })
}

// newBareManagerForSweep builds a minimal Manager for the orphan-sweep decision
// loop: just the maps and the mu lock the sweep functions touch. No NewManager,
// no disk, no goroutines — fully hermetic.
func newBareManagerForSweep() *Manager {
	return &Manager{
		instances:      make(map[string]*session.Instance),
		pendingCreates: make(map[string]session.InstanceData),
	}
}

// --- sweepStartupOrphanContainers: the startup-side decision ---

// TestSweepStartupOrphanContainers_RunsWhenSkippedReposEmpty is the no-regression
// baseline: with no skipped repos the daemon's session view is complete, so the
// destructive pass runs immediately exactly as it did before the fix.
func TestSweepStartupOrphanContainers_RunsWhenSkippedReposEmpty(t *testing.T) {
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.instances[daemonInstanceKey("repo1", "live one")] = &session.Instance{Title: "live one"}

	sweepStartupOrphanContainers(m)

	assert.Equal(t, 1, rec.count(), "the sweep must run when no repo is skipped")
	m.mu.Lock()
	assert.False(t, m.deferredOrphanSweepArmed, "the deferral flag must NOT be armed")
	m.mu.Unlock()

	call, ok := rec.lastCall()
	require.True(t, ok)
	assert.True(t, call.slugs[session.Slugify("live one")], "the protected set must include the live session's slug")
}

// TestSweepStartupOrphanContainers_DefersWhenSkippedReposNonEmpty is the core
// fix: with a skipped repo the daemon's session view is known-incomplete (the
// repo's live containers contributed zero rows to m.instances), so the
// destructive pass is deferred rather than force-removing containers it cannot
// distinguish from genuine orphans. The deferral flag is armed for the poll loop.
func TestSweepStartupOrphanContainers_DefersWhenSkippedReposNonEmpty(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.skippedRepos = []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonCorruptedInstancesJSON}}
	m.instances[daemonInstanceKey("healthy", "alive")] = &session.Instance{Title: "alive"}

	sweepStartupOrphanContainers(m)

	assert.Equal(t, 0, rec.count(), "the sweep must NOT run when a repo is skipped — its live containers cannot be distinguished from genuine orphans")
	m.mu.Lock()
	assert.True(t, m.deferredOrphanSweepArmed, "the deferral flag must be armed so the poll loop retries")
	m.mu.Unlock()
}

// TestSweepStartupOrphanContainers_DefersForMultipleSkippedRepos verifies the
// deferral fires for any non-empty skip set, not just a single repo.
func TestSweepStartupOrphanContainers_DefersForMultipleSkippedRepos(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.skippedRepos = []SkippedRepo{
		{RepoID: "corrupt-r", Reason: SkippedRepoReasonCorruptedInstancesJSON},
		{RepoID: "unreadable-r", Reason: SkippedRepoReasonUnreadableInstancesJSON},
	}

	sweepStartupOrphanContainers(m)

	assert.Equal(t, 0, rec.count(), "the sweep must defer for any non-empty skip set")
	m.mu.Lock()
	assert.True(t, m.deferredOrphanSweepArmed)
	m.mu.Unlock()
}

// TestSweepStartupOrphanContainers_DoesNotArmWhenHomeUnresolvable preserves the
// pre-existing behavior: when the AF home cannot be resolved the sweep is
// skipped (there is nothing to scope to), and the deferral flag must NOT be
// armed — the sweep did not defer for an incomplete view, it skipped for an
// unresolvable home, which is a different (non-recoverable-on-poll) condition.
func TestSweepStartupOrphanContainers_DoesNotArmWhenHomeUnresolvable(t *testing.T) {
	silenceWarnings(t)
	rec := stubSweepOrphanContainers(t)
	prev := configDirForReap
	configDirForReap = func() (string, error) { return "", assertErr("home unresolvable") }
	t.Cleanup(func() { configDirForReap = prev })

	m := newBareManagerForSweep()
	m.skippedRepos = []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonCorruptedInstancesJSON}}

	sweepStartupOrphanContainers(m)

	assert.Equal(t, 0, rec.count())
	m.mu.Lock()
	assert.False(t, m.deferredOrphanSweepArmed, "an unresolvable home is not an incomplete-view deferral; the flag must stay clear")
	m.mu.Unlock()
}

// --- runDeferredOrphanSweepIfReady: the poll-side recovery ---

// TestRunDeferredOrphanSweep_NoOpWhenNotArmed: with no deferral armed, the
// deferred-sweep check is a complete no-op — the sweep does not run and the flag
// stays clear. This is the steady state for a clean startup.
func TestRunDeferredOrphanSweep_NoOpWhenNotArmed(t *testing.T) {
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.instances[daemonInstanceKey("repo1", "live")] = &session.Instance{Title: "live"}

	runDeferredOrphanSweepIfReady(m)

	assert.Equal(t, 0, rec.count(), "no deferral armed → no sweep")
	m.mu.Lock()
	assert.False(t, m.deferredOrphanSweepArmed)
	m.mu.Unlock()
}

// TestRunDeferredOrphanSweep_StaysDeferredWhenSkipSetStillNonEmpty: the deferral
// was armed but the skip set has not drained (the corrupted file is still
// corrupted). The sweep stays deferred and re-evaluates on the next poll.
func TestRunDeferredOrphanSweep_StaysDeferredWhenSkipSetStillNonEmpty(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.skippedRepos = []SkippedRepo{{RepoID: "corrupt-r", Reason: SkippedRepoReasonCorruptedInstancesJSON}}
	m.deferredOrphanSweepArmed = true

	runDeferredOrphanSweepIfReady(m)

	assert.Equal(t, 0, rec.count(), "skip set still non-empty → sweep stays deferred")
	m.mu.Lock()
	assert.True(t, m.deferredOrphanSweepArmed, "the flag must stay armed for the next poll")
	m.mu.Unlock()
}

// TestRunDeferredOrphanSweep_RunsWhenArmedAndSkipSetDrained: the deferral was
// armed and every skipped repo has been repaired (the poll refresh re-read and
// parsed its instances.json, draining the skip set). The sweep runs — with a
// complete protected set — and the flag is cleared so it runs exactly once.
func TestRunDeferredOrphanSweep_RunsWhenArmedAndSkipSetDrained(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.deferredOrphanSweepArmed = true
	m.instances[daemonInstanceKey("repaired-r", "repaired-sess")] = &session.Instance{Title: "repaired-sess"}

	runDeferredOrphanSweepIfReady(m)

	assert.Equal(t, 1, rec.count(), "skip set drained → the deferred sweep runs")
	m.mu.Lock()
	assert.False(t, m.deferredOrphanSweepArmed, "the flag is cleared so the sweep runs exactly once")
	m.mu.Unlock()

	call, ok := rec.lastCall()
	require.True(t, ok)
	assert.True(t, call.slugs[session.Slugify("repaired-sess")], "the repaired session's slug must be protected now that the view is complete")
}

// TestRunDeferredOrphanSweep_RunsAtMostOnce: after the deferred sweep runs and
// the flag is cleared, a second check is a no-op — the sweep does not run again.
func TestRunDeferredOrphanSweep_RunsAtMostOnce(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.deferredOrphanSweepArmed = true
	m.instances[daemonInstanceKey("repaired-r", "repaired-sess")] = &session.Instance{Title: "repaired-sess"}

	runDeferredOrphanSweepIfReady(m)
	assert.Equal(t, 1, rec.count(), "first check runs the deferred sweep")

	runDeferredOrphanSweepIfReady(m)
	assert.Equal(t, 1, rec.count(), "second check is a no-op — the flag is cleared")
}

// --- End-to-end: the full restore → sweep → repair → deferred-sweep chain ---

// TestSweepStartup_SkippedRepoLiveContainerIsSpared_EndToEnd exercises the full
// causal chain the bug report names: a real corrupted instances.json on disk →
// NewManager (production restore path) seeds m.skippedRepos and contributes zero
// rows → sweepStartupOrphanContainers defers rather than reaping. Pre-fix the
// sweep ran unconditionally and force-removed the skipped repo's live container;
// post-fix the deferral spares it until the file is repaired.
func TestSweepStartup_SkippedRepoLiveContainerIsSpared_EndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err, "NewManager must start despite a corrupted repo")

	// The production restore path seeded the skip set and left the corrupted
	// repo out of m.instances (its live containers' slugs are absent from
	// the protected set).
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
		"startup must seed the skip set with the corrupted repo")
	require.Nil(t, m.instances[daemonInstanceKey("corrupt-r", "anything")],
		"corrupted repo must contribute zero rows")
	require.NotNil(t, m.instances[daemonInstanceKey("valid-r", "ok")],
		"healthy repo loads normally")

	// The startup sweep defers — it does NOT force-remove the skipped repo's
	// live containers.
	sweepStartupOrphanContainers(m)
	assert.Equal(t, 0, rec.count(),
		"the sweep must NOT have run: a skipped repo's live containers cannot be distinguished from genuine orphans")
	m.mu.Lock()
	assert.True(t, m.deferredOrphanSweepArmed, "the deferral flag is armed for the poll loop")
	m.mu.Unlock()
}

// TestDeferredSweep_RunsAfterRepair_EndToEnd exercises the self-healing half:
// after the startup sweep defers, repairing the corrupted instances.json and
// running a polling refresh (refreshLocked) drains the skip set and
// re-materializes the session, so the next deferred-sweep check runs the
// destructive pass — now with the repaired session's slug in the protected set,
// so its container is spared while genuine orphans are reaped.
func TestDeferredSweep_RunsAfterRepair_EndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Startup sweep defers (skip set non-empty).
	sweepStartupOrphanContainers(m)
	require.Equal(t, 0, rec.count(), "startup sweep defers for the corrupted repo")

	// Repair the corrupted file and run a polling refresh: the repo re-materializes
	// its session and drops out of the skip set.
	repairedJSON, err := json.Marshal([]session.InstanceData{{Title: "repaired"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairedJSON))

	m.mu.Lock()
	require.NoError(t, m.refreshLocked(), "a polling refresh of a repaired repo must not error")
	require.Empty(t, m.skippedRepos, "the repaired repo drops out of the skip set")
	require.NotNil(t, m.instances[daemonInstanceKey("corrupt-r", "repaired")],
		"the repaired session re-materializes in m.instances")
	m.mu.Unlock()

	// The deferred-sweep check now runs the destructive pass — with a complete
	// view. The repaired session's slug is in the protected set, so its container
	// would be spared; a genuine orphan (a slug not in the set) would be reaped.
	runDeferredOrphanSweepIfReady(m)
	assert.Equal(t, 1, rec.count(), "the deferred sweep runs once the skip set drains")

	call, ok := rec.lastCall()
	require.True(t, ok)
	assert.True(t, call.slugs[session.Slugify("repaired")], "the repaired session's slug is now protected")
	assert.True(t, call.slugs[session.Slugify("ok")], "the healthy session's slug is still protected")

	m.mu.Lock()
	assert.False(t, m.deferredOrphanSweepArmed, "the deferral flag is cleared after the sweep runs")
	m.mu.Unlock()
}

// TestDeferredSweep_StaysDeferredAcrossPollsWhileStillCorrupted_EndToEnd pins
// the self-healing bound: while the file stays corrupted the deferred sweep
// keeps deferring across polls (the sweep does not run with an incomplete view),
// and runs the first poll after the repair drains the skip set.
func TestDeferredSweep_StaysDeferredAcrossPollsWhileStillCorrupted_EndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubFromInstanceForRefresh(t)
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	seedCorruptedRepo(t, "corrupt-r")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	sweepStartupOrphanContainers(m)
	require.Equal(t, 0, rec.count(), "startup sweep defers")
	m.mu.Lock()
	require.True(t, m.deferredOrphanSweepArmed)
	m.mu.Unlock()

	// First poll: file still corrupted → skip set stays non-empty → sweep stays deferred.
	m.mu.Lock()
	require.NoError(t, m.refreshLocked())
	require.Equal(t, []string{"corrupt-r"}, skippedRepoIDs(m.skippedRepos),
		"a still-corrupted repo stays in the skip set")
	m.mu.Unlock()
	runDeferredOrphanSweepIfReady(m)
	require.Equal(t, 0, rec.count(), "still-corrupted → sweep stays deferred")
	m.mu.Lock()
	require.True(t, m.deferredOrphanSweepArmed, "flag stays armed across the poll")
	m.mu.Unlock()

	// Second poll: repair the file → skip set drains → deferred sweep runs.
	repairedJSON, err := json.Marshal([]session.InstanceData{{Title: "repaired"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairedJSON))
	m.mu.Lock()
	require.NoError(t, m.refreshLocked())
	require.Empty(t, m.skippedRepos, "repair drains the skip set")
	m.mu.Unlock()
	runDeferredOrphanSweepIfReady(m)
	require.Equal(t, 1, rec.count(), "repair → deferred sweep runs")
	m.mu.Lock()
	require.False(t, m.deferredOrphanSweepArmed, "flag cleared after the sweep")
	m.mu.Unlock()
}

// assertErr is a tiny sentinel error for stubs that need a non-nil error.
type assertErr string

func (e assertErr) Error() string { return string(e) }

// --- launchDeferredOrphanSweepIfReady: the poll-loop worker dispatch ---

// blockingSweepStub replaces sweepOrphanContainers with a stub that records the
// call and blocks until the test releases it via releaseCh, so a test can
// observe the worker while it is still in flight. Restored on cleanup.
func blockingSweepStub(t *testing.T) (*sweepRecorder, chan<- struct{}) {
	t.Helper()
	rec := &sweepRecorder{}
	release := make(chan struct{})
	prev := sweepOrphanContainers
	sweepOrphanContainers = func(homeID string, slugs map[string]bool) session.OrphanSweepResult {
		rec.record(homeID, slugs)
		<-release
		return session.OrphanSweepResult{}
	}
	t.Cleanup(func() { sweepOrphanContainers = prev })
	return rec, release
}

// TestLaunchDeferredOrphanSweep_RunsOnWorkerWithoutBlockingLauncher verifies the
// launcher returns immediately (the sweep runs on a separate goroutine): with
// the sweep stub blocked, launchDeferredOrphanSweepIfReady returns and the
// recorder still observes the call once the worker is released.
func TestLaunchDeferredOrphanSweep_RunsOnWorkerWithoutBlockingLauncher(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec, release := blockingSweepStub(t)

	m := newBareManagerForSweep()
	m.deferredOrphanSweepArmed = true
	m.instances[daemonInstanceKey("repaired-r", "repaired-sess")] = &session.Instance{Title: "repaired-sess"}

	stopCh := make(chan struct{})
	wg := &sync.WaitGroup{}

	// The launcher must return even though the sweep worker is blocked inside
	// sweepOrphanContainers.
	done := make(chan struct{})
	go func() {
		launchDeferredOrphanSweepIfReady(m, stopCh, wg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("launchDeferredOrphanSweepIfReady blocked while the sweep worker was still in flight")
	}

	// The worker is in flight and holding createSweepMu.
	m.mu.Lock()
	assert.True(t, m.deferredOrphanSweepInFlight, "the in-flight flag is set while the worker runs")
	m.mu.Unlock()

	close(release)

	// The worker is registered with wg, so wg.Wait joins it deterministically
	// rather than polling the in-flight flag.
	wg.Wait()
	m.mu.Lock()
	assert.False(t, m.deferredOrphanSweepInFlight, "the in-flight flag must clear once the worker exits")
	m.mu.Unlock()

	assert.Equal(t, 1, rec.count(), "the deferred sweep ran exactly once on the worker")
}

// TestLaunchDeferredOrphanSweep_DoesNotLaunchSecondWorkerWhileOneInFlight
// verifies the in-flight tracking: while the sweep worker is blocked, a second
// launch attempt is a no-op and does not queue a second sweep.
func TestLaunchDeferredOrphanSweep_DoesNotLaunchSecondWorkerWhileOneInFlight(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec, release := blockingSweepStub(t)

	m := newBareManagerForSweep()
	m.deferredOrphanSweepArmed = true
	m.instances[daemonInstanceKey("repaired-r", "repaired-sess")] = &session.Instance{Title: "repaired-sess"}

	stopCh := make(chan struct{})
	wg := &sync.WaitGroup{}

	launchDeferredOrphanSweepIfReady(m, stopCh, wg)
	// Give the worker a moment to enter the blocked sweep.
	require.Eventually(t, func() bool {
		return rec.count() == 1
	}, 5*time.Second, 10*time.Millisecond, "the first worker entered the sweep")

	// A second launch while the first is in flight is a no-op.
	launchDeferredOrphanSweepIfReady(m, stopCh, wg)
	assert.Equal(t, 1, rec.count(), "no second worker is launched while one is in flight")

	close(release)
	wg.Wait()
	m.mu.Lock()
	require.False(t, m.deferredOrphanSweepInFlight, "the in-flight flag clears after the worker exits")
	m.mu.Unlock()

	// After the worker exits the flag is cleared, but the deferral flag was
	// consumed (armed cleared at commit), so a subsequent launch is still a no-op.
	launchDeferredOrphanSweepIfReady(m, stopCh, wg)
	assert.Equal(t, 1, rec.count(), "armed was cleared at commit, so no further sweep launches")
}

// TestLaunchDeferredOrphanSweep_NoOpWhenNotArmed confirms a clean-startup steady
// state: with no deferral armed the launcher spawns no worker.
func TestLaunchDeferredOrphanSweep_NoOpWhenNotArmed(t *testing.T) {
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.instances[daemonInstanceKey("repo1", "live")] = &session.Instance{Title: "live"}

	launchDeferredOrphanSweepIfReady(m, make(chan struct{}), &sync.WaitGroup{})

	// No worker is spawned; assert via a short poll since the launcher is async.
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.deferredOrphanSweepInFlight
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, 0, rec.count(), "no deferral armed → no sweep")
}

// TestLaunchDeferredOrphanSweep_WorkerIsJoinedByShutdownWaitGroup verifies the
// worker is registered with RunDaemon's wait group so drainDaemon's wg.Wait()
// joins it rather than returning while a destructive Docker reap is still in
// flight (#4998): with the worker blocked inside the sweep, wg.Wait does not
// return until the worker exits.
func TestLaunchDeferredOrphanSweep_WorkerIsJoinedByShutdownWaitGroup(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	_, release := blockingSweepStub(t)

	m := newBareManagerForSweep()
	m.deferredOrphanSweepArmed = true
	m.instances[daemonInstanceKey("repaired-r", "repaired-sess")] = &session.Instance{Title: "repaired-sess"}

	stopCh := make(chan struct{})
	wg := &sync.WaitGroup{}

	launchDeferredOrphanSweepIfReady(m, stopCh, wg)

	// Wait for the worker to enter the (blocked) sweep, then close stopCh as
	// drainDaemon would. The worker is still mid-sweep, so wg.Wait must block.
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.deferredOrphanSweepInFlight
	}, 5*time.Second, 10*time.Millisecond, "the worker entered the sweep")

	wgDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
		t.Fatal("wg.Wait returned while the sweep worker was still in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-wgDone:
	case <-time.After(5 * time.Second):
		t.Fatal("wg.Wait did not return after the sweep worker exited")
	}

	m.mu.Lock()
	assert.False(t, m.deferredOrphanSweepInFlight, "the in-flight flag cleared after the worker joined")
	m.mu.Unlock()
}

// TestLaunchDeferredOrphanSweep_WorkerSkipsSweepWhenShutdownAlreadyRequested
// verifies the worker observes stopCh before starting the destructive pass: a
// worker launched after stopCh is closed bails without running the sweep, so a
// shutdown already in progress does not start a new reap.
func TestLaunchDeferredOrphanSweep_WorkerSkipsSweepWhenShutdownAlreadyRequested(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.deferredOrphanSweepArmed = true
	m.instances[daemonInstanceKey("repaired-r", "repaired-sess")] = &session.Instance{Title: "repaired-sess"}

	stopCh := make(chan struct{})
	close(stopCh)
	wg := &sync.WaitGroup{}

	launchDeferredOrphanSweepIfReady(m, stopCh, wg)

	// The worker observes the closed stopCh before the sweep and returns; wg
	// joins it deterministically.
	wg.Wait()
	assert.Equal(t, 0, rec.count(), "the sweep must not run when shutdown was requested before it began")
	m.mu.Lock()
	assert.False(t, m.deferredOrphanSweepInFlight, "the in-flight flag cleared without running the sweep")
	m.mu.Unlock()
}
