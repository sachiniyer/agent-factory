package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #3782 item 3 regression suite: the last of the four, the smallest, and
// the only one that is not a git probe.
//
// refreshRootClaudeConversation reads the Claude project transcript store —
// os.ReadDir plus a stat per entry — from the adopt branch of ensureResolvedRoot,
// on the instance poll goroutine, for a live and HEALTHY root. The 30s throttle
// (rootClaudeTranscriptInspectionDue) paces it; it bounds nothing. One stalled
// transcript store therefore stops RefreshStatuses, RestoreLostSessions and the
// settlement retries for every session on the box.
//
// THE REMEDY IS #3721'S, NOT #3760'S, and the reason is mechanical rather than
// stylistic. Every other probe in this series bounds a git CHILD PROCESS, which
// a context can kill. These are blocking syscalls: a context cannot cancel
// os.ReadDir, and threading one in would be a fiction that reads like a fix. So
// the inspection moves off the poll goroutine and the caller stops waiting.
//
// WHAT A TIMEOUT MEANS is the other half. The inspection's finding — "the
// recorded conversation's transcript is gone" — REPLACES the root's durable
// conversation id. A wait that ran out establishes nothing about the store, so
// it must be "not inspected this tick", never "no transcript": the id stands,
// and the throttle brings the check back.

// adoptLiveClaudeRoot puts a live, adopted claude root in the manager carrying
// the recorded conversation a real one has — which is the only state in which
// refreshRootClaudeConversation reaches the transcript store at all.
func adoptLiveClaudeRoot(t *testing.T, manager *Manager, rid, repoPath string) *session.Instance {
	t.Helper()
	inst := registerStarted(t, manager, rid, repoPath, session.RootSessionTitle,
		session.NewFakeBackend(), true, session.Running)
	seedRootConversation(t, inst)
	return inst
}

// stallTranscriptInspection makes one root's transcript inspection hang until
// the test releases it, exactly as a stalled mount does — and, like a stalled
// mount, it does NOT end on its own timer, because the property under test is
// that the caller stops waiting rather than that the read gets faster.
func stallTranscriptInspection(t *testing.T) (release func(), started <-chan struct{}) {
	t.Helper()
	prev := inspectClaudeProjectConversations
	released := make(chan struct{})
	begun := make(chan struct{})
	var once, beganOnce sync.Once
	inspectClaudeProjectConversations = func(program, workingDir string, recorded session.AgentConversationData) (session.ClaudeProjectConversationState, error) {
		beganOnce.Do(func() { close(begun) })
		<-released
		return prev(program, workingDir, recorded)
	}
	t.Cleanup(func() { inspectClaudeProjectConversations = prev })
	return func() { once.Do(func() { close(released) }) }, begun
}

