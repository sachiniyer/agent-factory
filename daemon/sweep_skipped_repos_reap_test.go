package daemon

import (
	"encoding/json"
	"fmt"
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

// TestSweepStartupOrphanContainers_ArmsWithoutRunningWhenSkippedReposEmpty pins
// #5815: with a complete session view the startup step must NOT run the Docker
// pass itself — doing so made daemon readiness wait on `docker info`/`docker ps`
// (30s each against a hung engine). It arms the sweep, which then runs after
// readiness on the deferred path with the same protected-slug set.
func TestSweepStartupOrphanContainers_ArmsWithoutRunningWhenSkippedReposEmpty(t *testing.T) {
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.instances[daemonInstanceKey("repo1", "live one")] = &session.Instance{Title: "live one"}

	sweepStartupOrphanContainers(m)

	assert.Equal(t, 0, rec.count(), "the startup step must not run the Docker pass before readiness")
	m.mu.Lock()
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed, "the sweep must be armed to run after readiness")

	runDeferredOrphanSweepIfReady(m)

	assert.Equal(t, 1, rec.count(), "the armed sweep runs once the deferred path is reached")
	call, ok := rec.lastCall()
	require.True(t, ok)
	assert.True(t, call.slugs[session.Slugify("live one")], "the protected set must include the live session's slug")
}

