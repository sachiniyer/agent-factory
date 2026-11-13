package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// A newer conversation in the same project says nothing about whether the
// recorded one is filed: only the recorded id's own transcript does.
func TestClaudeFiledHereIsTheRecordedTranscriptsOwnExistence(t *testing.T) {
	configDir := t.TempDir()
	workDir := filepath.Join(t.TempDir(), ".project")
	projectDir := filepath.Join(configDir, "projects", claudeProjectName(workDir))
	require.NoError(t, os.MkdirAll(projectDir, 0o700))

	const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
	const latestID = "5299e00d-1111-4222-8333-f7045e07a242"
	program := "CLAUDE_CONFIG_DIR=" + configDir + " claude"
	recorded := AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID}
	recordedPath := filepath.Join(projectDir, recordedID+".jsonl")
	require.NoError(t, os.WriteFile(recordedPath, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, latestID+".jsonl"), []byte(claudeRecordLine(t, workDir)), 0o600))

	require.True(t, claudeConversationFiledHere(program, workDir, recorded))
	require.NoError(t, os.Remove(recordedPath))
	require.False(t, claudeConversationFiledHere(program, workDir, recorded),
		"a newer conversation in the project is not the recorded one")
	require.False(t, claudeConversationFiledHere(program, workDir, AgentConversationData{Agent: tmux.ProgramCodex, ID: latestID}),
		"only a Claude conversation is filed in a Claude store")
}

func TestClaudeFiledHereUsesEffectiveLaunchDirectory(t *testing.T) {
	configDir := t.TempDir()
	repoDir := t.TempDir()
	launchDir := filepath.Join(t.TempDir(), "actual")
	require.NoError(t, os.MkdirAll(launchDir, 0o700))

	const id = "5299e00d-1111-4222-8333-f7045e07a242"
	projectDir := filepath.Join(configDir, "projects", claudeProjectName(launchDir))
	require.NoError(t, os.MkdirAll(projectDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, id+".jsonl"), []byte("{}\n"), 0o600))

	require.True(t, claudeConversationFiledHere(
		"env -C "+launchDir+" CLAUDE_CONFIG_DIR="+configDir+" claude", repoDir,
		AgentConversationData{Agent: tmux.ProgramClaude, ID: id},
	))
}
