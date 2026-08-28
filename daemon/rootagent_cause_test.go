package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// These tests are the #3264 regression suite: a consumer that refuses because
// the root-agent decision is fail-closed (#3241/#3247) must NAME the cause —
// a refusal without a reason reads as a bug, and the pre-#3264 messages
// actively misdirected ("unconfigured, unresolved, or its project may be
// deleted" for a repo whose personal config simply cannot be parsed). Hermetic
// on the rootagent_singleton_test.go rules: temp AGENT_FACTORY_HOME, fake
// backend, no real daemon.

// rootCauseFixture builds a manager whose repo fails closed (or resolves
// disabled) per the given corrupt step, with a legacy root_agents entry so the
// repo is a genuine root-agent candidate throughout.
func rootCauseFixture(t *testing.T, personal string, corrupt func(t *testing.T, projectID string)) (*Manager, string, string, config.Project) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	if personal != "" {
		writePersonalRootAgent(t, project.ID, personal)
	}
	if corrupt != nil {
		corrupt(t, project.ID)
	}
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager, repoPath, repoID(t, repoPath), project
}

// rootTargetTask is a valid enabled cron task aimed at the reserved root title.
func rootTargetTask(id, repoPath, repoID string) task.Task {
	return task.Task{
		ID: id, Name: "Root Cause Probe", Prompt: "run it", CronExpr: "*/15 * * * *",
		ProjectPath: repoPath, RepoID: repoID, TargetSession: session.RootSessionTitle,
		Enabled: true, CreatedAt: time.Now(),
	}
}

// TestValidateEnabledTaskTargetNamesFailClosedCause: enabling a root-targeted
// task on a repo whose root agent will not materialize must say WHY, naming
// the thing to fix — the unloadable personal config file, the unlistable
// registry directory, or the disable itself — instead of the pre-#3264
// guess-list that omits every actual cause.
func TestValidateEnabledTaskTargetNamesFailClosedCause(t *testing.T) {
	cases := []struct {
		name     string
		personal string
		corrupt  func(t *testing.T, projectID string)
		// wantIn is asserted present in the refusal; the misdirecting
		// pre-#3264 guess-list must be gone in every case.
		wantIn func(t *testing.T, project config.Project) string
	}{
		{
			name:     "unloadable personal config names the file",
			personal: "enabled = false",
			corrupt:  breakPersonalRootAgentToml,
			wantIn: func(t *testing.T, project config.Project) string {
				path, err := config.ProjectConfigTomlPath(project.ID)
				if err != nil {
					t.Fatalf("ProjectConfigTomlPath: %v", err)
				}
				return path
			},
		},
		{
			name: "unlistable registry names the registry",
			corrupt: func(t *testing.T, _ string) {
				breakProjectRegistryEnumeration(t)
			},
			wantIn: func(t *testing.T, _ config.Project) string {
				return config.ProjectRegistryDirName
			},
		},
		{
			name:     "resolved disable names the deciding layer",
			personal: "enabled = false",
			wantIn: func(t *testing.T, _ config.Project) string {
				return "enabled=false in the personal project layer"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoPath, rid, project := rootCauseFixture(t, tc.personal, tc.corrupt)
			ctx := manager.prepareTaskTargetValidation(rid, session.RootSessionTitle, true)
			err := manager.validateEnabledTaskTarget(rootTargetTask("cause001", repoPath, rid), ctx)
			if err == nil {
				t.Fatalf("enabling a root-targeted task must still be refused while the root cannot materialize")
			}
			if want := tc.wantIn(t, project); !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal must name the cause (%q); got: %v", want, err)
			}
			if strings.Contains(err.Error(), "unconfigured, unresolved") {
				t.Fatalf("the refusal still guesses instead of naming the cause: %v", err)
			}
		})
	}
}

