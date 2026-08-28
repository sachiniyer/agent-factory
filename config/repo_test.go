package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
)

func TestResolveMainRepoRoot_MainRepo(t *testing.T) {
	// Create a standalone git repo so the test doesn't depend on cwd
	// (which may itself be a worktree).
	mainDir := testguard.CanonicalTempDir(t)

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
	mainDir := testguard.CanonicalTempDir(t)

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
	wtDir := filepath.Join(testguard.CanonicalTempDir(t), "my-worktree")
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

// TestResolveMainRepoRoot_BareCloneWorktree pins #3358: a linked worktree's
// common directory is itself the repository when core.bare=true. Its identity
// root is therefore the bare directory, never that directory's non-repository
// parent.
func TestResolveMainRepoRoot_BareCloneWorktree(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	parent := testguard.CanonicalTempDir(t)
	source := filepath.Join(parent, "source")
	bare := filepath.Join(parent, "bare.git")
	worktree := filepath.Join(parent, "worktree")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
	}

	run(parent, "init", source)
	run(source, "config", "user.email", "test@test.com")
	run(source, "config", "user.name", "Test")
	run(source, "commit", "--allow-empty", "-m", "init")
	run(parent, "clone", "--bare", source, bare)
	run(bare, "worktree", "add", worktree)

	root, err := resolveMainRepoRoot("-C", worktree)
	require.NoError(t, err)
	assert.Equal(t, bare, root)

	repo, err := RepoFromPath(worktree)
	require.NoError(t, err)
	assert.Equal(t, worktree, repo.Root)
	assert.Equal(t, worktree, repo.WorkspacePath())
	assert.Equal(t, bare, repo.IdentityPath())
	assert.Equal(t, RepoIDFromRoot(bare), repo.ID)
	assert.Equal(t, repo.ID, RepoIDForPath(worktree))
	assert.NotEqual(t, RepoIDFromRoot(parent), repo.ID)
	directBare, err := RepoFromPath(bare)
	require.NoError(t, err)
	assert.Equal(t, bare, directBare.Root)
	assert.Equal(t, bare, directBare.IdentityPath())
	assert.Equal(t, repo.ID, directBare.ID)

	writeLegacyRepoConfig(t, repo.ID, &RepoConfig{
		PostWorktreeCommands: []string{"legacy-from-bare-identity"},
	})
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, InRepoConfigDirName), 0o755))
	require.NoError(t, os.WriteFile(InRepoTomlConfigPath(worktree),
		[]byte("default_program = \"codex\"\n"), 0o644))
	resolved, err := ResolveConfigForRepo(repo)
	require.NoError(t, err)
	assert.Equal(t, worktree, resolved.ProjectRoot)
	assert.Equal(t, "codex", resolved.DefaultProgram)
	assert.Equal(t, []string{"legacy-from-bare-identity"}, resolved.PostWorktreeCommands)

	project, err := RegisterProject(worktree)
	require.NoError(t, err)
	writePersonalConfig(t, project.ID, "on_archive_command = \"bare-personal\"\n")
	secondWorktree := filepath.Join(parent, "worktree-2")
	run(bare, "worktree", "add", "-b", "second", secondWorktree)
	secondRepo, err := RepoFromPath(secondWorktree)
	require.NoError(t, err)
	secondResolved, err := ResolveConfigForRepo(secondRepo)
	require.NoError(t, err)
	assert.Equal(t, secondWorktree, secondResolved.ProjectRoot)
	assert.Equal(t, "bare-personal", secondResolved.OnArchiveCommand,
		"personal project config follows bare identity across linked worktrees")
}

func TestRepoFromDirectBarePathPreservesTrailingWhitespace(t *testing.T) {
	for name, suffix := range map[string]string{"space": " ", "tab": "\t"} {
		t.Run(name, func(t *testing.T) {
			parent := testguard.CanonicalTempDir(t)
			bare := filepath.Join(parent, "bare.git"+suffix)
			cmd := exec.Command("git", "init", "--bare", bare)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "git init --bare: %s", out)

			repo, err := RepoFromPath(bare)
			require.NoError(t, err)
			assert.Equal(t, bare, repo.IdentityPath())
			assert.Equal(t, RepoIDFromRoot(bare), repo.ID)
		})
	}
}

