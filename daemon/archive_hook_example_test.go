package daemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// documentedPruneCommand returns the on_archive_command example published in
// docs/configuration.md.
//
// These tests read the example rather than restating it on purpose. The
// published line is the one users copy into their config, so it is the thing
// that has to survive a real archive; a restated copy would keep passing after
// the documented command drifted away from it, which is the failure mode that
// let a broken example ship in the first place.
func documentedPruneCommand(t *testing.T) string {
	t.Helper()
	const doc = "../docs/configuration.md"
	raw, err := os.ReadFile(doc)
	require.NoError(t, err, "read %s", doc)

	var found []string
	for _, line := range strings.Split(string(raw), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "on_archive_command = ")
		if !ok {
			continue
		}
		found = append(found, strings.Trim(strings.TrimSpace(value), `'"`))
	}
	require.Len(t, found, 1,
		"%s must publish exactly one on_archive_command example; these tests execute it as the docs oracle for #2573", doc)
	require.NotEmpty(t, found[0], "the documented on_archive_command example must not be empty")
	return found[0]
}

// TestDocumentedPruneExampleSucceedsWithNoDependencyTree is the #2573
// regression, and the reason the published example matters as much as the hook.
//
// Once configured, the example runs on EVERY archive in scope — including
// sessions whose worktree never had a dependency tree at all: a Go repo, or a JS
// one archived before install ever ran. An example that exits non-zero there
// makes af report a failed on-archive hook, on surfaces the user reads (CLI,
// TUI, web), for an archive that did exactly what it should. That is worse than
// cosmetic: it teaches the operator that hook warnings are noise, and the
// warning is the only channel a genuinely failing cleanup command has.
func TestDocumentedPruneExampleSucceedsWithNoDependencyTree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	writeOnArchiveCommand(t, documentedPruneCommand(t))

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.NoError(t, err,
		"the documented example must succeed when there is nothing to prune; a worktree with no dependency tree is the common case, not an error")
}

// TestDocumentedPruneExamplePrunesNestedDependencyTreesAndSpareLinkedStores
// pins the two properties that decide whether the example reclaims what #2573
// measured, and whether it can destroy anything while doing it.
//
// Reclaim: the archived bulk was never one tree per session. #2740 counted 5,103
// node_modules directories across 741 archived sessions — roughly seven each,
// because a workspace repo puts one in the root and one in every package. An
// example that only prunes the root leaves most of the bytes behind on exactly
// the repo shape that caused the issue.
//
// Safety: a node_modules may be a SYMLINK into a store shared with every other
// worktree on the box (pnpm's default layout). Deleting through it would take
// out other sessions' dependencies — unrecoverable, from a command the user
// enabled to reclaim disk. The example must leave the link and its target alone,
// and it must never touch tracked content.
func TestDocumentedPruneExamplePrunesNestedDependencyTreesAndSparesLinkedStores(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")

	for _, dir := range []string{
		filepath.Join("node_modules", "pkg"),
		filepath.Join("apps", "web", "node_modules", "dep"),
		filepath.Join("apps", "api", "node_modules", "dep"),
		filepath.Join("node_modules", "inner", "node_modules", "deep"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(srcPath, dir), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(srcPath, dir, "bulk.js"), []byte("bulk"), 0o644))
	}

	kept := filepath.Join("apps", "web", "src")
	require.NoError(t, os.MkdirAll(filepath.Join(srcPath, kept), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcPath, kept, "index.ts"), []byte("keep"), 0o644))

	// The shared store lives outside the worktree, like a real package-manager
	// store, so following the link would reach content no archive owns.
	store := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(store, "shared.js"), []byte("shared"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(srcPath, "vendor"), 0o755))
	require.NoError(t, os.Symlink(store, filepath.Join(srcPath, "vendor", "node_modules")))

	writeOnArchiveCommand(t, documentedPruneCommand(t))

	archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	var leftover []string
	require.NoError(t, filepath.WalkDir(archivedPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// WalkDir reports a symlink as a non-directory, so the linked store below
		// is deliberately not counted here — only real trees are leftovers.
		if d.IsDir() && d.Name() == "node_modules" {
			rel, relErr := filepath.Rel(archivedPath, path)
			if relErr != nil {
				return relErr
			}
			leftover = append(leftover, rel)
			return filepath.SkipDir
		}
		return nil
	}))
	assert.Empty(t, leftover,
		"every real dependency tree must be pruned before the worktree moves; a root-only example leaves most of the archived bytes behind in a workspace repo")

	assert.FileExists(t, filepath.Join(archivedPath, kept, "index.ts"),
		"the example must not touch tracked content")
	assert.FileExists(t, filepath.Join(store, "shared.js"),
		"the example must not delete through a node_modules symlink into a store shared with every other worktree")
}

// TestDocumentedPruneExampleRunsFromPersonalProjectConfig pins the per-project
// half of the configuration contract through the real archive path.
//
// Per-project is the shape #2573 actually calls for: the cost concentrated in
// ONE project on that box — the heavy JS repo — while every other project
// archived cheaply, so a global command would run a JS prune over repos that
// have nothing to prune. Resolution across both layers is covered in the config
// package; what is pinned here is that the personal project layer reaches the
// archive at all, which resolving a value does not prove.
func TestDocumentedPruneExampleRunsFromPersonalProjectConfig(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")
	require.NoError(t, os.MkdirAll(filepath.Join(srcPath, "node_modules", "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcPath, "node_modules", "pkg", "bulk.js"), []byte("bulk"), 0o644))

	project, err := config.RegisterProject(repoPath)
	require.NoError(t, err)
	personal, err := config.ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(personal), 0o755))
	require.NoError(t, os.WriteFile(personal,
		[]byte("on_archive_command = '"+documentedPruneCommand(t)+"'\n"), 0o644))

	archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	assert.NoDirExists(t, filepath.Join(archivedPath, "node_modules"),
		"an on_archive_command set for this project only must run when one of its sessions is archived")
}
