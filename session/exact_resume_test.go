package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// adoptInstanceAt is a fresh in-sandbox instance whose worktree is workDir.
func adoptInstanceAt(t *testing.T, workDir string) *Instance {
	t.Helper()
	gw, err := git.NewGitWorktreeFromStorage(workDir, workDir, "t", "main", "", false, true)
	require.NoError(t, err)
	return &Instance{Title: "t", Program: "codex", gitWorktree: gw}
}

const (
	adoptedOlderID  = "019f386f-7206-7fc2-803b-f7045e07a242"
	adoptedLatestID = "039f386f-7206-7fc2-803b-f7045e07a244"
)

// #5949: a workspace holding two conversations, the archived session's older than
// the latest. The adopted launch's command names the archived one, so the
// provider's own "latest" is never consulted.
func TestAdoptedLaunchResumesTheRecordedConversationNotTheLatest(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+adoptedOlderID+".jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-37-"+adoptedLatestID+".jsonl", workDir)

	t.Setenv("CODEX_HOME", codexHome)
	inst := adoptInstanceAt(t, workDir)
	conv := AgentConversationData{Agent: "codex", ID: adoptedOlderID}

	var launched string
	require.NoError(t, inst.launchAdoptedWith(&conv, func() error {
		launched = prepareResumeConversation(inst, inst.Program)
		return nil
	}))
	assert.Contains(t, launched, "resume "+adoptedOlderID)
	assert.NotContains(t, launched, adoptedLatestID)
	assert.NotContains(t, launched, "--last")
}

// A recorded id the host's store does not hold is not resumed, and nothing
// substitutes the latest: the launch command carries no resume at all.
func TestAdoptedLaunchWithAnAbsentConversationResumesNothing(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	inst := adoptInstanceAt(t, t.TempDir())
	conv := AgentConversationData{Agent: "codex", ID: adoptedOlderID}

	var launched string
	require.NoError(t, inst.launchAdoptedWith(&conv, func() error {
		launched = prepareResumeConversation(inst, inst.Program)
		return nil
	}))
	assert.Equal(t, inst.Program, launched)
}

// #4797: a recovery names the session's own recorded conversation, and with no id
// the program is untouched here — the tmux layer then declines the account-wide
// "latest" rewrite for a session marked exact-only.
func TestRecoveryResumeNamesTheSessionsOwnConversation(t *testing.T) {
	inst := &Instance{Title: "t", Tabs: []*Tab{{Conversation: AgentConversationData{Agent: "codex", ID: adoptedOlderID}}}}
	got := prepareResumeConversation(inst, "codex")
	assert.Contains(t, got, "resume "+adoptedOlderID)

	none := &Instance{Title: "t"}
	assert.Equal(t, "codex", prepareResumeConversation(none, "codex"))
}

func TestAdoptedLaunchFailureStillPropagates(t *testing.T) {
	inst := &Instance{Title: "t"}
	require.Error(t, inst.launchAdoptedWith(&AgentConversationData{Agent: "codex", ID: adoptedOlderID}, func() error { return errors.New("boom") }))
}
