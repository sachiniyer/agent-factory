package session

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// staleResumeInstance is a started-looking codex session bound to workDir —
// the shape a restore sees when it asks prepareExactResumeConversation whether
// the recorded id still names the pane's conversation.
func staleResumeInstance(t *testing.T, workDir string, conv AgentConversationData) *Instance {
	t.Helper()
	// A worktree af created for the session, apart from the repository's own
	// checkout: only there is a newer thread evidence (#7210).
	repo := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(repo, workDir, "t", "main", "", false, true)
	require.NoError(t, err)
	return &Instance{
		Title:       "t",
		Program:     tmux.ProgramCodex,
		Path:        repo,
		gitWorktree: gw,
		Tabs:        []*Tab{{Conversation: conv}},
	}
}

func markRolloutOlder(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
}

// #6769: a codex session created under an account writes its rollouts to the
// ACCOUNT's store — the pane's CODEX_HOME is delivered through the session
// environment, not the command text — so capture bound from the command model
// alone watches ~/.codex and can never record.
func TestPrepareCreateLaunchBindsAccountCodexHome(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	accountDir := registerAccount(t, tmux.ProgramCodex, "work")
	repoRoot := initInPlaceRepo(t, "account-codex-capture")

	inst, err := NewInstance(InstanceOptions{
		Title:   "account-codex-capture",
		Path:    repoRoot,
		Program: tmux.ProgramCodex,
		Account: "work",
		InPlace: true,
	})
	require.NoError(t, err)

	plan, err := inst.PrepareCreateLaunch()
	require.NoError(t, err)
	require.Equal(t, accountDir, plan.conversationCapture.codexHome,
		"an account-scoped session's capture must watch the account's store, not the ambient one")
	require.Equal(t, pathutil.ResolveForCompare(repoRoot), plan.conversationCapture.workingDir)
}

// The ambient half of the same contract: no account means the command's own
// environment model resolves the store, exactly as before.
func TestPrepareCreateLaunchBindsAmbientCodexHome(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	repoRoot := initInPlaceRepo(t, "ambient-codex-capture")

	inst, err := NewInstance(InstanceOptions{
		Title:   "ambient-codex-capture",
		Path:    repoRoot,
		Program: tmux.ProgramCodex,
		InPlace: true,
	})
	require.NoError(t, err)

	plan, err := inst.PrepareCreateLaunch()
	require.NoError(t, err)
	require.Equal(t, codexHome, plan.conversationCapture.codexHome)
}

// #6769 property 2: a recorded id that no longer names the conversation the
// pane is in must not be resumed by a restore. The recorded rollout exists but
// a NEWER top-level rollout for the same working directory appeared — the pane
// /new'd — so the id is stale and the restore starts fresh instead.
func TestExactResumeRefusesAStaleRecordedConversation(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	recordedID := "019f386f-7206-7fc2-803b-f7045e07a242"
	recorded := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+recordedID+".jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-39-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", workDir)
	markRolloutOlder(t, recorded)

	inst := staleResumeInstance(t, workDir, AgentConversationData{Agent: tmux.ProgramCodex, ID: recordedID})

	launched := prepareResumeConversation(inst, inst.Program)
	assert.Equal(t, inst.Program, launched,
		"a stale recorded id must not be spliced into the restore command")

	ok, report := inst.ExactResume()
	assert.False(t, ok)
	assert.Contains(t, report, "no longer the newest",
		"ExactResume must name the staleness, not a generic reason")
}

// The sole rollout being the recorded one is the ordinary restore: resume applies.
func TestExactResumeWithOnlyTheRecordedRolloutResumes(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	recordedID := "019f386f-7206-7fc2-803b-f7045e07a242"
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+recordedID+".jsonl", workDir)

	inst := staleResumeInstance(t, workDir, AgentConversationData{Agent: tmux.ProgramCodex, ID: recordedID})
	launched := prepareResumeConversation(inst, inst.Program)
	assert.Contains(t, launched, "resume "+recordedID)

	ok, report := inst.ExactResume()
	assert.True(t, ok)
	assert.Empty(t, report)
}

// #6766: a newer rollout that is provably a sub-agent thread can never be the
// session's conversation — it must not stale the recorded id either.
func TestExactResumeIgnoresANewerSubagentRollout(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	recordedID := "019f386f-7206-7fc2-803b-f7045e07a242"
	recorded := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+recordedID+".jsonl", workDir)
	writeCodexRolloutFileWithSource(t, codexHome, "rollout-2026-07-06T10-17-39-039f386f-7206-7fc2-803b-f7045e07a244.jsonl",
		workDir, map[string]any{"subagent": "review"})
	markRolloutOlder(t, recorded)

	inst := staleResumeInstance(t, workDir, AgentConversationData{Agent: tmux.ProgramCodex, ID: recordedID})
	launched := prepareResumeConversation(inst, inst.Program)
	assert.Contains(t, launched, "resume "+recordedID,
		"a newer sub-agent rollout is not a newer conversation")
}

