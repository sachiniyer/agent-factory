package daemon

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The #3757 regression suite: the POLLING twin of #3721.
//
// #3721 moved the root-agent CREATE off the instance poll goroutine, because a
// create is an admission path whose unbounded `git rev-parse` must keep its
// full error contract. This is the other unbounded probe on that goroutine, and
// it is a different defect with a different remedy:
//
//   - it fires BEFORE any create decision — it is how the sweep learns repo.ID,
//     which the m.instances lookup is keyed by, so #3721's off-poll create
//     cannot help;
//   - it fires on EVERY non-backed-off tick, for a live and healthy adopted
//     root as much as for a missing one;
//   - and it is a POLLING use of the unbounded entry point, so the remedy is to
//     BOUND it. config.RepoFromPathContext's own doc states the split: "Polling
//     and registry scans use it so one unreachable checkout cannot indefinitely
//     block an unrelated live project; admission paths retain RepoFromPath's
//     full error contract and unbounded caller lifetime." Nothing is admitted
//     here, so there is no half-created object a deadline could strand.
//
// The blast radius is the same as #3721's: the whole poll goroutine, so
// RefreshStatuses, RestoreLostSessions and the settlement retries stop for every
// session on the box because one configured path went quiet.

// stallLegacyRootResolution makes the legacy sweep's repository resolution hang
// for one path until the caller's own context ends it — which is precisely the
// property under test, so the stall must never end on its own TIMER. A fake that
// returned after a fixed sleep would pass whether or not production bounded
// anything.
//
// The returned release exists only for teardown, and the fail-first run is what
// proved it necessary. An unbounded sweep can never end this stall, so the
// wedged poll goroutine held the test's wg.Wait() until the package's 20-minute
// timeout fired and every daemon test after it never ran. Release is closed in
// cleanup, strictly after the assertion has reached its verdict, so it cannot
// help the assertion pass — it only lets a wedged goroutine out so the package
// can finish reporting.
//
// The seam is the package var rather than a `git` shim on PATH because PATH is
// process-global and would reach every other test in this package; this stalls
// exactly one path in one test.
func stallLegacyRootResolution(t *testing.T, repoPath string) (release func()) {
	t.Helper()
	prev := legacyRootRepoFromPath
	released := make(chan struct{})
	var once sync.Once
	legacyRootRepoFromPath = func(ctx context.Context, path string) (*config.RepoContext, error) {
		if filepath.Clean(path) != filepath.Clean(repoPath) {
			return prev(ctx, path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-released:
			return prev(ctx, path)
		}
	}
	t.Cleanup(func() { legacyRootRepoFromPath = prev })
	return func() { once.Do(func() { close(released) }) }
}

// TestStalledLegacyRootPathDoesNotWedgeTheInstancePoll drives the REAL poll
// loop, because the claim is about that goroutine and nothing else: a
// root_agents path whose repository resolution never answers must not stop the
// passes that come after the ensure sweep, for a session in an unrelated
// repository.
//
// The unrelated session's tmux has vanished, so what the loop owes it is
// RefreshStatuses marking it Lost and RestoreLostSessions recovering it. Both
// run strictly after EnsureRootAgents in poll_loop.go, and on master the sweep
// never returns, so neither happens.
//
// Note the stall is installed AFTER NewManager. The start-of-day snapshot
// resolves every legacy path too (legacyRepoIDSet), and stalling that would
// wedge construction instead of the poll — a real but separate exposure, noted
// in the PR rather than conflated with this one.
func TestStalledLegacyRootPathDoesNotWedgeTheInstancePoll(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)

	rootRepo := setupControlRepo(t)
	strandedRepo := setupControlRepo(t)
	strandedID := repoID(t, strandedRepo)

	manager, err := NewManager(rootTestConfig(rootRepo, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	release := stallLegacyRootResolution(t, rootRepo)

	backend := &deadButRecoverableBackend{FakeBackend: session.NewFakeBackend()}
	// Registered Running so the pass has to TRANSITION it to Lost rather than
	// merely leaving a pre-set status alone.
	registerStarted(t, manager, strandedID, strandedRepo, "stranded", backend, true, session.Running)

	stopCh := make(chan struct{})
	wg := &sync.WaitGroup{}
	startInstancePollLoop(manager, 50*time.Millisecond, stopCh, wg)
	// Release BEFORE stopping. On an unbounded sweep the poll goroutine is
	// inside the stall and can only observe stopCh once the resolution returns,
	// so without this wg.Wait() blocks until the package timeout — measured, in
	// the fail-first run. It runs after the verdict either way.
	t.Cleanup(func() {
		release()
		close(stopCh)
		wg.Wait()
		manager.waitRootAgentCreates()
	})

	// TWO liveness probes, not one, is what discriminates. RefreshStatuses runs
	// BEFORE the ensure sweep, so even a poll goroutine that never returns from
	// the sweep has already probed this session once. A second probe can only
	// come from a tick that got all the way round.
	deadline := time.Now().Add(60 * time.Second)
	for {
		aliveProbes, recovers := backend.counts()
		if aliveProbes >= 2 && recovers >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the instance poll goroutine is wedged in the legacy root_agents sweep: with one configured path's "+
				"repository resolution unanswered, the passes after EnsureRootAgents never ran for a session in an "+
				"unrelated repository (liveness probes=%d want >=2, recoveries=%d want >=1) (#3757)", aliveProbes, recovers)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHealthyLegacyRootPathResolvesIdentically is the non-regression half, and
// it is a guard rather than a red: it holds before and after the bound, which is
// the whole claim. Bounding a probe must change nothing whatsoever for a
// checkout that answers.
//
// A differential oracle rather than a restatement of the expected fields: the
// resolution the sweep uses is compared against config.RepoFromPath's own, whole
// struct including unexported state, so any divergence the bounded path
// introduces fails here rather than being described into agreement.
func TestHealthyLegacyRootPathResolvesIdentically(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	want, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	got, err := resolveLegacyRootRepo(repoPath)
	if err != nil {
		t.Fatalf("the sweep's resolution failed on a healthy checkout: %v", err)
	}
	if !reflect.DeepEqual(*got, *want) {
		t.Fatalf("the sweep's resolution differs from config.RepoFromPath's on a healthy checkout:\n got %+v\nwant %+v", *got, *want)
	}

	// And end to end: the entry still materializes its root, under that identity.
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("a healthy legacy entry must still create exactly one root, got %d creates", len(*seen))
	}
	if findRootInstance(t, manager, repoPath) == nil {
		t.Fatalf("no root instance registered for the healthy legacy entry")
	}
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatalf("no ensure state recorded for %s", repoPath)
	}
	manager.mu.Lock()
	failures, nextAttempt := st.consecutiveFailures, st.nextAttempt
	manager.mu.Unlock()
	if failures != 0 || !nextAttempt.IsZero() {
		t.Fatalf("a healthy legacy entry must charge no retry state, got failures=%d nextAttempt=%v", failures, nextAttempt)
	}
}

// TestTimedOutLegacyRootProbeIsUnknownNotARefusal pins the contract half of the
// bound, and it uses NO seam: a 1ns budget against a real repository, resolved
// through the real config.RepoFromPathContext, so the classification under test
// is production's rather than a double's.
//
// The distinction it exists for is #3500's. "git answered, and the answer is no"
// and "we could not ask git" are different states, and only the first entitles
// anyone to say anything about the user's path. A bound that reported its own
// impatience as a verdict would be the #3500 defect re-entered through a
// timeout — so the timeout must record a failure that ESTABLISHED NOTHING,
// leave everything it might otherwise have acted on alone, and keep retrying.
func TestTimedOutLegacyRootProbeIsUnknownNotARefusal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	rid := repoID(t, repoPath)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A live, adopted root for that repo: what a refusal would be able to damage.
	live := registerStarted(t, manager, rid, repoPath, session.RootSessionTitle,
		session.NewFakeBackend(), true, session.Running)

	var warnings bytes.Buffer
	prevWarning := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(prevWarning) })

	prevTimeout := rootLegacyRepoProbeTimeout
	rootLegacyRepoProbeTimeout = time.Nanosecond
	t.Cleanup(func() { rootLegacyRepoProbeTimeout = prevTimeout })

	manager.ensureRootAgentsAndWait()

	// Nothing was acted on. The sweep returns before ensureResolvedRoot, so the
	// live root is neither adopted nor reaped nor replaced.
	if got := live.GetStatus(); got != session.Running {
		t.Fatalf("the live root's status = %v, want Running: an unanswered probe must not touch it", got)
	}
	if findRootInstance(t, manager, repoPath) != live {
		t.Fatal("the live root's record was replaced by a pass whose probe never answered")
	}
	if len(*seen) != 0 {
		t.Fatalf("an unanswered probe must create nothing, got %d creates", len(*seen))
	}

	// It was recorded as UNKNOWN, not as a verdict, and it backed off.
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	if st == nil {
		t.Fatalf("no ensure state recorded for %s", repoPath)
	}
	manager.mu.Lock()
	failures, unanswered, nextAttempt := st.consecutiveFailures, st.unansweredFailures, st.nextAttempt
	manager.mu.Unlock()
	if failures != 1 || unanswered != 1 {
		t.Fatalf("a timed-out probe must count as one failure that established nothing, got failures=%d unanswered=%d", failures, unanswered)
	}
	if !nextAttempt.After(time.Now()) {
		t.Fatalf("a timed-out probe must settle onto the ensure backoff, got nextAttempt=%v", nextAttempt)
	}

	// And it said so in #3500's words, without claiming anything about the path.
	got := warnings.String()
	if !strings.Contains(got, "git never answered the probe") {
		t.Fatalf("the log must use the unanswered-probe wording (#3500); warnings were:\n%s", got)
	}
	if strings.Contains(got, "does not resolve to a git repository") {
		t.Fatalf("a probe that never answered must not be reported as a verdict about the path; warnings were:\n%s", got)
	}

	// #1122: retry-forever. With the budget restored and the backoff elapsed,
	// the very next pass heals the candidate rather than staying refused.
	rootLegacyRepoProbeTimeout = prevTimeout
	manager.mu.Lock()
	st.nextAttempt = time.Time{}
	manager.mu.Unlock()

	manager.ensureRootAgentsAndWait()

	manager.mu.Lock()
	failures, unanswered = st.consecutiveFailures, st.unansweredFailures
	manager.mu.Unlock()
	if failures != 0 || unanswered != 0 {
		t.Fatalf("the candidate must heal once the probe answers again, got failures=%d unanswered=%d", failures, unanswered)
	}
	if got := live.GetStatus(); got != session.Running {
		t.Fatalf("the healed pass must adopt the live root, not replace it; status = %v", got)
	}
}
