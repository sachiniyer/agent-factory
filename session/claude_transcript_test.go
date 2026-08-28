package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestClaudeProjectNameMatchesClaudeSanitizer(t *testing.T) {
	require.Equal(t, "-home-siyer--agent-factory", claudeProjectName("/home/siyer/.agent-factory"))

	longPath := "/" + strings.Repeat("a", 205)
	require.Equal(t,
		"-"+strings.Repeat("a", 199)+"-bn8w8e",
		claudeProjectName(longPath),
		"Claude truncates project slugs at 200 UTF-16 code units and appends its base-36 path hash")
}

func TestInspectClaudeProjectConversationsKeepsExistingRecordedTranscript(t *testing.T) {
	configDir := t.TempDir()
	workDir := filepath.Join(t.TempDir(), ".project")
	projectDir := filepath.Join(configDir, "projects", claudeProjectName(workDir))
	require.NoError(t, os.MkdirAll(projectDir, 0o700))

	const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
	const latestID = "5299e00d-1111-4222-8333-f7045e07a242"
	recordedPath := filepath.Join(projectDir, recordedID+".jsonl")
	latestPath := filepath.Join(projectDir, latestID+".jsonl")
	require.NoError(t, os.WriteFile(recordedPath, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(latestPath, []byte("{}\n"), 0o600))
	oldTime := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(recordedPath, oldTime, oldTime))
	require.NoError(t, os.Chtimes(latestPath, oldTime.Add(time.Second), oldTime.Add(time.Second)))

	state, err := InspectClaudeProjectConversations(
		"CLAUDE_CONFIG_DIR="+configDir+" claude", workDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID},
	)
	require.NoError(t, err)
	require.True(t, state.RecordedExists)
	require.Equal(t, recordedID, state.Resume.ID,
		"a newer same-project transcript is not evidence that it belongs to the recorded root process")
	require.Equal(t, ConversationCaptureClaudeTranscript, state.Resume.CaptureKind)

	require.NoError(t, os.Remove(recordedPath))
	state, err = InspectClaudeProjectConversations(
		"CLAUDE_CONFIG_DIR="+configDir+" claude", workDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID},
	)
	require.NoError(t, err)
	require.False(t, state.RecordedExists)
	require.Equal(t, latestID, state.Resume.ID)
}

func TestInspectClaudeProjectConversationsUsesEffectiveLaunchDirectory(t *testing.T) {
	configDir := t.TempDir()
	repoDir := t.TempDir()
	launchDir := filepath.Join(t.TempDir(), "actual")
	require.NoError(t, os.MkdirAll(launchDir, 0o700))

	const id = "5299e00d-1111-4222-8333-f7045e07a242"
	projectDir := filepath.Join(configDir, "projects", claudeProjectName(launchDir))
	require.NoError(t, os.MkdirAll(projectDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, id+".jsonl"), []byte("{}\n"), 0o600))

	state, err := InspectClaudeProjectConversations(
		"env -C "+launchDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: id},
	)
	require.NoError(t, err)
	require.True(t, state.RecordedExists)
	require.Equal(t, id, state.Resume.ID)
}
