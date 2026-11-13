package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// adoptInstanceAt is a fresh in-sandbox instance whose worktree is workDir: a
// linked worktree beside the sandbox's repository, as AdoptExistingWorktree
// binds it.
func adoptInstanceAt(t *testing.T, workDir string) *Instance {
	t.Helper()
	repo := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(repo, workDir, "t", "main", "", false, true)
	require.NoError(t, err)
	return &Instance{Title: "t", Program: "codex", Path: repo, gitWorktree: gw}
}

const (
	adoptedOlderID  = "019f386f-7206-7fc2-803b-f7045e07a242"
	adoptedLatestID = "039f386f-7206-7fc2-803b-f7045e07a244"
)

// #5949: a home holding two conversations, the archived session's older than
// the latest, which is another worktree's. The adopted launch's command names
// the archived one, so the provider's own "latest" is never consulted. (A
// newer thread in this worktree is a /new, judged by
// TestAdoptedLaunchJudgesTheTabsCodexRecord.)
func TestAdoptedLaunchResumesTheRecordedConversationNotTheLatest(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+adoptedOlderID+".jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-37-"+adoptedLatestID+".jsonl", t.TempDir())

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

// #7217 review R3: the Claude twin. A recorded Claude id this host's store does
// not hold (a fresh workspace's store holds none of the old history, and Claude
// files lazily) is not dropped before the launch: it is judged by the
// recorded-Claude rule, which starts Claude on it, keeps it recorded, and never
// leaves the launch to a latest-conversation selector.
func TestAdoptedLaunchStartsClaudeOnARecordedIDItsStoreLacks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	inst := adoptInstanceAt(t, t.TempDir())
	inst.Program = "claude"
	conv := AgentConversationData{Agent: "claude", ID: adoptedOlderID}

	var launched string
	require.NoError(t, inst.launchAdoptedWith(&conv, func() error {
		launched = prepareResumeConversation(inst, inst.Program)
		return nil
	}))
	assert.Equal(t, "claude --session-id "+adoptedOlderID, launched,
		"the adopted launch starts Claude on the id the host recorded")
	assert.Equal(t, adoptedOlderID, inst.resumableConversation().ID, "the id stays the record")
}

// #4797: a recovery names the session's own recorded conversation, and with no id
// the program is untouched here — the tmux layer then declines the account-wide
// "latest" rewrite for a session marked exact-only.
func TestRecoveryResumeNamesTheSessionsOwnConversation(t *testing.T) {
	workDir := t.TempDir()
	inst := adoptInstanceAt(t, workDir)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: "codex", ID: adoptedOlderID}}}
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	// The restore guard proves the recorded rollout exists in the session's
	// own store (#6769) — a named id that names nothing on disk is refused.
	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-"+adoptedOlderID+".jsonl")
	got := prepareResumeConversation(inst, "codex")
	assert.Contains(t, got, "resume "+adoptedOlderID)

	none := &Instance{Title: "t"}
	assert.Equal(t, "codex", prepareResumeConversation(none, "codex"))
}

func TestAdoptedLaunchFailureStillPropagates(t *testing.T) {
	inst := &Instance{Title: "t"}
	require.Error(t, inst.launchAdoptedWith(&AgentConversationData{Agent: "codex", ID: adoptedOlderID}, func() error { return errors.New("boom") }))
}

// #7219: a sandbox restore hands the agent-server the tab's record as a pin,
// and the Codex guard judges it like the tab's own record. After a /new in
// the sandbox (a newer top-level thread in this worktree), the relaunch does
// not resume the thread the user left, and the refused pin is spent. With no
// newer thread it resumes exactly.
func TestAdoptedLaunchJudgesTheTabsCodexRecord(t *testing.T) {
	for _, tc := range []struct {
		name  string
		newer bool
	}{{"after a /new", true}, {"with no newer thread", false}} {
		t.Run(tc.name, func(t *testing.T) {
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			workDir := t.TempDir()
			recorded := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-35-"+adoptedOlderID+".jsonl", workDir)
			markRolloutOlder(t, recorded)
			if tc.newer {
				writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-07-06T10-17-37-"+adoptedLatestID+".jsonl", workDir)
			}
			inst := adoptInstanceAt(t, workDir)
			inst.Tabs = []*Tab{{}}
			conv := AgentConversationData{Agent: "codex", ID: adoptedOlderID}

			var launched string
			var decline resumeDecline
			require.NoError(t, inst.launchAdoptedWith(&conv, func() error {
				var ok bool
				launched, ok, decline = prepareExactResumeConversation(inst, inst.Program)
				if !ok {
					inst.RetireConversationForFreshRuntime(inst.exactResumeRefusal(decline).ID, ConversationCaptureSnapshot{})
				}
				return nil
			}))
			if !tc.newer {
				assert.Contains(t, launched, "resume "+adoptedOlderID)
				return
			}
			assert.NotContains(t, launched, adoptedOlderID, "the thread the user left is not resumed")
			assert.NotContains(t, launched, "--last")
			assert.Equal(t, staleCarryReason, decline.reason)
			assert.False(t, inst.resumableConversation().HasID(), "the refused pin is spent")
		})
	}
}
