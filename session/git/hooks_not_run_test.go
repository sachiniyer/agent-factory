package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// #7084 review, P3s on the hook not-run note: worded by what happened, never
// claiming "in time" for a read that was cancelled; free of the "; " the row
// joins notes with, so a later run can replace exactly it; and recorded for a
// config the repository answered as invalid, not logged alone.

func TestHookConfigNotRunNoteIsWordedByCause(t *testing.T) {
	deadline := hookConfigNotRunNote("/repo", 3, fmt.Errorf("read checkout marker: %w", context.DeadlineExceeded))
	require.Contains(t, deadline, "in time (3 attempts)")

	cancelled := hookConfigNotRunNote("/repo", 2, fmt.Errorf("probe: %w", errors.Join(config.ErrRepoProbeUnanswered, context.Canceled)))
	require.NotContains(t, cancelled, "in time", "a cancelled read did not run out of time")
	require.Contains(t, cancelled, "cancelled or git did not answer")

	invalid := hookConfigNotRunNote("/repo", 1, errors.New("parse config.toml: expected '=' after key; got newline"))
	require.Contains(t, invalid, "could not be resolved")

	for _, note := range []string{deadline, cancelled, invalid} {
		require.True(t, strings.HasPrefix(note, HookNotRunNotePrefix), note)
		require.NotContains(t, note, "; ", "a not-run note never contains the row's note separator: %s", note)
	}
}

func TestHookRunWithAnInvalidConfigSaysSoOnTheRow(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initCmd := exec.Command("git", "init", "--quiet")
	initCmd.Dir = repo
	initCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, config.InRepoConfigDirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, config.InRepoConfigDirName, "config.toml"),
		[]byte("post_worktree_commands = [\"touch x\"\n[broken"), 0o644))
	gw, err := NewGitWorktreeFromStorage(repo, repo, "hooks-invalid", "main", "", true, false)
	require.NoError(t, err)

	<-gw.runHooks()

	note := gw.SetupFailureNote()
	require.Contains(t, note, HookNotRunNotePrefix, "an invalid config is reported on the row, not in a log line alone")
	require.Contains(t, note, "could not be resolved")
}