// TestSweepStartupOrphanContainers_NeverBlocksOnADockerEngine is the regression
// for #5815: a Docker pass that never returns (an unavailable engine) must not
// hold the startup step, because readiness waits on that step.
func TestSweepStartupOrphanContainers_NeverBlocksOnADockerEngine(t *testing.T) {
	stubConfigDirForReap(t, "/test/home")
	release := make(chan struct{})
	prev := sweepOrphanContainers
	sweepOrphanContainers = func(string, map[string]bool) session.OrphanSweepResult {
		<-release
		return session.OrphanSweepResult{}
	}
	t.Cleanup(func() {
		close(release)
		sweepOrphanContainers = prev
	})

	m := newBareManagerForSweep()
	done := make(chan struct{})
	go func() {
		sweepStartupOrphanContainers(m)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the startup orphan-sweep step blocked on the Docker pass; daemon readiness would wait on Docker (#5815)")
	}
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
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed, "the deferral flag must be armed so the poll loop retries")
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
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed)
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
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.False(t, sweepArmed, "an unresolvable home is not an incomplete-view deferral; the flag must stay clear")
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
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.False(t, sweepArmed)
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
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed, "the flag must stay armed for the next poll")
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
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.False(t, sweepArmed, "the flag is cleared so the sweep runs exactly once")

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
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed, "the deferral flag is armed for the poll loop")
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
	refreshErr := m.refreshLocked()
	skipped := m.skippedRepos
	repaired := m.instances[daemonInstanceKey("corrupt-r", "repaired")]
	m.mu.Unlock()
	require.NoError(t, refreshErr, "a polling refresh of a repaired repo must not error")
	require.Empty(t, skipped, "the repaired repo drops out of the skip set")
	require.NotNil(t, repaired,
		"the repaired session re-materializes in m.instances")

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
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.False(t, sweepArmed, "the deferral flag is cleared after the sweep runs")
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
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	require.True(t, sweepArmed)

	// First poll: file still corrupted → skip set stays non-empty → sweep stays deferred.
	m.mu.Lock()
	refreshErr := m.refreshLocked()
	skipped := skippedRepoIDs(m.skippedRepos)
	m.mu.Unlock()
	require.NoError(t, refreshErr)
	require.Equal(t, []string{"corrupt-r"}, skipped,
		"a still-corrupted repo stays in the skip set")
	runDeferredOrphanSweepIfReady(m)
	require.Equal(t, 0, rec.count(), "still-corrupted → sweep stays deferred")
	m.mu.Lock()
	sweepArmed2 := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	require.True(t, sweepArmed2, "flag stays armed across the poll")

	// Second poll: repair the file → skip set drains → deferred sweep runs.
	repairedJSON, err := json.Marshal([]session.InstanceData{{Title: "repaired"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("corrupt-r", repairedJSON))
	m.mu.Lock()
	refreshErr2 := m.refreshLocked()
	skipped2 := m.skippedRepos
	m.mu.Unlock()
	require.NoError(t, refreshErr2)
	require.Empty(t, skipped2, "repair drains the skip set")
	runDeferredOrphanSweepIfReady(m)
	require.Equal(t, 1, rec.count(), "repair → deferred sweep runs")
	m.mu.Lock()
	sweepArmed3 := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	require.False(t, sweepArmed3, "flag cleared after the sweep")
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
	sweepInFlight := m.deferredOrphanSweepInFlight
	m.mu.Unlock()
	assert.True(t, sweepInFlight, "the in-flight flag is set while the worker runs")

	close(release)

	// The worker is registered with wg, so wg.Wait joins it deterministically
	// rather than polling the in-flight flag.
	wg.Wait()
	m.mu.Lock()
	sweepInFlight2 := m.deferredOrphanSweepInFlight
	m.mu.Unlock()
	assert.False(t, sweepInFlight2, "the in-flight flag must clear once the worker exits")

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
	sweepInFlight := m.deferredOrphanSweepInFlight
	m.mu.Unlock()
	require.False(t, sweepInFlight, "the in-flight flag clears after the worker exits")

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
	sweepInFlight := m.deferredOrphanSweepInFlight
	m.mu.Unlock()
	assert.False(t, sweepInFlight, "the in-flight flag cleared after the worker joined")
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
	sweepInFlight := m.deferredOrphanSweepInFlight
	m.mu.Unlock()
	assert.False(t, sweepInFlight, "the in-flight flag cleared without running the sweep")
}

// --- Startup parses-but-unloadable rows: the destructive-sweep hole (#4998) ---

// TestSweepStartupOrphanContainers_DefersWhenSkippedReposIncludeRowsFailedToLoad
// pins the decision loop: the deferral gate fires for a rows-failed-to-load
// skip entry, not only the corrupted/unreadable reasons. A parses-but-
// unloadable repo contributes the same zero rows to the protected-slug set as a
// corrupted one, so the gate must treat it identically — the fix widens the
// startup skip set to carry this reason.
func TestSweepStartupOrphanContainers_DefersWhenSkippedReposIncludeRowsFailedToLoad(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.skippedRepos = []SkippedRepo{{RepoID: "unloadable-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 2}}
	m.instances[daemonInstanceKey("healthy", "alive")] = &session.Instance{Title: "alive"}

	sweepStartupOrphanContainers(m)

	assert.Equal(t, 0, rec.count(), "the sweep must NOT run when a rows-failed repo is skipped — its live containers cannot be distinguished from genuine orphans")
	m.mu.Lock()
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed, "the deferral flag must be armed so the poll loop retries")
}

// TestRunDeferredOrphanSweep_StaysDeferredWhenRowsFailedSkipSetStillNonEmpty
// mirrors TestRunDeferredOrphanSweep_StaysDeferredWhenSkipSetStillNonEmpty for
// the rows-failed reason: a deferral armed for an unloadable repo whose rows
// keep failing stays deferred across polls (the sweep never runs with an
// incomplete protected set).
func TestRunDeferredOrphanSweep_StaysDeferredWhenRowsFailedSkipSetStillNonEmpty(t *testing.T) {
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	m := newBareManagerForSweep()
	m.skippedRepos = []SkippedRepo{{RepoID: "unloadable-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}}
	m.deferredOrphanSweepArmed = true

	runDeferredOrphanSweepIfReady(m)

	assert.Equal(t, 0, rec.count(), "a rows-failed skip set still non-empty keeps the sweep deferred")
	m.mu.Lock()
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed, "the flag stays armed for the next poll")
}

// TestSweepStartup_ParsesButUnloadableRepoDefersSweep_EndToEnd is the headline
// repro for the destructive-sweep hole: a repo whose instances.json PARSES but
// whose row fails to materialize (a docker-backed row with a corrupted
// backend_type discriminator, a worktree that is gone, etc.) contributes zero
// rows to m.instances — yet pre-fix contributed zero entries to the startup
// skip set too, because retractRereadOnUnloadableRows seeded the set only on the
// polling path. The startup sweep then saw an empty skip set, ran the
// destructive docker pass with an incomplete protected-slug set, and
// force-removed the repo's still-running container. Post-fix the startup skip
// set carries the rows-failed-to-load entry, so sweepStartupOrphanContainers
// defers exactly as it does for a corrupted/unreadable file, sparing the live
// container until the poll loop re-materializes the row.
func TestSweepStartup_ParsesButUnloadableRepoDefersSweep_EndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	// A healthy repo (its row loads) and a repo whose single row fails to
	// materialize. The failing row carries a stable ID so it bypasses the
	// legacy-ID block and reaches fromInstanceDataForRefresh directly (the
	// production drop path the bug report names).
	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok", ID: "id-ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	unloadableJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("stuck-r", unloadableJSON))
	failingFromInstanceForRefreshFor(t, "stuck")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err, "NewManager must start despite a parses-but-unloadable repo")

	// The startup skip set carries the unloadable repo with the rows-failed
	// reason and count — the signal the pre-fix gate was missing.
	require.Equal(t, []SkippedRepo{{RepoID: "stuck-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}},
		m.skippedRepos, "startup must seed the skip set with the parses-but-unloadable repo")
	require.NotNil(t, m.instances[daemonInstanceKey("valid-r", "ok")], "the healthy repo loads normally")
	require.Nil(t, m.instances[daemonInstanceKey("stuck-r", "stuck")],
		"the unloadable row contributes zero rows to m.instances — its container's slug is absent from the protected set")

	// The startup sweep defers: it does NOT force-remove the unloadable repo's
	// still-running container.
	sweepStartupOrphanContainers(m)
	assert.Equal(t, 0, rec.count(),
		"the sweep must NOT run: a parses-but-unloadable repo's live containers cannot be distinguished from genuine orphans")
	m.mu.Lock()
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed, "the deferral flag is armed for the poll loop")
}

// TestSweepStartup_PartialRowFailureAtStartupDefersSweep_EndToEnd pins the
// 1-of-N case the bug report's "partial loss is the same lie as a total one"
// rationale extends to the sweep: a repo whose instances.json parses, one row
// loads and one fails, still leaves the failing row's container's slug absent
// from the protected set, so the sweep must defer for the partial loss too.
func TestSweepStartup_PartialRowFailureAtStartupDefersSweep_EndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	partialJSON, err := json.Marshal([]session.InstanceData{
		{Title: "loads", ID: "id-loads"},
		{Title: "stuck", ID: "id-stuck"},
	})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("partial-r", partialJSON))
	failingFromInstanceForRefreshFor(t, "stuck")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	require.Equal(t, []SkippedRepo{{RepoID: "partial-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}},
		m.skippedRepos, "a partial loss seeds the startup skip set (1 of N=2 unloadable)")
	require.NotNil(t, m.instances[daemonInstanceKey("partial-r", "loads")],
		"the loadable row still materializes")
	require.Nil(t, m.instances[daemonInstanceKey("partial-r", "stuck")],
		"the unloadable row is dropped — its container's slug is absent from the protected set")

	sweepStartupOrphanContainers(m)
	assert.Equal(t, 0, rec.count(),
		"the sweep must defer for a partial loss too: the failing row's container cannot be distinguished from a genuine orphan")
	m.mu.Lock()
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed)
}