// TestValidateEnabledTaskTargetDistinguishesDefaultOffFromExplicitDisable: a
// registered project with no root-agent config anywhere is a candidate that
// resolves to the built-in default (disabled). The refusal must say no layer
// enables it — inventing an "explicit enabled=false" there names a false
// cause and sends the user hunting for a disable that does not exist
// (#3304 review).
func TestValidateEnabledTaskTargetDistinguishesDefaultOffFromExplicitDisable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rid := repoID(t, repoPath)

	ctx := manager.prepareTaskTargetValidation(rid, session.RootSessionTitle, true)
	verr := manager.validateEnabledTaskTarget(rootTargetTask("cause002", repoPath, rid), ctx)
	if verr == nil {
		t.Fatalf("a default-off root must still refuse the task target")
	}
	if !strings.Contains(verr.Error(), "no root_agent layer enables") {
		t.Fatalf("the refusal must say no layer enables the repo; got: %v", verr)
	}
	if strings.Contains(verr.Error(), "enabled=false") {
		t.Fatalf("the refusal must not invent an explicit disable for the built-in default; got: %v", verr)
	}
}

// TestDeliverToReemergingRootNamesFailClosedCause: a delivery to the absent
// reserved root of a repo that fails closed (or resolves disabled) must be
// handled with an error naming the cause — not fall through to auto-create,
// whose reserved-name guard advises "add it to root_agents" at a repo that
// often already IS in root_agents. The unconfigured repo keeps the historical
// fallthrough: there the reserved-name advice is exactly right.
func TestDeliverToReemergingRootNamesFailClosedCause(t *testing.T) {
	cases := []struct {
		name        string
		personal    string
		corrupt     func(t *testing.T, projectID string)
		wantHandled bool
		wantIn      string
	}{
		{
			name:        "unloadable personal config",
			personal:    "enabled = false",
			corrupt:     breakPersonalRootAgentToml,
			wantHandled: true,
			wantIn:      "cannot be loaded",
		},
		{
			name: "unlistable registry",
			corrupt: func(t *testing.T, _ string) {
				breakProjectRegistryEnumeration(t)
			},
			wantHandled: true,
			wantIn:      config.ProjectRegistryDirName,
		},
		{
			name:        "resolved disable",
			personal:    "enabled = false",
			wantHandled: true,
			wantIn:      "disabled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoPath, _, _ := rootCauseFixture(t, tc.personal, tc.corrupt)
			repo, err := config.RepoFromPath(repoPath)
			if err != nil {
				t.Fatalf("RepoFromPath: %v", err)
			}
			_, _, handled, derr := manager.deliverToReemergingRoot(repo, DeliverPromptRequest{Title: session.RootSessionTitle, Prompt: "ping"})
			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v — a fail-closed root delivery must be answered with its cause, not fall through to the reserved-name guard", handled, tc.wantHandled)
			}
			if derr == nil || !strings.Contains(derr.Error(), tc.wantIn) {
				t.Fatalf("the delivery error must name the cause (%q); got: %v", tc.wantIn, derr)
			}
		})
	}
}

// TestVerdictNamesUnresolvedProjectRoot: a registered project whose recorded
// root does not resolve at daemon start — with a personal enable and no
// legacy entry — is CONFIGURED but uncreatable this run (#3247 arm 2). The
// verdict must say that, not "no root agent is configured", which would send
// the user to add config that already exists (#3264 review).
func TestVerdictNamesUnresolvedProjectRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")
	rid := repoID(t, repoPath)

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Rename(hidden, repoPath) })
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if got := manager.rootAgentMaterializeVerdictFor(rid).reason; got != rootAgentProjectUnresolved {
		t.Fatalf("verdict reason = %d, want rootAgentProjectUnresolved — an enabled project with an unresolvable root is not 'unconfigured'", got)
	}
	ctx := manager.prepareTaskTargetValidation(rid, session.RootSessionTitle, true)
	verr := manager.validateEnabledTaskTarget(rootTargetTask("cause003", repoPath, rid), ctx)
	if verr == nil {
		t.Fatalf("an uncreatable root must still refuse the task target")
	}
	if !strings.Contains(verr.Error(), "does not currently resolve") {
		t.Fatalf("the refusal must name the unresolved root, got: %v", verr)
	}
	if strings.Contains(verr.Error(), "no root agent is configured") {
		t.Fatalf("the refusal must not advise adding config that exists, got: %v", verr)
	}
}

