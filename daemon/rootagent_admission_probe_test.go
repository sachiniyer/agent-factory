package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The #3782 item 4 regression suite: the biggest of the four, and the only one
// that is not on a polling goroutine.
//
// config.LegacyRootAgentForRepo resolves every root_agents key through an
// unbounded config.RepoFromPath, and rootAgentMaterializeVerdictFor — the
// single authority for "will the ensure loop (re-)create this repo's root" —
// calls it on the RPC goroutine. Its three consumers are prompt delivery
// (deliverToReemergingRoot), DeleteProject, and the task target fence.
//
// TWO THINGS ARE WRONG, and the tests split along them.
//
// 1. IT HANGS, and two of the three callers hold taskTargetMu while it does.
// #3361 already states that rule for this very lock — "a stalled mount must
// not be walked while taskTargetMu is held" — so an unbounded git probe under
// it stops every task write on the box, not just the caller.
//
// 2. A TIMEOUT MUST NOT BECOME A VERDICT. The lookup's nil means "no entry
// names this repo", which the verdict renders as "no root agent is configured
// for this repo — add a root_agents entry". Said because a probe did not
// answer, that is advice the user cannot act on: the entry is already there.
// #3264 added the recorded-root fallback to stop exactly that misreport; a
// deadline must not re-enter it. So an unanswered probe is UNKNOWN, the
// admission paths FAIL CLOSED with a reason, and DeleteProject — where unknown
// already behaves like yes — keeps treating the root as present so a delete
// cannot silently drop an opt-in it could not read.

// stallLegacyVerdictLookup makes the verdict's legacy lookup hang until the
// caller's own context ends it. The property under test is that the CALLER's
// deadline is what ends it, so the stall must never end on its own timer.
//
// Seam at the daemon boundary rather than inside config/, because the bound
// belongs to the daemon: config keeps LegacyRootAgentForRepo's unbounded
// contract for `af config get root_agent --explain`, where a human typed the
// command and can interrupt it.
func stallLegacyVerdictLookup(t *testing.T, repoID string) (release func()) {
	t.Helper()
	prev := legacyRootAgentForRepoContext
	released := make(chan struct{})
	var once sync.Once
	legacyRootAgentForRepoContext = func(ctx context.Context, cfg *config.Config, id string) (*config.RootAgentConfig, string, error) {
		if id != repoID {
			return prev(ctx, cfg, id)
		}
		select {
		case <-ctx.Done():
			return nil, "", fmt.Errorf("could not establish whether a legacy root agent is configured for repository %s: %w", id, config.ErrRepoProbeUnanswered)
		case <-released:
			return prev(ctx, cfg, id)
		}
	}
	t.Cleanup(func() { legacyRootAgentForRepoContext = prev })
	return func() { once.Do(func() { close(released) }) }
}

// unansweredVerdictBudget makes the verdict's real legacy lookup time out, with
// NO SEAM: a 1ns budget, a real config, and the real
// config.LegacyRootAgentForRepoContext, so the classification under test is
// production's rather than a double's.
//
// It must be installed AFTER NewManager, because the start-of-day snapshot
// resolves legacy paths on the same budget.
func unansweredVerdictBudget(t *testing.T) {
	t.Helper()
	prev := rootRepoProbeBudget
	rootRepoProbeBudget = time.Nanosecond
	t.Cleanup(func() { rootRepoProbeBudget = prev })
}

// unknowableLegacyConfig is the shape that makes the answer genuinely unknown,
// and the shape matters. LegacyRootAgentForRepoContext falls back to the
// path-derived identity for a key it cannot resolve (#3264), so a key that IS
// the queried repo's main root answers the question even when its probe dies —
// the fallback matches. Only a key whose path hashes to something else leaves
// the question open.
//
// So: the registered project under test, and a root_agents key naming a
// DIFFERENT repository. That is also the realistic shape — one entry on a sick
// mount, every other repo's verdict resolved per call against it.
func unknowableLegacyConfig(t *testing.T) (projectPath, legacyPath string) {
	t.Helper()
	projectPath = setupControlRepo(t)
	legacyPath = setupControlRepo(t)
	project := registerTestProject(t, projectPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")
	return projectPath, legacyPath
}

// TestStalledLegacyLookupDoesNotWedgeTheTaskFence is the hang, at the caller
// that holds the lock. The fence runs under taskTargetMu, so an unbounded probe
// there is not one stuck request — it is every task write on the box.
func TestStalledLegacyLookupDoesNotWedgeTheTaskFence(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	rid := repoID(t, repoPath)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	release := stallLegacyVerdictLookup(t, rid)

	done := make(chan taskTargetValidationContext, 1)
	var preparing sync.WaitGroup
	preparing.Add(1)
	go func() {
		defer preparing.Done()
		done <- manager.prepareTaskTargetValidation(rid, session.RootSessionTitle, true)
	}()
	// Release and JOIN before the seam is restored: the seam is a package var,
	// and putting it back while a wedged goroutine reads it is a data race.
	t.Cleanup(func() {
		release()
		joined := make(chan struct{})
		go func() { preparing.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(30 * time.Second):
			t.Error("the preparing goroutine never returned after the stall was released")
		}
	})

	var ctx taskTargetValidationContext
	select {
	case ctx = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the task target fence is wedged in the legacy root_agents lookup: with one configured path's repository " +
			"probe unanswered, prepareTaskTargetValidation never returned — and it runs under taskTargetMu, so every task " +
			"write on the box waits behind it (#3782 item 4)")
	}

	// And it answered UNKNOWN rather than "unconfigured".
	if got := ctx.rootVerdict.reason; got != rootAgentLegacyProbeUnanswered {
		t.Fatalf("a timed-out legacy lookup must be UNKNOWN, got reason %d", got)
	}
}

