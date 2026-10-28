package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The #3793 regression suite: the probe #3782 item 2 did NOT bound.
//
// Item 2 bounded the legacy root_agents keys at daemon start. projectRootAgentLayers
// resolves every REGISTERED PROJECT's recorded root in the same construction,
// through config.RepoFromPath with no context and no deadline, so a registered
// project on a stalled mount still wedged NewManager — the same blast radius as
// item 2's, reached by the other half of the same function.
//
// The correction that came out of reading the code, and why this suite is
// shaped the way it is: ResolveRegisteredProjectRepoID is NOT unbounded. It
// opens its own registeredProjectProbeTimeout (250ms) around both the
// resolution and the marker read, so it could never hang. What it lacked was
// the CALLER's context — its budget added to the caller's instead of composing
// with it, and the step could not be cancelled halfway. That is a composition
// fix, not a hang fix, and the tests say so rather than claiming a second hang.
//
// THE CLAIM THAT NEEDS THE MOST CARE is not the bound. It is that a timed-out
// resolution is a DIFFERENT STATE from "the recorded root did not resolve".
// Both make the path unusable for the pass, and both are re-checked on the
// ensure cadence — the handling really is the same. What is not the same is
// what a consumer may SAY: "bring that path back" is a claim about the user's
// checkout, and a probe nobody answered establishes no such thing (#3500).

// stallProjectRootResolution stalls one registered project's recorded-root
// resolution until the caller's own context ends it, classified through
// production's own classifier so the stand-in cannot disagree with
// config.RepoFromPathContext about what a dead deadline means.
func stallProjectRootResolution(t *testing.T, root string) (release func()) {
	t.Helper()
	prev := projectRootRepoFromPath
	released := make(chan struct{})
	var once sync.Once
	projectRootRepoFromPath = func(ctx context.Context, path string) (*config.RepoContext, error) {
		if filepath.Clean(path) != filepath.Clean(root) {
			return prev(ctx, path)
		}
		select {
		case <-ctx.Done():
			return nil, config.ClassifyGitProbeError(ctx, ctx.Err())
		case <-released:
			return prev(ctx, path)
		}
	}
	t.Cleanup(func() { projectRootRepoFromPath = prev })
	return func() { once.Do(func() { close(released) }) }
}

// TestStalledProjectRootDoesNotWedgeDaemonStart is the hang, at the half of
// buildRootAgentSnapshot item 2 left alone. NewManager runs on its own
// goroutine so the test can outlive a construction that never returns.
func TestStalledProjectRootDoesNotWedgeDaemonStart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	release := stallProjectRootResolution(t, repoPath)

	type built struct {
		manager *Manager
		err     error
	}
	done := make(chan built, 1)
	var constructing sync.WaitGroup
	constructing.Add(1)
	go func() {
		defer constructing.Done()
		manager, err := NewManager(config.DefaultConfig())
		done <- built{manager, err}
	}()
	// Release and JOIN before the seam is restored: it is a package var, and
	// putting it back under a wedged reader is a data race.
	t.Cleanup(func() {
		release()
		joined := make(chan struct{})
		go func() { constructing.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(30 * time.Second):
			t.Error("the constructing goroutine never returned after the stall was released")
		}
	})

	var result built
	select {
	case result = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("daemon start is wedged resolving a registered project's recorded root: with one checkout on a stalled " +
			"mount, NewManager never returned, so the daemon serves no session on the box at all (#3793)")
	}
	if result.err != nil {
		t.Fatalf("NewManager failed rather than starting degraded: %v", result.err)
	}

	// It came up, and it recorded WHICH kind of unknown this is.
	layers := result.manager.rootAgentLayers.Load()
	rid := config.ReconciledRepoIDForProject(project)
	record, ok := layers.unresolvedRoots[rid]
	if !ok {
		t.Fatalf("the project must still be carried as unresolved so the ensure cadence retries it; unresolvedRoots=%v", layers.unresolvedRoots)
	}
	if !record.rootProbeUnanswered {
		t.Fatal("a recorded root whose probe never answered must be marked as such, or the verdict tells the user their " +
			"checkout is gone on the strength of a killed subprocess (#3500)")
	}
}

