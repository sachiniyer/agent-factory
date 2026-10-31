package projectclone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateAccepts(t *testing.T) {
	for _, u := range []string{
		"https://github.com/owner/repo",
		"https://github.com/owner/repo.git",
		"https://user@example.com/owner/repo.git",
		"ssh://git@example.com/owner/repo.git",
		"ssh://git@example.com:2222/owner/repo.git",
		"git@github.com:owner/repo.git",
	} {
		require.NoError(t, Validate(u), u)
	}
}

func TestValidateRefuses(t *testing.T) {
	for u, why := range map[string]string{
		"file:///srv/secret/repo":                 "file://",
		"file://localhost/etc":                    "file://",
		"/srv/repo":                               "not a git URL",
		"./repo":                                  "not a git URL",
		"~/repo":                                  "not a git URL",
		"../repo":                                 "not a git URL",
		"git://example.com/owner/repo":            "not accepted",
		"http://example.com/owner/repo":           "not accepted",
		"ext::sh -c touch% /tmp/pwned":            "",
		"fd::17/foo":                              "transport helper",
		"-u./payload":                             "option",
		"--upload-pack=touch /tmp/x":              "option",
		"https://example.com/owner/repo extra":    "space",
		"https://example.com/owner/repo\nx":       "control",
		"https://example.com/a/../../etc":         "'..'",
		"git@example.com:../../etc/passwd":        "'..'",
		"https://user:secret@example.com/o/r.git": "password",
		"https:///owner/repo":                     "no host",
		"https://example.com/":                    "no repository",
		"ssh://example.com":                       "no repository",
		"":                                        "empty",
		" https://example.com/o/r":                "whitespace",
		"https://example.com\\@evil.com/o/r":      "backslash",
		"git@-oProxyCommand=x:owner/repo":         "",
	} {
		err := Validate(u)
		require.Error(t, err, u)
		require.Contains(t, err.Error(), why, u)
	}
}

func TestLooksLikeURLRoutesPathsAndURLs(t *testing.T) {
	for _, u := range []string{"https://x/y", "file:///etc", "ssh://h/p", "git@github.com:o/r.git"} {
		require.True(t, LooksLikeURL(u), u)
	}
	for _, p := range []string{".", "./repo", "/srv/repo", "~/repo", "repo", "C:repo", "a:b"} {
		require.False(t, LooksLikeURL(p), p)
	}
}

func TestRepoNameIsASingleSafeElement(t *testing.T) {
	for u, want := range map[string]string{
		"https://github.com/owner/repo.git": "repo",
		"https://github.com/owner/repo/":    "repo",
		"git@github.com:owner/my-repo.git":  "my-repo",
		"ssh://git@h:22/a/b/c.d.git":        "c.d",
	} {
		got, err := RepoName(u)
		require.NoError(t, err, u)
		require.Equal(t, want, got)
	}
	for _, u := range []string{"https://h/o/..", "https://h/o/.git", "https://h/o/a%20b", "https://h/o/a$b"} {
		_, err := RepoName(u)
		require.Error(t, err, u)
	}
}

func stubGit(t *testing.T, fn Runner) {
	t.Helper()
	prev := RunGit
	RunGit = fn
	t.Cleanup(func() { RunGit = prev })
}

func TestCloneRunsGitWithTheURLAfterDoubleDashUnderTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects")
	var gotArgs []string
	var gotDir string
	var gotEnv []string
	stubGit(t, func(_ context.Context, dir string, env []string, args ...string) ([]byte, error) {
		gotDir, gotArgs, gotEnv = dir, args, env
		return nil, os.MkdirAll(filepath.Join(dir, args[len(args)-1]), 0o755)
	})
	dest, err := Clone(context.Background(), root, "https://github.com/owner/repo.git")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "repo"), dest)
	require.Equal(t, root, gotDir)
	require.Equal(t, []string{"clone", "--no-recurse-submodules", "--", "https://github.com/owner/repo.git", "repo"}, gotArgs)
	require.Contains(t, gotEnv, "GIT_TERMINAL_PROMPT=0")
	require.Contains(t, gotEnv, "GIT_ALLOW_PROTOCOL=https:ssh")
}

func TestCloneNeverRunsGitOnAnInvalidURL(t *testing.T) {
	stubGit(t, func(context.Context, string, []string, ...string) ([]byte, error) {
		t.Fatal("git ran for a refused URL")
		return nil, nil
	})
	for _, u := range []string{"file:///srv/x", "/srv/x", "ext::sh", "-x"} {
		_, err := Clone(context.Background(), t.TempDir(), u)
		require.Error(t, err, u)
	}
}

func TestCloneFailureRemovesThePartialDirectoryAndReportsTheDaemonHost(t *testing.T) {
	root := t.TempDir()
	stubGit(t, func(_ context.Context, dir string, _ []string, args ...string) ([]byte, error) {
		_ = os.MkdirAll(filepath.Join(dir, args[len(args)-1], ".git"), 0o755)
		return []byte("fatal: Authentication failed"), errors.New("exit status 128")
	})
	_, err := Clone(context.Background(), root, "https://github.com/owner/private.git")
	require.ErrorContains(t, err, "on the daemon host")
	require.ErrorContains(t, err, "Authentication failed")
	_, statErr := os.Stat(filepath.Join(root, "private"))
	require.True(t, os.IsNotExist(statErr), "a failed clone leaves no partial checkout")
}

func TestCloneIsIdempotentForTheSameOriginAndRefusesAnotherOccupant(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "repo"), 0o755))
	origin := "https://github.com/owner/repo.git"
	stubGit(t, func(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
		if args[0] == "config" {
			return []byte(origin + "\n"), nil
		}
		t.Fatal("re-cloned an existing checkout")
		return nil, nil
	})
	dest, err := Clone(context.Background(), root, origin)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "repo"), dest)

	stubGit(t, func(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
		return []byte("https://github.com/someone/else.git\n"), nil
	})
	_, err = Clone(context.Background(), root, origin)
	require.ErrorContains(t, err, "already exists")
	require.False(t, strings.Contains(err.Error(), "<nil>"))
}

func TestCloneRefusesARelativeRoot(t *testing.T) {
	_, err := Clone(context.Background(), "relative/root", "https://github.com/o/r.git")
	require.ErrorContains(t, err, "must be absolute")
}
