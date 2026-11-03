package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The setup overlay is the second half of #6667: whatever the daemon-side
// setup_commands staged into the session's payload dir is copied into the
// freshly created worktree AFTER `git worktree add` and BEFORE
// post_worktree_commands run, so those hooks (and the agent) see the files.

func commitFile(t *testing.T, repoRoot, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, name), []byte(content), 0644))
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "c"}} {
		cmd := exec.Command("git", "-C", repoRoot, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
}

func TestSetupOverlayCopiesPayloadIntoWorktree(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	commitFile(t, repoRoot, "main.go", "package main\n")

	payload := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(payload, ".env"), []byte("A=1\n"), 0600))
	require.NoError(t, os.MkdirAll(filepath.Join(payload, "sub", "dir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(payload, "sub", "dir", "local.conf"), []byte("x\n"), 0644))

	gw, _, err := NewGitWorktree(repoRoot, "overlay-session", branchPrefixForTest(t))
	require.NoError(t, err)
	gw.SetSetupOverlayDir(payload, payload)
	require.NoError(t, gw.Setup())

	wt := gw.GetWorktreePath()
	envInfo, err := os.Stat(filepath.Join(wt, ".env"))
	require.NoError(t, err, ".env must land in the worktree root")
	assert.Equal(t, os.FileMode(0600), envInfo.Mode().Perm(), "payload modes are preserved")
	assert.FileExists(t, filepath.Join(wt, "sub", "dir", "local.conf"))
	assert.Equal(t, "", gw.SetupOverlayError())

	_, cleanupErr := gw.Cleanup()
	require.NoError(t, cleanupErr)
}

// Ordering is the point of the feature: the payload must be in place before
// post_worktree_commands run, so a hook that depends on a staged file works.
func TestSetupOverlayRunsBeforePostWorktreeHooks(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)

	// The hook must be committed — post-worktree resolution reads the
	// worktree's checked-in config, not the source checkout's.
	dir := filepath.Join(repoRoot, ".agent-factory")
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte(`post_worktree_commands = ["test -f .env && touch .hook-saw-env"]`+"\n"), 0644))
	commitFile(t, repoRoot, ".agent-factory/config.toml", "")

	payload := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(payload, ".env"), []byte("A=1\n"), 0644))

	gw, _, err := NewGitWorktree(repoRoot, "overlay-order", branchPrefixForTest(t))
	require.NoError(t, err)
	gw.SetSetupOverlayDir(payload, payload)
	require.NoError(t, gw.Setup())
	<-gw.HooksDone()

	assert.FileExists(t, filepath.Join(gw.GetWorktreePath(), ".hook-saw-env"),
		"the post-worktree hook must observe the overlaid .env — overlay ran first")

	_, cleanupErr := gw.Cleanup()
	require.NoError(t, cleanupErr)
}

// A staging dir that does not exist (no setup_commands ran) is not an error —
// it is the common case.
func TestSetupOverlayMissingDirIsNoOp(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	commitFile(t, repoRoot, "main.go", "package main\n")

	gw, _, err := NewGitWorktree(repoRoot, "overlay-empty", branchPrefixForTest(t))
	require.NoError(t, err)
	gw.SetSetupOverlayDir(filepath.Join(t.TempDir(), "does-not-exist"), "")
	require.NoError(t, gw.Setup())
	assert.Equal(t, "", gw.SetupOverlayError())

	_, cleanupErr := gw.Cleanup()
	require.NoError(t, cleanupErr)
}

// An empty staging dir is equally not an error — a script may only run host
// commands and stage nothing.
func TestSetupOverlayEmptyDirIsNoOp(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	commitFile(t, repoRoot, "main.go", "package main\n")

	gw, _, err := NewGitWorktree(repoRoot, "overlay-emptydir", branchPrefixForTest(t))
	require.NoError(t, err)
	gw.SetSetupOverlayDir(t.TempDir(), "")
	require.NoError(t, gw.Setup())
	assert.Equal(t, "", gw.SetupOverlayError())

	_, cleanupErr := gw.Cleanup()
	require.NoError(t, cleanupErr)
}

// A copy failure is recorded on the worktree (the instance reads it after
// Setup) rather than propagated — same non-blocking rule as the hook itself.
func TestSetupOverlayErrorRecordedOnFailure(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.Chmod(wt, 0555))
	t.Cleanup(func() { _ = os.Chmod(wt, 0755) })

	payload := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(payload, ".env"), []byte("A=1\n"), 0644))

	g := &GitWorktree{worktreePath: wt}
	g.SetSetupOverlayDir(payload, "")
	g.applySetupOverlay()
	assert.NotEqual(t, "", g.SetupOverlayError(), "a failed payload copy must be recorded")
}