// TestStalledTranscriptStoreDoesNotWedgeTheInstancePoll drives the REAL poll
// loop: a root whose transcript store never answers must not stop the passes
// that come after the ensure sweep, for a session in an unrelated repository.
func TestStalledTranscriptStoreDoesNotWedgeTheInstancePoll(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)

	rootRepo := setupControlRepo(t)
	strandedRepo := setupControlRepo(t)
	strandedID := repoID(t, strandedRepo)
	rid := repoID(t, rootRepo)

	manager, err := NewManager(rootTestConfig(rootRepo, config.RootAgentConfig{Program: tmux.ProgramClaude}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A live, adopted root carrying a recorded claude conversation is what
	// makes the adopt branch reach the inspection at all.
	adoptLiveClaudeRoot(t, manager, rid, rootRepo)
	release, _ := stallTranscriptInspection(t)

	backend := &deadButRecoverableBackend{FakeBackend: session.NewFakeBackend()}
	registerStarted(t, manager, strandedID, strandedRepo, "stranded", backend, true, session.Running)

	stopCh := make(chan struct{})
	wg := &sync.WaitGroup{}
	startInstancePollLoop(manager, 50*time.Millisecond, stopCh, wg)
	// Release BEFORE stopping: on an unbounded inspection the poll goroutine is
	// inside the read and can only observe stopCh once it returns. It runs
	// after the verdict either way.
	t.Cleanup(func() {
		release()
		close(stopCh)
		wg.Wait()
		manager.waitRootAgentCreates()
	})

	// TWO liveness probes: RefreshStatuses runs BEFORE the ensure sweep, so
	// even a wedged goroutine has already probed this session once.
	deadline := time.Now().Add(60 * time.Second)
	for {
		aliveProbes, recovers := backend.counts()
		if aliveProbes >= 2 && recovers >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the instance poll goroutine is wedged reading a root's claude transcript store: the passes after "+
				"EnsureRootAgents never ran for a session in an unrelated repository (liveness probes=%d want >=2, "+
				"recoveries=%d want >=1) (#3782 item 3)", aliveProbes, recovers)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTimedOutTranscriptInspectionIsNotAMissingTranscript pins what the timeout
// MEANS, which is the half a bound alone would get wrong.
//
// The finding this inspection can produce replaces the root's durable
// conversation id. A wait that ran out establishes nothing about the store, so
// the recorded id must be untouched and the log must not say the transcript is
// missing.
func TestTimedOutTranscriptInspectionIsNotAMissingTranscript(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	rid := repoID(t, repoPath)

	// Scoped to THIS Manager (#3787): the process-global sink is written by
	// every Manager alive in the binary, so an assertion on it can be satisfied
	// by a warning this test never caused.
	manager, warnings := newManagerCapturingWarnings(t, rootTestConfig(repoPath, config.RootAgentConfig{Program: tmux.ProgramClaude}))
	inst := adoptLiveClaudeRoot(t, manager, rid, repoPath)
	recorded := inst.AgentConversation()
	prev := rootClaudeTranscriptInspectBudget
	rootClaudeTranscriptInspectBudget = 50 * time.Millisecond
	t.Cleanup(func() { rootClaudeTranscriptInspectBudget = prev })
	release, started := stallTranscriptInspection(t)

	manager.mu.Lock()
	st := manager.rootEnsureStateForLocked(repoPath)
	manager.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.refreshRootClaudeConversation(rid, daemonInstanceKey(rid, session.RootSessionTitle), repoPath, inst, st)
	}()
	// Registered AFTER the stall's own cleanup, so LIFO runs this first: the
	// seam is a package var, and restoring it while a wedged caller is still
	// reading it is a data race — measured on the fail-first run, where the
	// verdict below hit t.Fatal with the inspection still inside the stall.
	t.Cleanup(func() {
		release()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the inspecting goroutine never returned after the stall was released")
		}
	})
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("refreshRootClaudeConversation did not give up on a transcript store that never answered (#3782 item 3)")
	}
	<-started // the inspection really was attempted, not skipped by the throttle

	if got := inst.AgentConversation(); got != recorded {
		t.Fatalf("a wait that ran out must leave the recorded conversation alone, got %+v want %+v", got, recorded)
	}
	log := warnings.String()
	if !strings.Contains(log, "not inspected this tick") {
		t.Fatalf("the warning must say the inspection did not finish; got %q", log)
	}
	if strings.Contains(log, "has no transcript") {
		t.Fatalf("a wait that ran out must not be reported as a missing transcript; got %q", log)
	}
}

// TestTranscriptInspectionIsSingleFlightedPerRoot pins the reason the bound
// needs company. A deadline releases the caller; it cannot release the read. So
// on a store that never answers, one goroutine per 30s throttle interval would
// accumulate for the life of the daemon — a slow leak introduced by the fix.
func TestTranscriptInspectionIsSingleFlightedPerRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	rid := repoID(t, repoPath)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{Program: tmux.ProgramClaude}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	inst := adoptLiveClaudeRoot(t, manager, rid, repoPath)

	var mu sync.Mutex
	inFlight, peak := 0, 0
	prevInspect := inspectClaudeProjectConversations
	released := make(chan struct{})
	inspectClaudeProjectConversations = func(program, workingDir string, recorded session.AgentConversationData) (session.ClaudeProjectConversationState, error) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		<-released
		mu.Lock()
		inFlight--
		mu.Unlock()
		return prevInspect(program, workingDir, recorded)
	}
	t.Cleanup(func() { inspectClaudeProjectConversations = prevInspect })

	prev := rootClaudeTranscriptInspectBudget
	rootClaudeTranscriptInspectBudget = 20 * time.Millisecond
	t.Cleanup(func() { rootClaudeTranscriptInspectBudget = prev })

	manager.mu.Lock()
	st := manager.rootEnsureStateForLocked(repoPath)
	manager.mu.Unlock()
	key := daemonInstanceKey(rid, session.RootSessionTitle)

	var wg sync.WaitGroup
	for range 5 {
		// Each pass is due: the throttle is what paces production, and this
		// test is about what happens when it lets several through over time.
		manager.mu.Lock()
		st.nextClaudeTranscriptInspection = time.Time{}
		manager.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.refreshRootClaudeConversation(rid, key, repoPath, inst, st)
		}()
		time.Sleep(40 * time.Millisecond)
	}
	close(released)
	wg.Wait()
	t.Cleanup(func() {})

	mu.Lock()
	got := peak
	mu.Unlock()
	if got > 1 {
		t.Fatalf("a stalled transcript store must never hold more than one inspection per root, got %d concurrent — "+
			"one per throttle interval accumulates for the life of the daemon (#3782 item 3)", got)
	}
}
