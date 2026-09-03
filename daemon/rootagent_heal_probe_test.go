package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The #3782 item 1 regression suite: the SECOND unbounded probe on the instance
// poll goroutine, and the one #3760 named and deliberately left.
//
// #3760 bounded the legacy sweep's per-tick resolution. This is the other
// caller of config.RepoFromPath that the poll goroutine reaches —
// legacyRepoIDSet, recomputing the singleton sweep's dedup set on every
// published heal (rootagent_heal.go) — and it is a narrower exposure with the
// SAME blast radius: RefreshStatuses, RestoreLostSessions and the settlement
// retries stop for every session on the box because one configured path went
// quiet.
//
// Narrower because healRootAgentLayers early-returns unless the snapshot
// already carries unknowns and the recompute runs only when a heal actually
// republished, which is why #3760's red reproduces without it. It therefore
// needs a box that is ALREADY degraded — which is also the state in which a
// stalled mount is most likely.
//
// The bound alone would be a REGRESSION, and that is the other half of this
// suite. The recompute exists because of #3315: a legacy path missing from the
// dedup set lets the singleton sweep double-visit its repo behind a failing
// legacy attempt and start the root without the legacy layer. A timeout that
// dropped the entry would re-enter that defect through the deadline, so an
// unanswered probe is UNKNOWN and the repo ID the path last resolved to stands.

// unansweredLegacyRootResolution makes one configured path's resolution fail
// the way a BOUND makes it fail — an error carrying config.ErrRepoProbeUnanswered
// — without a stall, so the carry-forward contract can be asserted rather than
// waited on. config.RepoFromPathContext wraps exactly this sentinel when its
// deadline expires (config/repo.go), so the stand-in's error shape is
// production's, not an invention.
func unansweredLegacyRootResolution(t *testing.T, repoPath string) (restore func()) {
	t.Helper()
	prev := legacyRootRepoFromPath
	legacyRootRepoFromPath = func(ctx context.Context, path string) (*config.RepoContext, error) {
		if filepath.Clean(path) != filepath.Clean(repoPath) {
			return prev(ctx, path)
		}
		return nil, fmt.Errorf("failed to get git repo root for %s: %w", path, config.ErrRepoProbeUnanswered)
	}
	t.Cleanup(func() { legacyRootRepoFromPath = prev })
	return func() { legacyRootRepoFromPath = prev }
}

// degradedRootAgentManager returns a manager whose snapshot carries an unknown
// — a personal config that does not parse — and heals that unknown the moment
// the next pass reads it. That is the precondition the heal recompute needs:
// healRootAgentLayers early-returns while the snapshot is clean, and the
// recompute runs only on a heal that actually republished.
//
// The legacy root_agents entry and the registered project name the SAME repo,
// which is the whole point of the dedup set: whichever sweep is allowed to
// reach it decides which layer stack the root runs under.
func degradedRootAgentManager(t *testing.T, repoPath string, rc config.RootAgentConfig) *Manager {
	t.Helper()
	project := registerTestProject(t, repoPath)
	breakPersonalRootAgentToml(t, project.ID)
	manager, err := NewManager(rootTestConfig(repoPath, rc))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if len(manager.rootAgentLayers.Load().personalUnreadable) == 0 {
		t.Fatal("fixture: the boot snapshot must carry an unreadable personal config, or the heal pass never runs")
	}
	// Content-bearing, so the retry heals it on its first attempt.
	writePersonalRootAgent(t, project.ID, "enabled = true")
	return manager
}