// TestDeferredSweep_RunsAfterUnloadableRowsRepair_EndToEnd exercises the
// self-healing half for the parses-but-unloadable class: after the startup
// sweep defers, re-materializing the unloadable row (the worktree is restored,
// the corrupted backend_type is repaired, etc.) via a polling refresh drains
// the skip set, so the next deferred-sweep check runs the destructive pass —
// now with the previously-unloadable session's slug in the protected set, so
// its container is spared while genuine orphans are reaped.
func TestDeferredSweep_RunsAfterUnloadableRowsRepair_EndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok", ID: "id-ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	unloadableJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("stuck-r", unloadableJSON))
	failingFromInstanceForRefreshFor(t, "stuck")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Startup sweep defers (skip set non-empty on the rows-failed reason).
	sweepStartupOrphanContainers(m)
	require.Equal(t, 0, rec.count(), "startup sweep defers for the parses-but-unloadable repo")

	// Restore the materializer so stuck-r's row loads again, and run a polling
	// refresh: the row re-materializes and the repo drops out of the skip set.
	stubFromInstanceForRefresh(t)
	m.mu.Lock()
	refreshErr := m.refreshLocked()
	skipped := m.skippedRepos
	stuck := m.instances[daemonInstanceKey("stuck-r", "stuck")]
	m.mu.Unlock()
	require.NoError(t, refreshErr, "a polling refresh that re-materializes the row must not error")
	require.Empty(t, skipped, "the repaired repo drops out of the skip set")
	require.NotNil(t, stuck,
		"the previously-unloadable session re-materializes in m.instances")

	// The deferred-sweep check now runs the destructive pass with a complete
	// protected set: the re-materialized session's slug is protected.
	runDeferredOrphanSweepIfReady(m)
	assert.Equal(t, 1, rec.count(), "the deferred sweep runs once the skip set drains")

	call, ok := rec.lastCall()
	require.True(t, ok)
	assert.True(t, call.slugs[session.Slugify("stuck")], "the repaired session's slug is now protected")
	assert.True(t, call.slugs[session.Slugify("ok")], "the healthy session's slug is still protected")

	m.mu.Lock()
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.False(t, sweepArmed, "the deferral flag is cleared after the sweep runs")
}