// TestUnansweredProjectRootDoesNotClaimTheCheckoutIsGone is the claim half, and
// the one that is not a mechanical port.
//
// "Its recorded project root does not currently resolve to a git repository;
// bring the path back" is the #3247/#3264 advice, and it is correct when git
// answered. Said because a subprocess was killed, it sends a user to repair a
// checkout that may be perfectly healthy — and it is the same overclaim #3500
// removed from the log lines, surviving in the verdict.
func TestUnansweredProjectRootDoesNotClaimTheCheckoutIsGone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	// The REAL classifier, no seam: a 1ns budget against a real checkout, so
	// the resolution times out inside config.RepoFromPathContext exactly as it
	// would on a stalled mount.
	prev := rootRepoProbeBudget
	rootRepoProbeBudget = time.Nanosecond
	t.Cleanup(func() { rootRepoProbeBudget = prev })

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rootRepoProbeBudget = prev

	rid := config.ReconciledRepoIDForProject(project)
	verdict := manager.rootAgentMaterializeVerdictFor(rid)
	if verdict.reason == rootAgentProjectUnresolved {
		t.Fatal("an unanswered recorded-root probe must NOT reuse the resolved-and-absent verdict: that reason's remedy " +
			"is 'bring the path back', which nothing established (#3793)")
	}
	if verdict.reason != rootAgentProjectRootProbeUnanswered {
		t.Fatalf("an unanswered recorded-root probe must produce its own verdict, got reason %d", verdict.reason)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if !strings.Contains(detail, "could not establish whether its recorded project root is a git repository") {
		t.Fatalf("the refusal must say what could not be established (#3500); got %q", detail)
	}
	if strings.Contains(detail, "bring the path back") || strings.Contains(detail, "does not currently resolve") {
		t.Fatalf("a probe that never answered must not tell the user their checkout is gone; got %q", detail)
	}
}

// TestAnsweredMissingProjectRootStillSaysBringItBack is the other side of the
// same coin, and the reason this is a distinction rather than a softening: when
// git DOES answer, the old advice is right and must survive.
func TestAnsweredMissingProjectRootStillSaysBringItBack(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	// git ANSWERS about a path that is not there: exit 128, a verdict.
	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide checkout: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rid := config.ReconciledRepoIDForProject(project)
	record, ok := manager.rootAgentLayers.Load().unresolvedRoots[rid]
	if !ok {
		t.Fatalf("fixture: the project must be unresolved; unresolvedRoots=%v", manager.rootAgentLayers.Load().unresolvedRoots)
	}
	if record.rootProbeUnanswered {
		t.Fatal("git exiting 128 about a missing path is an ANSWER; marking it unanswered would make every ordinary " +
			"absent checkout stop saying what the user has to do")
	}
	verdict := manager.rootAgentMaterializeVerdictFor(rid)
	if verdict.reason != rootAgentProjectUnresolved {
		t.Fatalf("an answered absence must keep the resolved-and-absent verdict, got reason %d", verdict.reason)
	}
	if got := rootAgentUnavailableDetail(verdict); !strings.Contains(got, "bring the path back") {
		t.Fatalf("an answered absence must still prescribe bringing the path back, got %q", got)
	}
}

// TestUnansweredIdentityProofBindsNothing is the identity negative (#3530): a
// proof that could not be established is NOT a proof of something different.
//
// The project's identity has never been recorded, so the boot pass tries to
// prove it. With the budget exhausted the proof cannot run, and the only safe
// outcome is that nothing is written and the retry is latched UNPROVEN — the
// proven:false side of reconcileOwed, so the retry re-establishes the proof
// rather than inheriting a claim about a checkout it never verified.
func TestUnansweredIdentityProofBindsNothing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	clearRecordedProjectRepoID(t, project.ID)

	prev := rootRepoProbeBudget
	rootRepoProbeBudget = time.Nanosecond
	t.Cleanup(func() { rootRepoProbeBudget = prev })

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rootRepoProbeBudget = prev

	for _, owed := range manager.rootAgentLayers.Load().reconcileOwed {
		if owed.proven {
			t.Fatal("a proof that never ran must be latched UNPROVEN: recording it as proven lets the retry write an " +
				"identity for a checkout nothing verified (#3530)")
		}
	}
	// And nothing was written: the record's identity is still empty, so a
	// replacement checkout at that path cannot inherit this project's state.
	if got := recordedProjectRepoID(t, project.ID); got != "" {
		t.Fatalf("an unanswered proof must bind nothing, but the record now claims repo %q", got)
	}
}

// clearRecordedProjectRepoID blanks a registry record's repo_id, producing the
// pre-#3530 shape the identity proof exists for: a project whose identity has
// never been written down, so the boot pass has to establish it.
func clearRecordedProjectRepoID(t *testing.T, projectID string) {
	t.Helper()
	rewriteProjectRecord(t, projectID, func(record map[string]any) { delete(record, "repo_id") })
}

// recordedProjectRepoID reads the durable identity straight off the record, so
// the assertion is about what was PERSISTED rather than about an in-memory view
// that a later pass could have refreshed.
func recordedProjectRepoID(t *testing.T, projectID string) string {
	t.Helper()
	var id string
	rewriteProjectRecord(t, projectID, func(record map[string]any) {
		if v, ok := record["repo_id"].(string); ok {
			id = v
		}
	})
	return id
}

func rewriteProjectRecord(t *testing.T, projectID string, edit func(map[string]any)) {
	t.Helper()
	dir, err := config.ProjectRegistryDir()
	if err != nil {
		t.Fatalf("ProjectRegistryDir: %v", err)
	}
	path := filepath.Join(dir, projectID, "project.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read project record: %v", err)
	}
	record := map[string]any{}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("parse project record: %v", err)
	}
	before := len(record)
	edit(record)
	if len(record) == before {
		return
	}
	out, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal project record: %v", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("write project record: %v", err)
	}
}