// A newer top-level rollout for a DIFFERENT working directory is not evidence
// about this session's conversation.
func TestExactResumeIgnoresANewerForeignWorktreeRollout(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	otherDir := t.TempDir()
	recordedID := "019f386f-7206-7fc2-803b-f7045e07a242"
	recorded := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+recordedID+".jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-39-049f386f-7206-7fc2-803b-f7045e07a245.jsonl", otherDir)
	markRolloutOlder(t, recorded)

	inst := staleResumeInstance(t, workDir, AgentConversationData{Agent: tmux.ProgramCodex, ID: recordedID})
	launched := prepareResumeConversation(inst, inst.Program)
	assert.Contains(t, launched, "resume "+recordedID)
}

// A recorded id with no rollout on disk is not resumed — the check sees the
// absence and refuses rather than handing codex a dangling id.
func TestExactResumeRefusesWhenRecordedRolloutIsMissing(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	recordedID := "019f386f-7206-7fc2-803b-f7045e07a242"

	inst := staleResumeInstance(t, workDir, AgentConversationData{Agent: tmux.ProgramCodex, ID: recordedID})
	launched := prepareResumeConversation(inst, inst.Program)
	assert.Equal(t, inst.Program, launched)
}

// A newer rollout whose session_meta cannot be classified cannot be excused —
// fail closed rather than resume a recorded id that might be stale.
func TestExactResumeRefusesWhenANewerRolloutCannotBeCorrelated(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	recordedID := "019f386f-7206-7fc2-803b-f7045e07a242"
	recorded := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+recordedID+".jsonl", workDir)
	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-39-059f386f-7206-7fc2-803b-f7045e07a246.jsonl")
	markRolloutOlder(t, recorded)

	inst := staleResumeInstance(t, workDir, AgentConversationData{Agent: tmux.ProgramCodex, ID: recordedID})
	launched := prepareResumeConversation(inst, inst.Program)
	assert.Equal(t, inst.Program, launched,
		"an unattributable newer rollout is refuse, not resume")
}

// The stale check resolves the store the way the session writes it: an
// account-scoped session's recorded rollout lives in the account's dir, and a
// newer thread there is what stales it.
func TestExactResumeChecksTheAccountStore(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	accountDir := registerAccount(t, tmux.ProgramCodex, "work")
	ambient := t.TempDir()
	t.Setenv("CODEX_HOME", ambient)
	workDir := t.TempDir()
	recordedID := "019f386f-7206-7fc2-803b-f7045e07a242"
	recorded := writeCodexRolloutFileWithCwd(t, accountDir, "rollout-2026-07-06T10-17-35-"+recordedID+".jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, accountDir, "rollout-2026-07-06T10-17-39-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", workDir)
	markRolloutOlder(t, recorded)

	inst := staleResumeInstance(t, workDir, AgentConversationData{Agent: tmux.ProgramCodex, ID: recordedID})
	inst.Account, inst.accountAgent = "work", tmux.ProgramCodex

	launched := prepareResumeConversation(inst, inst.Program)
	assert.Equal(t, inst.Program, launched,
		"a stale id in the account store is refused, not resumed")
}

// A carried conversation — the launch pinned before the tab exists — is not the
// tab's recorded id and is not the restore guard's business: the adopt/swap
// paths that set it make their own calls about what it names.
func TestExactResumeDoesNotStaleCheckACarriedConversation(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	carriedID := "019f386f-7206-7fc2-803b-f7045e07a242"
	carried := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+carriedID+".jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-39-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", workDir)
	markRolloutOlder(t, carried)

	inst := staleResumeInstance(t, workDir, AgentConversationData{})
	inst.Tabs = nil
	inst.carriedConversation = AgentConversationData{Agent: tmux.ProgramCodex, ID: carriedID}

	launched := prepareResumeConversation(inst, inst.Program)
	assert.Contains(t, launched, "resume "+carriedID)
}

// The adopted-launch existence check must look in the same store the pane
// writes to: an account-scoped session's recorded rollout lives in the
// account's dir, invisible to the ambient command model.
func TestAdoptedLaunchFindsTheRecordedConversationInTheAccountStore(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	accountDir := registerAccount(t, tmux.ProgramCodex, "work")
	t.Setenv("CODEX_HOME", t.TempDir())
	workDir := t.TempDir()
	recordedID := "019f386f-7206-7fc2-803b-f7045e07a242"
	writeCodexRolloutFileWithCwd(t, accountDir, "rollout-2026-07-06T10-17-35-"+recordedID+".jsonl", workDir)

	inst := staleResumeInstance(t, workDir, AgentConversationData{})
	inst.Tabs = nil
	inst.Account, inst.accountAgent = "work", tmux.ProgramCodex

	conv := AgentConversationData{Agent: tmux.ProgramCodex, ID: recordedID}
	var launched string
	require.NoError(t, inst.launchAdoptedWith(&conv, func() error {
		launched = prepareResumeConversation(inst, inst.Program)
		return nil
	}))
	assert.Contains(t, launched, "resume "+recordedID,
		"the adopt check must see the rollout in the account store and pin the resume")
}