// TestStalledLegacyPathDoesNotWedgeTheHealRecompute drives the REAL poll loop,
// because the claim is about that goroutine and nothing else: with the snapshot
// degraded and a heal due, a root_agents path whose repository resolution never
// answers must not stop the passes that come after the ensure sweep, for a
// session in an unrelated repository.
//
// The unrelated session's tmux has vanished, so what the loop owes it is
// RefreshStatuses marking it Lost and RestoreLostSessions recovering it. Both
// run strictly after EnsureRootAgents in poll_loop.go, and healRootAgentLayers
// is the FIRST thing EnsureRootAgents does, so on master neither happens.
//
// The stall is installed AFTER NewManager, exactly as #3760's was: the
// start-of-day snapshot resolves every legacy path too, and stalling that
// wedges construction instead of the poll — a real but separate exposure
// (#3782 item 2), not conflated with this one.
func TestStalledLegacyPathDoesNotWedgeTheHealRecompute(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)

	rootRepo := setupControlRepo(t)
	strandedRepo := setupControlRepo(t)
	strandedID := repoID(t, strandedRepo)

	manager := degradedRootAgentManager(t, rootRepo, config.RootAgentConfig{})
	release := stallLegacyRootResolution(t, rootRepo)

	backend := &deadButRecoverableBackend{FakeBackend: session.NewFakeBackend()}
	// Registered Running so the pass has to TRANSITION it to Lost rather than
	// merely leaving a pre-set status alone.
	registerStarted(t, manager, strandedID, strandedRepo, "stranded", backend, true, session.Running)

	stopCh := make(chan struct{})
	wg := &sync.WaitGroup{}
	startInstancePollLoop(manager, 50*time.Millisecond, stopCh, wg)
	// Release BEFORE stopping. On an unbounded recompute the poll goroutine is
	// inside the stall and can only observe stopCh once the resolution returns,
	// so without this wg.Wait() blocks until the package timeout — the failure
	// mode #3760 measured, where every daemon test after it never ran. It runs
	// after the verdict either way, so it cannot help the assertion pass.
	t.Cleanup(func() {
		release()
		close(stopCh)
		wg.Wait()
		manager.waitRootAgentCreates()
	})

	// TWO liveness probes, not one, is what discriminates. RefreshStatuses runs
	// BEFORE the ensure sweep, so even a poll goroutine that never returns from
	// the heal has already probed this session once. A second probe can only
	// come from a tick that got all the way round.
	deadline := time.Now().Add(60 * time.Second)
	for {
		aliveProbes, recovers := backend.counts()
		if aliveProbes >= 2 && recovers >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the instance poll goroutine is wedged in the heal pass's legacy dedup recompute: with one "+
				"configured path's repository resolution unanswered, the passes after EnsureRootAgents never ran for a "+
				"session in an unrelated repository (liveness probes=%d want >=2, recoveries=%d want >=1) (#3782 item 1)",
				aliveProbes, recovers)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUnansweredLegacyDedupProbeKeepsThePreviousRepoID pins the dedup decision
// the bound forces, at the function that makes it.
//
// "git answered, and the answer is no" and "we could not ask git" are different
// states (#3500), and here the difference decides a SET MEMBERSHIP rather than
// a log line: a verdict may drop the entry, an unanswered probe may not,
// because dropping it is precisely #3315's double-visit — the singleton sweep
// visiting a repo whose root_agents opt-in has been sitting there all along and
// starting its root without the legacy layer.
func TestUnansweredLegacyDedupProbeKeepsThePreviousRepoID(t *testing.T) {
	const path = "/repos/alpha"
	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{path: {}}

	unanswered := func(string) (*config.RepoContext, error) {
		return nil, fmt.Errorf("failed to get git repo root for %s: %w", path, config.ErrRepoProbeUnanswered)
	}
	// A VERDICT: git ran, git answered, and the answer is that the path is not
	// a repository. Deliberately carries no ErrRepoProbeUnanswered.
	verdict := func(string) (*config.RepoContext, error) {
		return nil, errors.New("failed to get git repo root for " + path + ": not a git repository")
	}
	resolves := func(id string) legacyRepoResolver {
		return func(string) (*config.RepoContext, error) { return &config.RepoContext{ID: id}, nil }
	}

	cases := []struct {
		name       string
		resolve    legacyRepoResolver
		previous   map[string]string
		wantIDs    map[string]bool
		wantByPath map[string]string
	}{{
		name:       "an unanswered probe leaves the previous repo ID standing",
		resolve:    unanswered,
		previous:   map[string]string{path: "aaaaaaaaaaaa"},
		wantIDs:    map[string]bool{"aaaaaaaaaaaa": true},
		wantByPath: map[string]string{path: "aaaaaaaaaaaa"},
	}, {
		// A path that never resolved was never in the set, so there is
		// nothing to keep. This is #1122's not-yet-cloned entry, unchanged.
		name:       "an unanswered probe with nothing to carry forward adds nothing",
		resolve:    unanswered,
		previous:   nil,
		wantIDs:    map[string]bool{},
		wantByPath: map[string]string{},
	}, {
		name:       "a verdict drops the entry even when one was carried",
		resolve:    verdict,
		previous:   map[string]string{path: "aaaaaaaaaaaa"},
		wantIDs:    map[string]bool{},
		wantByPath: map[string]string{},
	}, {
		name:       "a fresh resolution replaces what was carried",
		resolve:    resolves("bbbbbbbbbbbb"),
		previous:   map[string]string{path: "aaaaaaaaaaaa"},
		wantIDs:    map[string]bool{"bbbbbbbbbbbb": true},
		wantByPath: map[string]string{path: "bbbbbbbbbbbb"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids, byPath := legacyRepoIDSet(cfg, tc.resolve, tc.previous)
			if !maps.Equal(ids, tc.wantIDs) {
				t.Fatalf("dedup set = %v, want %v", ids, tc.wantIDs)
			}
			if !maps.Equal(byPath, tc.wantByPath) {
				t.Fatalf("per-path resolutions = %v, want %v", byPath, tc.wantByPath)
			}
		})
	}
}