// TestDeferredSweep_StaysDeferredAcrossPollsWhileRowsStillUnloadable_EndToEnd
// pins the self-healing bound for the parses-but-unloadable class: while the
// row keeps failing poll after poll, the skip set stays non-empty and the
// deferred sweep keeps deferring (the sweep never runs with an incomplete
// protected set). The first poll that re-materializes the row drains the set
// and the deferred sweep runs.
func TestDeferredSweep_StaysDeferredAcrossPollsWhileRowsStillUnloadable_EndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "ok", ID: "id-ok"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("valid-r", validJSON))
	unloadableJSON, err := json.Marshal([]session.InstanceData{{Title: "stuck", ID: "id-stuck"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("stuck-r", unloadableJSON))
	failingFromInstanceForRefreshFor(t, "stuck")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	sweepStartupOrphanContainers(m)
	require.Equal(t, 0, rec.count(), "startup sweep defers")
	m.mu.Lock()
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	require.True(t, sweepArmed)

	// First poll: the row still fails -> skip set stays non-empty -> sweep stays deferred.
	m.mu.Lock()
	refreshErr := m.refreshLocked()
	skipped := skippedRepoIDs(m.skippedRepos)
	m.mu.Unlock()
	require.NoError(t, refreshErr)
	require.Equal(t, []string{"stuck-r"}, skipped,
		"a still-unloadable repo stays in the skip set")
	runDeferredOrphanSweepIfReady(m)
	require.Equal(t, 0, rec.count(), "still-unloadable -> sweep stays deferred")
	m.mu.Lock()
	sweepArmed2 := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	require.True(t, sweepArmed2, "flag stays armed across the poll")

	// Second poll: restore the materializer -> row re-materializes -> skip set drains -> deferred sweep runs.
	stubFromInstanceForRefresh(t)
	m.mu.Lock()
	refreshErr2 := m.refreshLocked()
	skipped2 := m.skippedRepos
	m.mu.Unlock()
	require.NoError(t, refreshErr2)
	require.Empty(t, skipped2, "repair drains the skip set")
	runDeferredOrphanSweepIfReady(m)
	require.Equal(t, 1, rec.count(), "repair -> deferred sweep runs")
	m.mu.Lock()
	sweepArmed3 := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	require.False(t, sweepArmed3, "flag cleared after the sweep")
}

// TestDeferredSweep_PartialRowFailureDrainsAfterRepair_EndToEnd is the
// regression test for the partial-loss shape the bug report's self-healing half
// must cover: a repo whose instances.json parses with N=2 rows where one row
// loads at startup and the other fails. The loadable row is re-hydrated from
// the prior in-memory map on every later poll without incrementing
// `materialized`, so a repair that re-materializes only the previously-failed
// row must STILL reach `materialized == len(data)` for the skip set to drain —
// otherwise the skip entry seeded at startup is retained forever and the
// deferred orphan sweep never runs again for the daemon's lifetime. Pre-fix,
// the re-hydrated row was not counted, so `materialized` stayed below
// `len(data)` even after repair and the repo was stuck skipped forever.
func TestDeferredSweep_PartialRowFailureDrainsAfterRepair_EndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	// A two-row repo: "loads" materializes at startup, "stuck" fails. Both
	// carry stable IDs so they reach fromInstanceDataForRefresh directly.
	partialJSON, err := json.Marshal([]session.InstanceData{
		{Title: "loads", ID: "id-loads"},
		{Title: "stuck", ID: "id-stuck"},
	})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("partial-r", partialJSON))
	failingFromInstanceForRefreshFor(t, "stuck")

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Startup: one row loads, one fails -> partial loss seeds the skip set.
	m.mu.Lock()
	skipped := m.skippedRepos
	loadsInst := m.instances[daemonInstanceKey("partial-r", "loads")]
	stuckInst := m.instances[daemonInstanceKey("partial-r", "stuck")]
	m.mu.Unlock()
	require.Equal(t, []SkippedRepo{{RepoID: "partial-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}},
		skipped, "a 1-of-2 partial loss seeds the startup skip set")
	require.NotNil(t, loadsInst,
		"the loadable row materializes at startup")
	require.Nil(t, stuckInst,
		"the unloadable row does not")

	// The startup sweep defers: the failing row's container's slug is absent
	// from the protected set.
	sweepStartupOrphanContainers(m)
	require.Equal(t, 0, rec.count(), "the sweep must defer for a partial loss")
	m.mu.Lock()
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	require.True(t, sweepArmed, "the deferral flag is armed")

	// A first poll where the row is still unloadable: the loadable row is
	// re-hydrated from the prior in-memory map (without incrementing
	// materialized) and the failing row still fails, so the skip set stays.
	m.mu.Lock()
	refreshErr := m.refreshLocked()
	skippedIDs := skippedRepoIDs(m.skippedRepos)
	m.mu.Unlock()
	require.NoError(t, refreshErr)
	require.Equal(t, []string{"partial-r"}, skippedIDs,
		"a still-unloadable partial repo stays skipped")
	runDeferredOrphanSweepIfReady(m)
	require.Equal(t, 0, rec.count(), "still-unloadable -> sweep stays deferred")
	m.mu.Lock()
	sweepArmed2 := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	require.True(t, sweepArmed2, "flag stays armed")

	// Repair: restore the materializer so "stuck" loads again. The loadable
	// row "loads" is re-hydrated from the prior in-memory map and the
	// previously-failing "stuck" re-materializes fresh. With the fix, BOTH
	// count toward `materialized`, so the repo reaches `materialized ==
	// len(data)`, stays in reread, and the skip set drains. Pre-fix, only the
	// freshly re-materialized "stuck" was counted, leaving `materialized` at
	// 1 < 2, so the repo was stuck skipped forever.
	stubFromInstanceForRefresh(t)
	m.mu.Lock()
	refreshErr2 := m.refreshLocked()
	skipped2 := m.skippedRepos
	loadsInst2 := m.instances[daemonInstanceKey("partial-r", "loads")]
	stuckInst2 := m.instances[daemonInstanceKey("partial-r", "stuck")]
	m.mu.Unlock()
	require.NoError(t, refreshErr2, "a polling refresh that re-materializes both rows must not error")
	require.Empty(t, skipped2, "a fully-repaired partial repo drains the skip set")
	require.NotNil(t, loadsInst2,
		"the loadable row is still present")
	require.NotNil(t, stuckInst2,
		"the previously-unloadable row re-materializes")

	// The deferred sweep now runs with both rows' slugs in the protected set.
	runDeferredOrphanSweepIfReady(m)
	require.Equal(t, 1, rec.count(), "the deferred sweep runs once the partial repo's skip set drains")

	call, ok := rec.lastCall()
	require.True(t, ok)
	assert.True(t, call.slugs[session.Slugify("loads")], "the loadable row's slug is protected")
	assert.True(t, call.slugs[session.Slugify("stuck")], "the repaired row's slug is protected")

	m.mu.Lock()
	sweepArmed3 := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	require.False(t, sweepArmed3, "the deferral flag is cleared after the sweep runs")

	// The wire surface stops refusing now that the skip set has drained: the
	// Snapshot RPC returns the full row set with no SkippedRepos, so list/get/
	// whoami no longer reject the partial list (they would have while the
	// rows-failed skip entry persisted).
	cs := &controlServer{manager: m}
	var resp SnapshotResponse
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: "partial-r"}, &resp))
	require.ElementsMatch(t, []string{"loads", "stuck"}, snapshotFilterTitles(resp.Instances),
		"after repair both rows are served and nothing is skipped")
	require.Empty(t, resp.SkippedRepos, "the wire refusal clears once the partial repo's skip set drains")
}

