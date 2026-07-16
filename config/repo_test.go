package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canonicalTempDir returns t.TempDir() resolved to its physical path.
//
// resolveMainRepoRoot reports what git resolves, which is the physical path. On
// macOS t.TempDir() sits under /var/folders/…, and /var is a symlink to
// /private/var — so an expectation written in t.TempDir()'s spelling compares
// /var/… against /private/var/… and fails there while passing on Linux
// (#1918). Canonicalizing at the source keeps every assertion below written
// in the same spelling production uses; it is a no-op on an already-canonical
// path, so Linux is unaffected.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err, "canonicalize temp dir")
	return dir
}

func TestResolveMainRepoRoot_MainRepo(t *testing.T) {
	// Create a standalone git repo so the test doesn't depend on cwd
	// (which may itself be a worktree).
	mainDir := canonicalTempDir(t)

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
	}

	run(mainDir, "init")
	run(mainDir, "config", "user.email", "test@test.com")
	run(mainDir, "config", "user.name", "Test")

	require.NoError(t, os.WriteFile(filepath.Join(mainDir, "file.txt"), []byte("hello"), 0644))
	run(mainDir, "add", ".")
	run(mainDir, "commit", "-m", "init")

	root, err := resolveMainRepoRoot("-C", mainDir)
	require.NoError(t, err)
	assert.Equal(t, mainDir, root)
}

func TestResolveMainRepoRoot_Worktree(t *testing.T) {
	// Create a temporary git repo and a linked worktree, then verify
	// that resolveMainRepoRoot from the worktree returns the main repo root.
	mainDir := canonicalTempDir(t)

	// Initialize a git repo
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
	}

	run(mainDir, "init")
	run(mainDir, "config", "user.email", "test@test.com")
	run(mainDir, "config", "user.name", "Test")

	// Create an initial commit so we can create a worktree
	dummy := filepath.Join(mainDir, "file.txt")
	require.NoError(t, os.WriteFile(dummy, []byte("hello"), 0644))
	run(mainDir, "add", ".")
	run(mainDir, "commit", "-m", "init")

	// Create a linked worktree
	wtDir := filepath.Join(canonicalTempDir(t), "my-worktree")
	run(mainDir, "worktree", "add", wtDir, "-b", "test-branch")

	// resolveMainRepoRoot from the worktree should return mainDir
	root, err := resolveMainRepoRoot("-C", wtDir)
	require.NoError(t, err)
	assert.Equal(t, mainDir, root)

	// RepoFromPath should also resolve to the main repo
	repoFromWT, err := RepoFromPath(wtDir)
	require.NoError(t, err)
	repoFromMain, err := RepoFromPath(mainDir)
	require.NoError(t, err)
	assert.Equal(t, repoFromMain.ID, repoFromWT.ID)
	assert.Equal(t, repoFromMain.Root, repoFromWT.Root)
}

func TestResolveMainRepoRoot_Public(t *testing.T) {
	// Verify the exported wrapper works the same as the internal function.
	mainDir := canonicalTempDir(t)

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
	}

	run(mainDir, "init")
	run(mainDir, "config", "user.email", "test@test.com")
	run(mainDir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(mainDir, "file.txt"), []byte("hello"), 0644))
	run(mainDir, "add", ".")
	run(mainDir, "commit", "-m", "init")

	// Create a linked worktree
	wtDir := filepath.Join(canonicalTempDir(t), "wt-public")
	run(mainDir, "worktree", "add", wtDir, "-b", "public-test-branch")

	// ResolveMainRepoRoot from the worktree should return mainDir
	root, err := ResolveMainRepoRoot(wtDir)
	require.NoError(t, err)
	assert.Equal(t, mainDir, root)

	// ResolveMainRepoRoot from the main repo should also return mainDir
	root, err = ResolveMainRepoRoot(mainDir)
	require.NoError(t, err)
	assert.Equal(t, mainDir, root)
}

func TestRepoIDFromRoot(t *testing.T) {
	id := RepoIDFromRoot("/some/path")
	assert.Len(t, id, 12) // 6 bytes = 12 hex chars
}

// TestValidateRepoID_PathTraversalRejected covers the daemon RPC path
// traversal exploit class from #515. Every input that could break out of
// the per-repo file scope must be rejected.
func TestValidateRepoID_PathTraversalRejected(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"dotdot-slash", "../"},
		{"deep-traversal", "../../../etc/passwd"},
		{"embedded-traversal", "foo/../bar"},
		{"trailing-traversal", "abc/.."},
		{"absolute-path", "/etc/passwd"},
		{"windows-absolute", "C:\\windows\\system32"},
		{"forward-slash", "foo/bar"},
		{"backslash", "foo\\bar"},
		{"null-byte", "foo\x00bar"},
		{"newline", "foo\nbar"},
		{"tilde", "~/secrets"},
		{"hidden", ".hidden"},
		{"glob", "foo*"},
		{"space", "foo bar"},
		{"unicode-traversal", "foo/../bar"},
		{"too-long", strings.Repeat("a", maxRepoIDLength+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRepoID(tc.input)
			assert.Error(t, err, "expected %q to be rejected", tc.input)
		})
	}
}

// TestValidateRepoID_LegitimateAccepted ensures real-world repo IDs from
// RepoIDFromRoot, plus the test fixture IDs already used elsewhere in the
// codebase, continue to validate.
func TestValidateRepoID_LegitimateAccepted(t *testing.T) {
	cases := []string{
		RepoIDFromRoot("/some/path"), // 12 hex chars from production helper
		"abc123def456",
		"AAAA1111BBBB",
		"test-repo-id",
		"test-repo-no-hooks",
		"ghost-repo",
		"json-roundtrip",
		"underscore_id",
		"a",
		strings.Repeat("a", maxRepoIDLength),
	}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			assert.NoError(t, ValidateRepoID(id))
		})
	}
}
