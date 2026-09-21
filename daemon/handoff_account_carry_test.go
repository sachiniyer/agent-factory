package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

const carryHandoffConversationID = "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d"

// carryHandoffFixture is a healthy ambient claude session with a real worktree
// and a recorded conversation, handing off to the registered "personal"
// account. It returns the transcript path relative to a provider home.
func carryHandoffFixture(t *testing.T) (*Manager, string, *session.Instance, *limitResumeBackend, string, string) {
	t.Helper()
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", time.Now().Add(time.Hour))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	require.NoError(t, os.Unsetenv("CLAUDE_CONFIG_DIR"))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.SetAgentConversation(session.AgentConversationData{
		Agent: tmux.ProgramClaude, ID: carryHandoffConversationID,
		CapturedAt: time.Now(), CaptureKind: session.ConversationCaptureInjected,
	})
	// Claude names a project after its launch directory with every
	// non-alphanumeric byte replaced; temp paths stay under the 200-byte cap.
	project := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(filepath.Clean(inst.Path), "-")
	rel := filepath.Join("projects", project, carryHandoffConversationID+".jsonl")
	return m, repo, inst, backend, filepath.Join(home, ".claude"), rel
}

func TestHandoffAccountCarriesTheSameAgentConversation(t *testing.T) {
	m, repo, inst, backend, source, rel := carryHandoffFixture(t)
	const transcript = "{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(source, rel)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, rel), []byte(transcript), 0o600))

	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.NoError(t, err)
	require.True(t, resp.OK)

	afHome, err := config.GetConfigDir()
	require.NoError(t, err)
	carried, err := os.ReadFile(filepath.Join(afHome, "accounts", tmux.ProgramClaude, "personal", rel))
	require.NoError(t, err, "the conversation must be copied into the new account before its replacement starts")
	require.Equal(t, transcript, string(carried))
	original, err := os.ReadFile(filepath.Join(source, rel))
	require.NoError(t, err)
	require.Equal(t, transcript, string(original), "the previous account's store is read, never changed")

	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "This conversation continues after an account handoff")
	require.Contains(t, prompts[0], "finish the migration")
	require.NotContains(t, prompts[0], "not available to you",
		"a carried conversation must not be described as lost")

	conv := inst.AgentConversation()
	require.Equal(t, carryHandoffConversationID, conv.ID, "the replacement resumes the same conversation id")
	require.Equal(t, session.ConversationCaptureCarried, conv.CaptureKind)
	handoffs := inst.Handoffs()
	require.Len(t, handoffs, 1)
	require.Equal(t, carryHandoffConversationID, handoffs[0].From.ID,
		"the ledger keeps the outgoing conversation for provenance and return trips")
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending)
}

func TestHandoffAccountStatesWhyItCouldNotCarry(t *testing.T) {
	m, repo, inst, backend, _, _ := carryHandoffFixture(t)

	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.NoError(t, err)

	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "af tried to carry the previous conversation over to the new account, "+
		"but its transcript is missing from the previous account's home",
		"a fresh start after a failed carry must say so rather than silently truncate the history")
	require.Contains(t, prompts[0], "finish the migration")
	conv := inst.AgentConversation()
	require.NotEqual(t, carryHandoffConversationID, conv.ID)
	require.Equal(t, session.ConversationCaptureInjected, conv.CaptureKind)
}

func TestAccountSwapPromptStatesTheConversationOutcome(t *testing.T) {
	swap := &autoAccountSwap{agent: tmux.ProgramCodex, from: "work", to: "personal"}
	plain := accountSwapPrompt(swap, "finish it", session.HandoffConversation{})
	require.NotContains(t, plain, "carry", "an agent no carry applies to keeps today's notice")
	require.Contains(t, plain, "finish it")

	carried := accountSwapPrompt(swap, "finish it", session.HandoffConversation{Carried: true})
	require.Contains(t, carried, "Your conversation was carried over to the new identity")
	require.Contains(t, carried, "finish it")

	failed := accountSwapPrompt(swap, "", session.HandoffConversation{CarryFailure: "its rollout is missing from the previous account's home"})
	require.Contains(t, failed, "af tried to carry the previous conversation over, but its rollout is missing "+
		"from the previous account's home, so this is a fresh conversation")
	require.Contains(t, failed, "\n\ncontinue")

	manual := &autoAccountSwap{manual: true, agent: tmux.ProgramClaude, to: "personal", mission: "the mission"}
	require.Contains(t, accountSwapPrompt(manual, "", session.HandoffConversation{Carried: true}), "the mission",
		"a manual swap's mission already states its conversation outcome")
}

// TestResumeFromLimitAbandonsACarryWhoseLaunchDidNotSurvive is the daemon half
// of the failed-carry fallback: a committed carry whose resume was already
// launched, and whose replacement is gone, is relaunched fresh with a notice
// that says so, rather than re-planning the same resume on every retry.
func TestResumeFromLimitAbandonsACarryWhoseLaunchDidNotSurvive(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", false, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "work")
	inst.ReconcileAccountHandoffSnapshot("work", "claude", true, &session.AccountSwapData{
		To:                    "work",
		CarriedConversationID: carryHandoffConversationID,
		CarriedLaunchStarted:  true,
	})

	_, err := m.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
	require.NoError(t, err)

	_, respawns, prompts := backend.snapshot()
	require.Equal(t, 1, respawns)
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "af tried to carry the previous conversation over, but the replacement stopped "+
		"before it was confirmed working on the carried conversation, so this is a fresh conversation")
	conv := inst.AgentConversation()
	require.NotEqual(t, carryHandoffConversationID, conv.ID, "the abandoned carry must not be resumed again")
	require.Equal(t, session.ConversationCaptureInjected, conv.CaptureKind)
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending)
}