// TestSweepStartup_LegacyIDPersistenceFailureSeedsSkipSetAndDefers exercises
// the earlier drop path the rows-failed gate must also cover: a legacy row
// (empty ID) whose persistLegacyInstanceID cannot write — a read-only state file
// or a full disk at startup — is omitted from next without going through
// fromInstanceDataForRefresh, so the failedRows counter the deferral gate reads
// was never incremented. Pre-fix, the row contributed zero to materialized AND
// zero to failedRows, so the skip set was empty and the destructive sweep ran
// with the row's slug absent from the protected set. Post-fix, the dropped row
// counts as a failed row, so the skip set is seeded and the sweep defers.
func TestSweepStartup_LegacyIDPersistenceFailureSeedsSkipSetAndDefers(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	silenceWarnings(t)
	stubConfigDirForReap(t, "/test/home")
	rec := stubSweepOrphanContainers(t)

	// A legacy row (empty ID, non-transient) whose stable ID cannot be persisted.
	legacyJSON, err := json.Marshal([]session.InstanceData{{
		Title: "legacy-editor", Status: session.Archived, Liveness: session.LiveArchived,
	}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("legacy-r", legacyJSON))

	prevPersist := persistLegacyInstanceID
	persistLegacyInstanceID = func(string, session.InstanceData) error {
		return fmt.Errorf("forced stable ID persistence failure")
	}
	t.Cleanup(func() { persistLegacyInstanceID = prevPersist })
	stubFromInstanceForRefresh(t)

	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// The skip set is seeded with the rows-failed reason: the row's slug is
	// absent from the protected set, so the sweep must defer.
	require.Equal(t, []SkippedRepo{{RepoID: "legacy-r", Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}},
		m.skippedRepos, "a legacy row whose ID could not be persisted seeds the skip set")
	require.Nil(t, m.instances[daemonInstanceKey("legacy-r", "legacy-editor")],
		"the legacy row is not materialized")

	sweepStartupOrphanContainers(m)
	assert.Equal(t, 0, rec.count(),
		"the sweep must defer: the legacy row's container cannot be distinguished from a genuine orphan")
	m.mu.Lock()
	sweepArmed := m.deferredOrphanSweepArmed
	m.mu.Unlock()
	assert.True(t, sweepArmed, "the deferral flag is armed")
}