// TestUnansweredLegacyLookupRefusesRatherThanReportUnconfigured is the
// classification, across the two admission paths that cannot wait.
//
// Both must refuse, both must say why, and neither may say the repo is
// unconfigured — the entry is in root_agents, and a user told to add one has
// nothing to do. The wording is #3500's: a subprocess that did not answer
// establishes nothing about the user's configuration.
func TestUnansweredLegacyLookupRefusesRatherThanReportUnconfigured(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	projectPath, legacyPath := unknowableLegacyConfig(t)
	rid := repoID(t, projectPath)

	manager, err := NewManager(rootTestConfig(legacyPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The project resolves and is enabled, so the ONLY thing standing between
	// it and will-materialize is whether a root_agents key names it.
	if got := manager.rootAgentMaterializeVerdictFor(rid).reason; got != rootAgentWillMaterialize {
		t.Fatalf("fixture: the project must otherwise materialize, got reason %d", got)
	}
	unansweredVerdictBudget(t)

	verdict := manager.rootAgentMaterializeVerdictFor(rid)
	if verdict.reason != rootAgentLegacyProbeUnanswered {
		t.Fatalf("an unanswered legacy probe must produce the UNKNOWN verdict, got reason %d", verdict.reason)
	}
	detail := rootAgentUnavailableDetail(verdict)
	if !strings.Contains(detail, "could not establish whether a legacy root agent is configured") {
		t.Fatalf("the refusal must say what could not be established (#3500); got %q", detail)
	}
	if strings.Contains(detail, "add a root_agents entry") {
		t.Fatalf("a probe that never answered must not advise adding config the user already has; got %q", detail)
	}

	// The task fence refuses, and says nothing was changed.
	fenceCtx := manager.prepareTaskTargetValidation(rid, session.RootSessionTitle, true)
	fenceErr := manager.validateEnabledTaskTarget(task.Task{
		ID: "t1", Enabled: true, TargetSession: session.RootSessionTitle, RepoID: rid,
	}, fenceCtx)
	if fenceErr == nil {
		t.Fatal("the task fence must FAIL CLOSED on an unknown root-agent verdict: a task it accepts here targets a root " +
			"the daemon may never create")
	}
	if !strings.Contains(fenceErr.Error(), "could not establish whether a legacy root agent is configured") {
		t.Fatalf("the fence refusal must carry the cause, got %q", fenceErr)
	}
	if !strings.Contains(fenceErr.Error(), "nothing was changed") {
		t.Fatalf("the fence refusal must say nothing was changed, got %q", fenceErr)
	}

	// Prompt delivery refuses too, rather than falling through to the
	// reserved-name guard's "add it to root_agents" advice.
	repo, err := config.RepoFromPath(projectPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	_, _, handled, deliverErr := manager.deliverToReemergingRoot(repo, DeliverPromptRequest{
		Title: session.RootSessionTitle, Prompt: "hello",
	})
	if !handled || deliverErr == nil {
		t.Fatalf("prompt delivery must refuse with its cause; handled=%v err=%v", handled, deliverErr)
	}
	if !strings.Contains(deliverErr.Error(), "could not establish whether a legacy root agent is configured") {
		t.Fatalf("the delivery refusal must carry the cause, got %q", deliverErr)
	}

	// #1122's shape, and the boundary of the claim: with the budget restored the
	// very next call answers, so the refusal is transient and needs no config
	// change to clear.
	rootRepoProbeBudget = 2 * time.Second
	if got := manager.rootAgentMaterializeVerdictFor(rid).reason; got != rootAgentWillMaterialize {
		t.Fatalf("the verdict must heal once the probe answers again, got reason %d", got)
	}
}

// writeEnabledRootTargetedTask seeds one enabled task pointed at the reserved
// root session, which is what makes the delete preflight have something to
// refuse over.
func writeEnabledRootTargetedTask(t *testing.T, repoPath string) {
	t.Helper()
	if err := task.AddTask(task.Task{
		ID: "roottask", Name: "nightly", Prompt: "go",
		CronExpr: "0 3 * * *", Program: "claude",
		ProjectPath: repoPath, Enabled: true,
		TargetSession: session.RootSessionTitle,
	}); err != nil {
		t.Fatalf("seed root-targeted task: %v", err)
	}
}

// TestUnansweredLegacyLookupKeepsDeleteProjectFailClosed pins the consumer that
// goes the OTHER way, and why that is the same rule.
//
// preflightDeleteProjectTaskTargets already treats every unknown as "yes, there
// is a root" (#3264): deleting through an unknown would drop the root_agents
// opt-in and strand an enabled task the moment the config became readable
// again. An unanswered probe is one more unknown, so the delete must still see
// the reserved target and refuse while a task points at it.
func TestUnansweredLegacyLookupKeepsDeleteProjectFailClosed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	projectPath, legacyPath := unknowableLegacyConfig(t)
	rid := repoID(t, projectPath)
	writeEnabledRootTargetedTask(t, projectPath)

	manager, err := NewManager(rootTestConfig(legacyPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	unansweredVerdictBudget(t)

	_, err = manager.preflightDeleteProjectTaskTargets(rid)
	if err == nil {
		t.Fatal("DeleteProject must keep failing closed on an unknown root-agent verdict: deleting through it drops the " +
			"root_agents opt-in and strands the enabled task that targets the root (#3264)")
	}
	if !strings.Contains(err.Error(), session.RootSessionTitle) {
		t.Fatalf("the delete refusal must name the reserved target it is blocking on, got %q", err)
	}
}
