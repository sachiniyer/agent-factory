package session

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func originGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// OriginBranchHead names the branch's commit on the project's origin, says ""
// when origin positively lacks the branch, and errors when there is no origin
// to ask.
func TestOriginBranchHead(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	originGit(t, root, "init", "-q", "--bare", origin)
	originGit(t, root, "init", "-q", "-b", "main", repo)
	originGit(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	originGit(t, repo, "remote", "add", "origin", origin)
	originGit(t, repo, "push", "-q", "origin", "main:af/b-2-a")
	want := originGit(t, repo, "rev-parse", "HEAD")

	got, err := OriginBranchHead(repo, "af/b-2-a", 10*time.Second)
	require.NoError(t, err)
	require.Equal(t, want, got)

	got, err = OriginBranchHead(repo, "af/never-pushed", 10*time.Second)
	require.NoError(t, err, "origin answered: it does not have the branch")
	require.Empty(t, got)

	originGit(t, repo, "remote", "remove", "origin")
	_, err = OriginBranchHead(repo, "af/b-2-a", 10*time.Second)
	require.ErrorContains(t, err, "no origin remote")
}
