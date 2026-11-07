package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
)

// These tests pin #6944's structural reset source: the Claude transcript
// record a refused request leaves behind
// (apiError="usage_limit_reached", apiErrorParams.rate_limit_info.resetsAt).
// ClaudeActiveLimitReset must read it ONLY from the transcripts of the
// session's CURRENT account and launch directory — Captain's scoping
// requirement — and must treat the epoch resetsAt as the wall's instant
// rather than trusting the record's display text, which a narrow pane wraps
// into an unparseable shape and which can omit the day entirely (weekly).

// claudeLimitRecord builds one refusal record line, the real shape Claude
// writes (fabricated content — no real transcript data).
func claudeLimitRecord(t *testing.T, timestamp string, resetsAt any) []byte {
	t.Helper()
	rec := map[string]any{
		"type":      "assistant",
		"timestamp": timestamp,
		"message": map[string]any{
			"model": "<synthetic>",
			"role":  "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "You've hit your 5-hour limit · resets 9:40am (America/Los_Angeles)"},
			},
		},
		"apiError": "usage_limit_reached",
		"apiErrorParams": map[string]any{
			"rate_limit_info": map[string]any{"status": "rejected", "rateLimitType": "five_hour"},
		},
		"isApiErrorMessage": true,
		"session_id":        "28e1b947-ba23-4588-988b-3c688b6f241c",
	}
	if resetsAt != nil {
		rec["apiErrorParams"].(map[string]any)["rate_limit_info"].(map[string]any)["resetsAt"] = resetsAt
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal fixture record: %v", err)
	}
	return line
}

// claudeLimitInstance returns a claude instance rooted at a temp worktree, and
// the worktree path Claude encodes into its project directory name.
func claudeLimitInstance(t *testing.T, account string) (*Instance, string) {
	t.Helper()
	repo := initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repo, repo, "limit", "main", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		ID:      "claude-limit",
		Title:   "claude-limit",
		Path:    repo,
		Program: "claude",
		Account: account,
		backend: &LocalBackend{},
	}
	inst.gitWorktree = gw
	return inst, gw.GetWorktreePath()
}

// writeClaudeTranscript drops a transcript file under configDir for workDir,
// in the same layout Claude uses: projects/<encoded-workdir>/<uuid>.jsonl.
func writeClaudeTranscript(t *testing.T, configDir, workDir, conversationID string, lines ...[]byte) string {
	t.Helper()
	dir := filepath.Join(configDir, "projects", claudeProjectName(workDir))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, conversationID+".jsonl")
	var content []byte
	for _, line := range lines {
		content = append(content, line...)
		content = append(content, '\n')
	}
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

// claudeLimitConfigDir returns the Claude config dir the instance's
// transcripts must come from: $HOME/.claude for the default account, or
// <AFHOME>/accounts/claude/<account> for a named one. It creates the account
// registration directory so agentaccount.Selected accepts it.
func claudeLimitConfigDir(t *testing.T, home, account string) string {
	t.Helper()
	if account == "" {
		return filepath.Join(home, ".claude")
	}
	afHome := testguard.CanonicalTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	dir := filepath.Join(afHome, "accounts", "claude", account)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	return dir
}

func TestClaudeActiveLimitReset(t *testing.T) {
	now := time.Date(2026, 11, 7, 17, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	stale := now.Add(-time.Hour)
	futureUnix := future.Unix()
	staleUnix := stale.Unix()
	recorded := now.Add(-time.Minute).UTC().Format(time.RFC3339)

	t.Run("refusal record supplies the epoch reset, not the display text", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		resetAt, ok := inst.ClaudeActiveLimitReset(now)
		require.True(t, ok, "a current refusal record must supply the reset")
		require.True(t, resetAt.Equal(future), "resetAt = %v, want %v", resetAt, future)
	})

	t.Run("stale refusal record supplies nothing", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, staleUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "an expired wall must not supply a reset")
	})

	t.Run("the LATEST refusal record answers for the wall", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		later := now.Add(4 * time.Hour)
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix),
			claudeLimitRecord(t, now.Add(-30*time.Second).UTC().Format(time.RFC3339), later.Unix()))
		resetAt, ok := inst.ClaudeActiveLimitReset(now)
		require.True(t, ok)
		require.True(t, resetAt.Equal(later), "resetAt = %v, want the newest refusal's %v", resetAt, later)
	})

	t.Run("a newest record with an expired reset supersedes an older future one", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, now.Add(-3*time.Hour).UTC().Format(time.RFC3339), futureUnix),
			claudeLimitRecord(t, recorded, staleUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "the newest refusal says the wall already ended")
	})

	t.Run("a refusal publishing no reset supplies nothing", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, nil))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok)
	})

	t.Run("transcript in a different project directory is out of scope", func(t *testing.T) {
		inst, _ := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		other := filepath.Join(testguard.CanonicalTempDir(t), "elsewhere")
		writeClaudeTranscript(t, configDir, other, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "another worktree's transcript must not supply this session's wall")
	})

	t.Run("an account the session swapped away from cannot supply the reset", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "new")
		afHome := testguard.CanonicalTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", afHome)
		// Register BOTH account dirs, but put the refusal record only in the
		// account the session swapped AWAY from.
		for _, account := range []string{"old", "new"} {
			require.NoError(t, os.MkdirAll(filepath.Join(afHome, "accounts", "claude", account), 0o700))
		}
		oldDir := filepath.Join(afHome, "accounts", "claude", "old")
		writeClaudeTranscript(t, oldDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "the previous account's transcript must not supply the reset")
	})

	t.Run("the current account's transcript supplies the reset after a swap", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "new")
		afHome := testguard.CanonicalTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", afHome)
		for _, account := range []string{"old", "new"} {
			require.NoError(t, os.MkdirAll(filepath.Join(afHome, "accounts", "claude", account), 0o700))
		}
		writeClaudeTranscript(t, filepath.Join(afHome, "accounts", "claude", "old"), workDir,
			"28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, stale.Add(-2*time.Hour).Unix()))
		writeClaudeTranscript(t, filepath.Join(afHome, "accounts", "claude", "new"), workDir,
			"28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		resetAt, ok := inst.ClaudeActiveLimitReset(now)
		require.True(t, ok)
		require.True(t, resetAt.Equal(future), "resetAt = %v, want %v", resetAt, future)
	})

	t.Run("the newest refusal across the project's transcripts answers for the wall", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		// A second conversation in the same worktree on the same account: the
		// account's wall is shared, so the freshest refusal statement wins —
		// here it says the wall already ended, superseding the older future
		// reset in the other transcript.
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		writeClaudeTranscript(t, configDir, workDir, "11111111-2222-3333-4444-555555555555",
			claudeLimitRecord(t, now.Add(-10*time.Second).UTC().Format(time.RFC3339), staleUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "the freshest refusal statement says the wall ended")
	})

	t.Run("no transcript supplies nothing", func(t *testing.T) {
		inst, _ := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok)
	})

	t.Run("non-claude session supplies nothing", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		inst.Program = "codex"
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok)
	})
}
