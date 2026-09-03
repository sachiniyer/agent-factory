package daemon

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The #3721 regression suite: a root-agent (re-)create ran synchronously on the
// instance poll goroutine, and its first act is an unbounded `git rev-parse`.
//
// EnsureRootAgents sits in the middle of poll_loop.go's pass sequence, between
// RefreshStatuses and RestoreLostSessions. A root create reaches CreateSession →
// reserveCreate, whose first act is config.RepoFromPath — "itself an unbounded
// `git rev-parse`", as daemon/manager.go says in as many words: no context, no
// deadline. On a hard-mounted NFS export or a dead FUSE filesystem that call
// never returns, and with it the poll goroutine. Every session on the box then
// loses its status refresh, its Lost-restore, and its settlement retries —
// because ONE configured repo's checkout stopped answering.
//
// poll_loop.go priced this as bounded ("a (re-)create blocks this poll briefly
// while the session starts — acceptable for a rare, backoff-throttled event"),
// which is true for a healthy filesystem and has no upper bound on a stalled
// one.

// repoResolutionStall makes reserveCreate's repo resolution never return, for
// one repo path.
//
// The seam is the repoFromPathForCreate package var rather than a `git` shim on
// PATH, for two reasons. It is the exact call the issue names, so what stalls
// here is the reported failure and not a lookalike one layer down; and PATH is
// process-global, so a shim would reach every other test in this package
// instead of the one repo under test. create_prelock_resolution_test.go stalls
// the same var for the #2947 window.
type repoResolutionStall struct {
	// entries counts how many creates have reached the stalled resolution. It
	// is the create COUNT the second red asserts on, and it is incremented
	// before the block, so it counts creates that started rather than creates
	// that finished.
	entries atomic.Int64
	release chan struct{}
	once    sync.Once
}

// stallRepoResolutionFor blocks every create whose repo path is repoPath inside
// repoFromPathForCreate until the returned stall is unblocked. Creates for any
// other path run the real resolution.
func stallRepoResolutionFor(t *testing.T, repoPath string) *repoResolutionStall {
	t.Helper()
	stall := &repoResolutionStall{release: make(chan struct{})}
	prev := repoFromPathForCreate
	repoFromPathForCreate = func(path string) (*config.RepoContext, error) {
		if filepath.Clean(path) == filepath.Clean(repoPath) {
			stall.entries.Add(1)
			<-stall.release
		}
		return prev(path)
	}
	t.Cleanup(func() { repoFromPathForCreate = prev })
	return stall
}

func (s *repoResolutionStall) unblock() { s.once.Do(func() { close(s.release) }) }

// awaitEntries waits for at least n creates to have entered the stall.
func (s *repoResolutionStall) awaitEntries(t *testing.T, n int64, why string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for s.entries.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s (creates that reached the stalled resolution: %d, want %d)", why, s.entries.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// deadButRecoverableBackend is deadTmuxBackend plus recoverFakeBackend's
// success contract: a session whose tmux has vanished, which RefreshStatuses
// must therefore mark Lost and RestoreLostSessions must then recover. It is
// the observable end of the poll's pass sequence — the passes that run AFTER
// EnsureRootAgents.
type deadButRecoverableBackend struct {
	*session.FakeBackend
	mu sync.Mutex
	// aliveProbes counts the liveness probes RefreshStatuses makes, and recovers
	// counts the recoveries RestoreLostSessions makes. Both are MONOTONE, which
	// is why the assertions read them instead of reading the session's status:
	// this backend is permanently dead, so a recovered session is legitimately
	// marked Lost again by the very next tick, and any status the test sampled
	// would be a snapshot of a value the loop keeps flipping.
	aliveProbes int
	recovers    int
}

func (b *deadButRecoverableBackend) IsAlive(*session.Instance) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.aliveProbes++
	return false, nil
}

func (b *deadButRecoverableBackend) Recover(inst *session.Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recovers++
	inst.SetStatusForTest(session.Running)
	return nil
}

func (b *deadButRecoverableBackend) counts() (aliveProbes, recovers int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.aliveProbes, b.recovers
}

// requireRootCreated asserts the released create actually produced its session.
// It needs no polling because every caller has already joined the create, and
// that join is also the happens-before edge for reading what the create wrote.
func requireRootCreated(t *testing.T, manager *Manager, repoPath, why string) {
	t.Helper()
	if findRootInstance(t, manager, repoPath) == nil {
		t.Error(why)
	}
}

