package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pruneConvID = "aaaaaaaa-1111-2222-3333-444444444444"

// pruneFixture builds a throwaway provider layout:
//
//	<claudeHome>/projects/proj/<id>.jsonl        — the transcript
//	<claudeHome>/projects/proj/<id>/tool.json    — the conversation's aux dir
//	<claudeHome>/projects/proj/other.jsonl       — ANOTHER conversation, must stay
//	<afHome>/accounts/claude/work/projects/proj2/<id>.jsonl — a carried copy
//	<codexHome>/sessions/2026/09/05/rollout-…-<id2>.jsonl     — a codex rollout
func pruneFixture(t *testing.T) (afHome, claudeHome, codexHome string) {
	t.Helper()
	afHome = t.TempDir()
	claudeHome = t.TempDir()
	codexHome = t.TempDir()

	proj := filepath.Join(claudeHome, "projects", "-repo-worktrees-old-session")
	require.NoError(t, os.MkdirAll(proj, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proj, pruneConvID+".jsonl"), make([]byte, 2048), 0o644))
	aux := filepath.Join(proj, pruneConvID)
	require.NoError(t, os.MkdirAll(aux, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(aux, "tool-result.json"), make([]byte, 512), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "other-conversation-0000-0000-0000-000000000099.jsonl"), make([]byte, 100), 0o644))

	accountProj := filepath.Join(afHome, agentaccount.DirName, tmux.ProgramClaude, "work", "projects", "-repo-worktrees-old-session")
	require.NoError(t, os.MkdirAll(accountProj, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(accountProj, pruneConvID+".jsonl"), make([]byte, 4096), 0o644))

	codexDir := filepath.Join(codexHome, "sessions", "2026", "09", "05")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(codexDir, "rollout-2026-09-05T12-00-00-bbbbbbbb-0000-0000-0000-000000000002.jsonl"),
		make([]byte, 1024), 0o644))

	return afHome, claudeHome, codexHome
}

// TestPruneFileIndex_FilesForClaude proves the index resolves a recorded
// conversation id to its transcript across the ambient home AND every
// registered account home — the carried copy — while never claiming the
// unrelated conversation living in the same project directory.
func TestPruneFileIndex_FilesForClaude(t *testing.T) {
	afHome, claudeHome, _ := pruneFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", t.TempDir())

	idx := NewPruneFileIndex(afHome)
	data := InstanceData{AgentConversation: &AgentConversationData{Agent: tmux.ProgramClaude, ID: pruneConvID}}

	files, warns := idx.FilesFor(data)
	require.Empty(t, warns)
	require.Len(t, files.Files, 2, "ambient + carried account copy")
	require.Len(t, files.Dirs, 1, "the aux directory beside the ambient transcript")
	assert.Equal(t, int64(2048+4096), files.Bytes)
	for _, path := range files.Files {
		assert.Contains(t, path, pruneConvID+".jsonl")
	}
	for _, path := range append(files.Files, files.Dirs...) {
		assert.NotContains(t, path, "other-conversation")
	}
}

// TestPruneFileIndex_CodexAndUnsupported proves a codex rollout resolves by id
// and that an agent af does not manage reports a warning instead of deleting
// by guess.
func TestPruneFileIndex_CodexAndUnsupported(t *testing.T) {
	afHome, _, codexHome := pruneFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", codexHome)

	idx := NewPruneFileIndex(afHome)
	codexID := "bbbbbbbb-0000-0000-0000-000000000002"
	data := InstanceData{
		Tabs: []TabData{{
			Conversation: &AgentConversationData{Agent: tmux.ProgramCodex, ID: codexID},
			Handoffs:     []AgentHandoff{{From: AgentConversationData{Agent: "aider", ID: "cccc"}}},
		}},
	}
	files, warns := idx.FilesFor(data)
	require.Len(t, files.Files, 1)
	assert.Contains(t, files.Files[0], codexID)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "aider")
}

// TestDeleteConversationFiles pins the deletion contract: resolved files and
// aux dirs are removed, a foreign conversation in the same project directory
// is untouched, and a second call (retry) is a clean no-op.
func TestDeleteConversationFiles(t *testing.T) {
	afHome, claudeHome, _ := pruneFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", t.TempDir())

	idx := NewPruneFileIndex(afHome)
	data := InstanceData{AgentConversation: &AgentConversationData{Agent: tmux.ProgramClaude, ID: pruneConvID}}
	files, _ := idx.FilesFor(data)

	foreign := filepath.Join(claudeHome, "projects", "-repo-worktrees-old-session",
		"other-conversation-0000-0000-0000-000000000099.jsonl")

	require.Empty(t, DeleteConversationFiles(files))
	for _, path := range files.Files {
		_, err := os.Stat(path)
		assert.True(t, os.IsNotExist(err), "%s must be gone", path)
	}
	_, err := os.Stat(files.Dirs[0])
	assert.True(t, os.IsNotExist(err), "the aux dir must be gone")
	_, err = os.Stat(foreign)
	require.NoError(t, err, "a conversation the record does not own must remain")

	// Retry-safety: deleting the same resolved set again is a no-op, not an error.
	require.Empty(t, DeleteConversationFiles(files))
}
