package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func exactResumeSession(t *testing.T, program string) *Instance {
	t.Helper()
	workDir := t.TempDir()
	inst, err := NewInstance(InstanceOptions{Title: "B-7 item", Path: workDir, Program: program, BacklogItem: 7})
	require.NoError(t, err)
	// The restore guard resolves the session's launch environment against its
	// worktree path (#6769); a bare Path without a worktree resolves nothing.
	gw, err := git.NewGitWorktreeFromStorage(workDir, workDir, "B-7 item", "main", "", false, true)
	require.NoError(t, err)
	inst.gitWorktree = gw
	return inst
}

// fileRecordedClaudeTranscript files conversation id in inst's Claude store, the
// way Claude does on the conversation's first entry: a recorded id with no
// transcript is started on rather than resumed (recordedClaudeDecline, #7215).
func fileRecordedClaudeTranscript(t *testing.T, inst *Instance, id string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	writeCarryFile(t, configDir, claudeTranscriptRel(inst.GetWorktreePath(), id), "{}\n")
}

// ExactResume, which gates the archive after an agent asks, answers exactly as
// ResumeProgramWithConversationID does for each supported program: af archives
// only a session it can resume by its own conversation id. Today that is
// claude, codex and amp; every other agent would come back as a fresh agent.
func TestExactResumeFollowsResumeProgramWithConversationID(t *testing.T) {
	resumable := map[string]bool{tmux.ProgramClaude: true, tmux.ProgramCodex: true, tmux.ProgramAmp: true}
	const convID = "11111111-1111-4111-8111-111111111111"
	for _, program := range tmux.SupportedPrograms {
		t.Run(program, func(t *testing.T) {
			inst := exactResumeSession(t, program)
			inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: program, ID: convID, CapturedAt: time.Now()}}}
			if program == tmux.ProgramCodex {
				// The restore guard also proves the recorded rollout exists in
				// the session's own store (#6769): give the fixture one.
				codexHome := t.TempDir()
				t.Setenv("CODEX_HOME", codexHome)
				writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-"+convID+".jsonl")
			}
			if program == tmux.ProgramClaude {
				fileRecordedClaudeTranscript(t, inst, convID)
			}
			_, want := tmux.ResumeProgramWithConversationID(program, program, convID)
			got, why := inst.ExactResume()
			require.Equal(t, want, got, "ExactResume must follow ResumeProgramWithConversationID for %s", program)
			require.Equal(t, resumable[program], got, "which agents af resumes by id changed; check the archive after asking and docs/cli.md")
			if got {
				require.Empty(t, why)
			} else {
				require.Equal(t, "af cannot resume a "+program+" conversation by its id", why)
			}
		})
	}
}

// A session of a resumable agent still needs its own recorded id: without one
// a restore starts a fresh conversation, so it is not archived either.
func TestExactResumeNeedsARecordedConversation(t *testing.T) {
	inst := exactResumeSession(t, tmux.ProgramClaude)
	ok, why := inst.ExactResume()
	require.False(t, ok)
	require.Equal(t, "af recorded no conversation id for this claude session", why)

	// A recorded conversation of another agent is not this session's.
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramCodex, ID: "conv-1", CapturedAt: time.Now()}}}
	ok, why = inst.ExactResume()
	require.False(t, ok)
	require.True(t, strings.HasPrefix(why, "af cannot resume this claude session's recorded conversation"), why)
}

// remoteWorkspaceBackend is a sandbox backend (docker, ssh, hook) for the gate:
// only its remote workspace matters.
type remoteWorkspaceBackend struct{ *FakeBackend }

func (b remoteWorkspaceBackend) Capabilities() Capabilities {
	c := b.FakeBackend.Capabilities()
	c.Workspace = WorkspaceRemote
	return c
}

// An off-box session is never archived on ask, even with a conversation id its
// command could resume: archiving reaps the sandbox, and the fresh one a restore
// provisions starts a new conversation (Codex P1 on #6475).
func TestExactResumeRefusesASandboxSession(t *testing.T) {
	inst := exactResumeSession(t, tmux.ProgramClaude)
	const convID = "11111111-1111-4111-8111-111111111111"
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: convID, CapturedAt: time.Now()}}}
	fileRecordedClaudeTranscript(t, inst, convID)
	ok, _ := inst.ExactResume()
	require.True(t, ok, "the control: a local claude session with a recorded id resumes exactly")
	inst.SetBackend(remoteWorkspaceBackend{NewFakeBackend()})
	ok, why := inst.ExactResume()
	require.False(t, ok, "a sandbox session must stay live on ask")
	require.Equal(t, "this session runs in a sandbox, and the sandbox af restores it into does not keep its agent's conversation", why)
}