// TestStalledRootCreateDoesNotWedgeTheInstancePoll is the blast-radius red, and
// it drives the REAL poll loop rather than the individual passes, because the
// claim is about that goroutine and nothing else: a create wedged on a repo
// whose filesystem never answers must not stop the passes that come after it
// for a session in a completely unrelated repository.
//
// The unrelated session's tmux has vanished, so the production sequence the
// loop owes it is RefreshStatuses marking it Lost and RestoreLostSessions
// recovering it — both of which run strictly after EnsureRootAgents in
// poll_loop.go. On master the loop never returns from the ensure pass, so the
// recovery never happens and this test's wait expires.
func TestStalledRootCreateDoesNotWedgeTheInstancePoll(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)

	// The configured root agent, registry-backed: its create is what stalls.
	rootRepo := setupControlRepo(t)
	project := registerTestProject(t, rootRepo)
	writePersonalRootAgent(t, project.ID, "enabled = true")
	stall := stallRepoResolutionFor(t, rootRepo)

	// A different repository entirely, with a session the poll owes a recovery.
	strandedRepo := setupControlRepo(t)
	strandedID := repoID(t, strandedRepo)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	backend := &deadButRecoverableBackend{FakeBackend: session.NewFakeBackend()}
	// Registered Running so the pass has to TRANSITION it to Lost, rather than
	// merely leaving a pre-set status alone.
	registerStarted(t, manager, strandedID, strandedRepo, "stranded", backend, true, session.Running)

	stopCh := make(chan struct{})
	wg := &sync.WaitGroup{}
	startInstancePollLoop(manager, 50*time.Millisecond, stopCh, wg)
	// Teardown JOINS everything this test started, and the order is the whole
	// point. stallRepoResolutionFor registered its restore of the
	// repoFromPathForCreate package var BEFORE this cleanup, so LIFO runs that
	// restore after this one — and a restore that lands while a create is still
	// reading the var is a data race, which is exactly what the fail-first run
	// reported. It also leaked goroutines into the NEXT test in the package,
	// where the race surfaced under an unrelated test's name.
	t.Cleanup(func() {
		// Release before stopping: on master the loop is INSIDE the stalled
		// create, so it can only observe stopCh once the create returns.
		stall.unblock()
		close(stopCh)
		wg.Wait()                      // the poll loop is out, so nothing else launches
		manager.waitRootAgentCreates() // and so is the create it launched
		requireRootCreated(t, manager, rootRepo, "the released root create never registered its session")
	})

	stall.awaitEntries(t, 1, "the poll loop never attempted the configured root's create")

	// TWO probes, not one, is what discriminates. The wedge happens mid-tick:
	// RefreshStatuses runs before EnsureRootAgents, so even a poll goroutine that
	// never returns from the create has already probed this session ONCE. A second
	// probe can only come from a tick that got all the way round — which is the
	// claim. And the recovery can only come from RestoreLostSessions, which runs
	// after the ensure pass and never runs at all on master.
	deadline := time.Now().Add(30 * time.Second)
	for {
		aliveProbes, recovers := backend.counts()
		if aliveProbes >= 2 && recovers >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the instance poll goroutine is wedged inside a root create: with the create's repo resolution "+
				"stalled, the passes after EnsureRootAgents never ran for a session in an unrelated repository "+
				"(liveness probes=%d want >=2, recoveries=%d want >=1) (#3721)", aliveProbes, recovers)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSecondEnsureTickDuringInFlightRootCreateStartsNoSecondCreate is the other
// half: taking the create off the poll goroutine must not turn a one-second
// ensure cadence into a create storm. The next tick must neither block on the
// in-flight create nor start a second one — and it must charge the candidate's
// retry state nothing, because a create still running is not an outcome.
//
// On master the first tick owns the create, so the second tick blocks behind
// it; released, both run a create, and reserveCreate's title conflict is all
// that stops the second from existing.
func TestSecondEnsureTickDuringInFlightRootCreateStartsNoSecondCreate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)

	rootRepo := setupControlRepo(t)
	project := registerTestProject(t, rootRepo)
	writePersonalRootAgent(t, project.ID, "enabled = true")
	stall := stallRepoResolutionFor(t, rootRepo)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Same teardown discipline as the test above, for the same reason: the ticks
	// below are still wedged when an assertion fails, and the stall's restore of
	// the repoFromPathForCreate package var must not land while they are running.
	// ticks counts only the ones actually started, so a failure before the second
	// launch does not hang the wait.
	var ticks sync.WaitGroup
	t.Cleanup(func() {
		stall.unblock()
		ticks.Wait()
		manager.waitRootAgentCreates()
	})

	firstTick := make(chan struct{})
	ticks.Add(1)
	go func() { defer ticks.Done(); defer close(firstTick); manager.EnsureRootAgents() }()
	stall.awaitEntries(t, 1, "the first ensure tick never reached the create's repo resolution")

	secondTick := make(chan struct{})
	ticks.Add(1)
	go func() { defer ticks.Done(); defer close(secondTick); manager.EnsureRootAgents() }()
	select {
	case <-secondTick:
	case <-time.After(30 * time.Second):
		t.Fatal("a second ensure tick blocked behind the in-flight root create: the create still owns the poll goroutine (#3721)")
	}
	select {
	case <-firstTick:
	case <-time.After(30 * time.Second):
		t.Fatal("the ensure pass that launched the create never returned while the create's repo resolution was stalled (#3721)")
	}

	// Long enough for a second create to have reached the same resolution, since
	// the first one reached it within milliseconds of its tick.
	time.Sleep(2 * time.Second)
	if got := stall.entries.Load(); got != 1 {
		t.Fatalf("%d creates reached repo resolution, want exactly 1: a tick arriving during an in-flight create must start no second create (#3721)", got)
	}

	// An in-flight create is not an outcome, so it may charge the candidate's
	// retry state neither a failure nor a success.
	// Read the state by enumeration rather than by key: there is exactly one
	// candidate here, and its key is a resolved path this assertion has no
	// business re-deriving.
	manager.mu.Lock()
	states := make(map[string]rootEnsureState, len(manager.rootEnsureStates))
	for key, st := range manager.rootEnsureStates {
		states[key] = *st
	}
	manager.mu.Unlock()
	if len(states) != 1 {
		t.Fatalf("ensure states = %d, want exactly 1 candidate", len(states))
	}
	for key, st := range states {
		if st.consecutiveFailures != 0 || !st.nextAttempt.IsZero() {
			t.Fatalf("a tick that skipped an in-flight create charged %s's retry state (failures=%d nextAttempt=%v); it must charge nothing",
				key, st.consecutiveFailures, st.nextAttempt)
		}
	}

	stall.unblock()
	manager.waitRootAgentCreates()
	requireRootCreated(t, manager, rootRepo, "the released root create never registered its session")
	if len(*seen) != 1 {
		t.Fatalf("session.NewInstance was called %d times for one root, want 1", len(*seen))
	}
}