// TestVerdictSeesUnresolvableLegacyEntry: with BOTH the recorded project root
// and its matching root_agents path unavailable at daemon start, the legacy
// layer must still be attributed by recorded-root identity — an empty entry
// means enabled, and the legacy sweep's per-tick retry creates the root the
// moment the path returns, so the verdict is "will materialize", not "no
// layer enables this repo" (#3264 review, round 5).
func TestVerdictSeesUnresolvableLegacyEntry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	rid := repoID(t, repoPath)

	hidden := repoPath + ".hidden"
	if err := os.Rename(repoPath, hidden); err != nil {
		t.Fatalf("hide repo dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Rename(hidden, repoPath) })
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if got := manager.rootAgentMaterializeVerdictFor(rid).reason; got != rootAgentWillMaterialize {
		t.Fatalf("verdict reason = %d, want rootAgentWillMaterialize — the unresolvable legacy entry still opts the repo in, and its per-tick retry creates the root when the path returns", got)
	}
}

// TestDeliverToReemergingRootVariantTitleKeepsFallthrough: a reserved-title
// VARIANT ("Root") can never be delivered to — the ensure loop creates only
// the exact title — so the cause-bearing branch must not intercept it and
// promise that a policy fix will make it deliverable. It falls through to the
// reserved-name guard, whose "pick another name" is the correct advice.
func TestDeliverToReemergingRootVariantTitleKeepsFallthrough(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	_, _, handled, _ := manager.deliverToReemergingRoot(repo, DeliverPromptRequest{Title: "Root", Prompt: "ping"})
	if handled {
		t.Fatalf("a reserved-title variant must keep the reserved-name fallthrough — no policy remedy can make %q deliverable", "Root")
	}
}

// TestDeliverToReemergingRootUnconfiguredKeepsFallthrough pins the carve-out:
// with no legacy entry and no registered project the reserved-name guard's
// "add it to root_agents" advice is correct, so the delivery must keep
// falling through to it.
func TestDeliverToReemergingRootUnconfiguredKeepsFallthrough(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	_, _, handled, _ := manager.deliverToReemergingRoot(repo, DeliverPromptRequest{Title: session.RootSessionTitle, Prompt: "ping"})
	if handled {
		t.Fatalf("an unconfigured repo must keep the auto-create fallthrough — its reserved-name advice is the correct answer there")
	}
}

// TestPreflightDeleteBlocksRootTaskWhenDecisionUnknown is #3264's one
// behavioral consumer change: for the delete preflight, "unknown" must act
// like YES. With the personal config unloadable (fail-closed, no live root),
// an enabled root-targeted task previously stopped blocking `af projects
// delete` and was left stranded behind a title ordinary auto-create refuses.
func TestPreflightDeleteBlocksRootTaskWhenDecisionUnknown(t *testing.T) {
	manager, repoPath, rid, _ := rootCauseFixture(t, "enabled = false", breakPersonalRootAgentToml)
	if err := task.AddTask(rootTargetTask("strand01", repoPath, rid)); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	_, err := manager.preflightDeleteProjectTaskTargets(rid)
	if err == nil {
		t.Fatalf("delete preflight must block on an enabled root-targeted task while the root-agent decision is unknown — deleting would strand the task behind a reserved title nothing will create")
	}
	if !strings.Contains(err.Error(), session.RootSessionTitle) || !strings.Contains(err.Error(), "strand01") {
		t.Fatalf("the preflight refusal must name the reserved title and the blocking task; got: %v", err)
	}
}