func TestResolveConfigForRepoWarnsAboutRetainedLegacyBareParentConfig(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	parent := testguard.CanonicalTempDir(t)
	source := filepath.Join(parent, "source")
	bare := filepath.Join(parent, "bare.git")
	worktree := filepath.Join(parent, "worktree")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
	}
	run(parent, "init", source)
	run(source, "config", "user.email", "test@test.com")
	run(source, "config", "user.name", "Test")
	run(source, "commit", "--allow-empty", "-m", "init")
	run(parent, "clone", "--bare", source, bare)
	run(bare, "worktree", "add", worktree)

	repo, err := RepoFromPath(worktree)
	require.NoError(t, err)
	legacyRoot, legacyID := repo.LegacyBareRepoIdentity()
	require.Equal(t, parent, legacyRoot)
	writeLegacyRepoConfig(t, legacyID, &RepoConfig{
		PostWorktreeCommands: []string{"ambiguous-parent-command"},
	})
	_, legacyPath, err := repoConfigPath(legacyID)
	require.NoError(t, err)
	warnings := captureLog(t, &aflog.WarningLog)

	resolved, err := ResolveConfigForRepo(repo)
	require.NoError(t, err)
	assert.Empty(t, resolved.PostWorktreeCommands,
		"an ambiguous parent-keyed config must not be silently adopted")
	assert.Contains(t, warnings.String(), legacyID)
	assert.Contains(t, warnings.String(), legacyPath)
	assert.Contains(t, warnings.String(), "was not applied")
}

func TestResolveMainRepoRoot_Public(t *testing.T) {
	// Verify the exported wrapper works the same as the internal function.
	mainDir := testguard.CanonicalTempDir(t)

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
	wtDir := filepath.Join(testguard.CanonicalTempDir(t), "wt-public")
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

// TestResolveMainRepoRootForcesStableGitDiagnostics pins the locale-independent
// outside-repository classification. The resolver may parse Git's diagnostic to
// distinguish ordinary absence from broken metadata, so the command must force
// the language that parser expects (#3134 review).
func TestResolveMainRepoRootForcesStableGitDiagnostics(t *testing.T) {
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	require.NoError(t, os.WriteFile(fakeGit, []byte(`#!/bin/sh
if [ "$LC_ALL" = "C" ]; then
	printf '%s\n' 'fatal: not a git repository (or any of the parent directories): .git' >&2
else
	printf '%s\n' 'fatal: Kein Git-Repository (oder irgendeines der Elternverzeichnisse): .git' >&2
fi
exit 128
`), 0o755))
	t.Setenv("PATH", binDir)
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Chdir(t.TempDir())

	_, err := CurrentRepo()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotGitRepository,
		"an ordinary outside-repository result must not become a fatal scope error under a translated locale")
}

// TestResolveRepoRootRecognizesFilesystemBoundaryDiagnostic pins recognition
// of Git's second outside-repository message. When repo discovery stops at a
// filesystem boundary (tmpfs /tmp, a separate mount), Git emits the
// "or any parent up to mount point" variant instead of the ceiling variant.
// Both set *nongit_ok in setup.c, so both must resolve to ErrNotGitRepository
// when no .git exists up the path hierarchy (fail-closed guard still applies).
func TestResolveRepoRootRecognizesFilesystemBoundaryDiagnostic(t *testing.T) {
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	require.NoError(t, os.WriteFile(fakeGit, []byte(`#!/bin/sh
printf '%s\n' 'fatal: not a git repository (or any parent up to mount point /)' >&2
printf '%s\n' 'Stopping at filesystem boundary (GIT_DISCOVERY_ACROSS_FILESYSTEM not set).' >&2
exit 128
`), 0o755))
	t.Setenv("PATH", binDir)
	t.Chdir(t.TempDir())

	_, err := CurrentRepo()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotGitRepository,
		"Git's filesystem-boundary variant of the outside-repository diagnostic must fall back to global scope")
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