// TestUnansweredLegacyDedupProbeDoesNotDoubleVisitTheRepo is the same claim end
// to end, through the real heal pass and both real sweeps: the blast radius the
// carry-forward closes.
//
// With the recompute's probe unanswered and the entry dropped, the repo falls
// out of the dedup set, the singleton sweep stops skipping it, and it starts
// the root from the global/personal layers alone — a root the user configured a
// program for, running something else, because a probe on an unrelated mount
// was slow. Nothing may be created behind an unknown; and once the probe
// answers, the LEGACY layer is the one that materializes the root.
func TestUnansweredLegacyDedupProbeDoesNotDoubleVisitTheRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	rid := repoID(t, repoPath)

	const legacyProgram = "/opt/legacy-root --model opus"
	manager := degradedRootAgentManager(t, repoPath, config.RootAgentConfig{Program: legacyProgram})
	if !manager.rootAgentLayers.Load().legacyRepoIDs[rid] {
		t.Fatal("fixture: the boot snapshot must already dedup the legacy path, or there is nothing to carry forward")
	}
	restore := unansweredLegacyRootResolution(t, repoPath)

	manager.ensureRootAgentsAndWait()

	layers := manager.rootAgentLayers.Load()
	if len(layers.personalUnreadable) != 0 {
		t.Fatalf("fixture: the personal config must have healed so the recompute actually ran, got %d still unreadable", len(layers.personalUnreadable))
	}
	if !layers.legacyRepoIDs[rid] {
		t.Fatal("an unanswered dedup probe dropped the repo from the healed snapshot's dedup set: an unknown was read as absent, " +
			"which is #3315's double-visit re-entered through a timeout (#3782 item 1)")
	}
	if len(*seen) != 0 {
		t.Fatalf("nothing may be created behind an unanswered probe — the singleton sweep started the root without the legacy layer, got %d creates", len(*seen))
	}

	// And once the probe answers, it is the LEGACY layer that materializes it.
	restore()
	manager.mu.Lock()
	if st := manager.rootEnsureStates[repoPath]; st != nil {
		st.nextAttempt = time.Time{}
	}
	manager.mu.Unlock()
	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("the answered pass must create exactly one root, got %d creates", len(*seen))
	}
	if got := (*seen)[0].Program; got != legacyProgram {
		t.Fatalf("the root must run the legacy entry's program, got %q — the singleton sweep reached the repo behind the legacy attempt", got)
	}
	if findRootInstance(t, manager, repoPath) == nil {
		t.Fatal("no root instance registered once the dedup probe answered")
	}
}

// TestHealthyLegacyDedupRecomputeIsUnchanged is the non-regression half, and it
// is a guard rather than a red: it holds before and after the bound, which is
// the whole claim. Bounding a probe must change nothing whatsoever for a
// checkout that answers.
//
// A differential oracle against the unbounded entry point's own result rather
// than a restatement of the expected set, so any divergence the bounded path
// introduces fails here rather than being described into agreement.
func TestHealthyLegacyDedupRecomputeIsUnchanged(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	cfg := rootTestConfig(repoPath, config.RootAgentConfig{})

	wantIDs, wantByPath := legacyRepoIDSet(cfg, unboundedLegacyRootRepo, nil)
	if len(wantIDs) != 1 || len(wantByPath) != 1 {
		t.Fatalf("fixture: the unbounded resolution must produce one entry, got ids=%v byPath=%v", wantIDs, wantByPath)
	}
	gotIDs, gotByPath := legacyRepoIDSet(cfg, resolveLegacyRootRepo, nil)
	if !maps.Equal(gotIDs, wantIDs) {
		t.Fatalf("the bounded recompute's dedup set differs on a healthy checkout:\n got %v\nwant %v", gotIDs, wantIDs)
	}
	if !maps.Equal(gotByPath, wantByPath) {
		t.Fatalf("the bounded recompute's per-path resolutions differ on a healthy checkout:\n got %v\nwant %v", gotByPath, wantByPath)
	}

	// And a healthy carry-forward is a no-op: the previous map must not be able
	// to resurrect an ID the current pass did not resolve.
	stale := map[string]string{repoPath: "ffffffffffff", "/repos/gone": "eeeeeeeeeeee"}
	staleIDs, staleByPath := legacyRepoIDSet(cfg, resolveLegacyRootRepo, stale)
	if !maps.Equal(staleIDs, wantIDs) || !maps.Equal(staleByPath, wantByPath) {
		t.Fatalf("a previous resolution must not survive a probe that answered:\n got ids=%v byPath=%v\nwant ids=%v byPath=%v",
			staleIDs, staleByPath, wantIDs, wantByPath)
	}
}
