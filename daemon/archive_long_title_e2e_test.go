package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerArchivableLongTitle is registerArchivable for a long session title whose
// SOURCE worktree must keep a short, NAME_MAX-safe path. Production bounds the
// source worktree directory via boundWorktreeComponent (#2528) while leaving the
// session title itself uncapped (validateTitleShapeLocked checks only shape), so a
// running long-titled session has a short worktree path but a long title. This
// helper mirrors that: a short wtName for the on-disk worktree path and branch,
// but the session title is the (long) title under test. The archive destination is
// then derived from the long title via archivedWorktreePath -> sanitizeArchiveTitle.
func registerArchivableLongTitle(t *testing.T, m *Manager, repoID, repoPath, title, wtName string) (*session.Instance, string) {
	t.Helper()
	wtPath := filepath.Join(filepath.Dir(repoPath), "wt-"+wtName)
	branch := "af-" + wtName
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted"), 0644))

	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, title, branch, "", false, true)
	require.NoError(t, err)

	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetGitWorktreeForTest(gw)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)

	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst, wtPath
}

// longArchiveTitle is a session title whose sanitized archive leaf exceeds
// NAME_MAX (255) before the fix: 300 ASCII bytes. Such a title is a valid running
// session (the authoritative validateTitleShapeLocked checks only shape; only the
// TUI caps its naming input at 32 chars), reachable via the CLI/RPC/HTTP.
func longArchiveTitle() string { return "long-title-" + strings.Repeat("a", 289) } // 300 bytes

// TestArchiveLongTitleE2E is the archive-side regression for the long-title class
// the report reproduces: a > NAME_MAX (255-byte) session title is a valid, running
// session (the authoritative validateTitleShapeLocked has no length cap; only the
// TUI caps naming at 32 chars), but archiving it always failed with ENAMETOOLONG —
// at destination inspection when the repo already had a prior archive (Case A), or
// at the worktree relocation otherwise (Case C git worktree move fast path; the
// os.Rename fallback Case B is covered at the session/git layer in
// TestMoveWorktree_BoundedLongDestLeaf, where worktreeMoveFast is stubbable). The
// worktree side was bounded in #2528; the archive leaf is bounded by
// sanitizeArchiveTitle. Each subtest drives the real ArchiveSession path with a
// 300-byte title and asserts the archive completes.
func TestArchiveLongTitleE2E(t *testing.T) {
	longTitle := longArchiveTitle()

	t.Run("fresh_fast_path", func(t *testing.T) {
		// Case C: first archive in a repo on the production `git worktree move` fast
		// path. Against the unbounded sanitizer `git worktree move <src> <300-byte
		// leaf>` fails with "File name too long"; with the bound it moves to a
		// 255-byte leaf.
		manager, repoID, repoPath := newStatusTestManager(t)
		inst, srcPath := registerArchivableLongTitle(t, manager, repoID, repoPath, longTitle, "longsrc")

		archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{ID: inst.ID, Title: longTitle, RepoID: repoID})
		require.NoError(t, err, "archiving a > NAME_MAX title must not fail with file name too long")

		leaf := filepath.Base(archivedPath)
		require.NotEmpty(t, leaf)
		assert.LessOrEqualf(t, len(leaf), archiveLeafNameMax, "archive leaf %d bytes over NAME_MAX %d", len(leaf), archiveLeafNameMax)
		assert.False(t, exists(srcPath), "the original worktree directory must be gone")
		assert.True(t, exists(archivedPath), "the worktree must exist at the archive path")
		dirty, rerr := os.ReadFile(filepath.Join(archivedPath, "dirty.txt"))
		require.NoError(t, rerr, "the uncommitted tree must survive the archive move")
		assert.Equal(t, "uncommitted", string(dirty))
		assert.Equal(t, session.Archived, inst.GetStatus())

		list, lerr := exec.Command("git", "-C", repoPath, "worktree", "list", "--porcelain").CombinedOutput()
		require.NoError(t, lerr, string(list))
		assert.Contains(t, string(list), archivedPath, "git must register the worktree at its bounded archive path")
	})

	t.Run("prior_archive_exists", func(t *testing.T) {
		// Case A: the repo already has a prior archive, so the parent
		// <home>/archived/<repoID>/ directory exists. inspectArchiveDestination's
		// BoundedLstat of the long destination is the FIRST probe; against the
		// unbounded sanitizer it returns ENAMETOOLONG and the archive aborts at
		// inspection. With the bound, BoundedLstat returns ENOENT (missing) and the
		// archive proceeds to the move.
		manager, repoID, repoPath := newStatusTestManager(t)

		// First: archive a short-title session so the archive parent dir exists.
		shortInst, shortSrc := registerArchivableLongTitle(t, manager, repoID, repoPath, "prior-short", "priorsrc")
		_, _, err := manager.ArchiveSession(ArchiveSessionRequest{ID: shortInst.ID, Title: "prior-short", RepoID: repoID})
		require.NoError(t, err)
		assert.False(t, exists(shortSrc))
		parent, perr := archivedWorktreePath(repoID, "prior-short")
		require.NoError(t, perr)
		require.True(t, exists(filepath.Dir(parent)), "the archive parent directory must exist after a prior archive")

		// Now archive a long-title session in the SAME repo: the parent exists, so
		// the long destination is inspected (not treated as a missing-parent fast
		// path). Without the bound this is the Case A ENAMETOOLONG at inspection.
		inst, srcPath := registerArchivableLongTitle(t, manager, repoID, repoPath, longTitle, "longsrc3")
		archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{ID: inst.ID, Title: longTitle, RepoID: repoID})
		require.NoError(t, err, "archiving a long title when the archive parent exists must not fail at destination inspection")

		leaf := filepath.Base(archivedPath)
		assert.LessOrEqualf(t, len(leaf), archiveLeafNameMax, "archive leaf %d bytes over NAME_MAX %d", len(leaf), archiveLeafNameMax)
		assert.False(t, exists(srcPath))
		assert.True(t, exists(archivedPath))
		assert.Equal(t, session.Archived, inst.GetStatus())
	})
}

// TestArchiveLongTitleCreateCollision verifies guarantee 9 end-to-end: two distinct
// long titles that share their first 255 sanitized bytes collapse to one
// archiveTitleKey and the second create is REJECTED at admission, not silently
// aliased on disk. CreateSession runs the full admission path (branch-side guard
// first, then the archive-namespace guard); the second must be refused. Mirrors
// TestCreateSessionRejectsArchiveDirectoryCollision for the long-title truncation
// class.
func TestArchiveLongTitleCreateCollision(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Two distinct titles whose sanitized forms share their first 255 bytes —
	// both 300 bytes, differing only in the final byte, so sanitizeArchiveTitle
	// truncates each to the same 255-byte leaf and archiveTitleKey collides.
	titleA := strings.Repeat("a", 300)
	titleB := strings.Repeat("a", 299) + "b"
	require.Equal(t, sanitizeArchiveTitle(titleA), sanitizeArchiveTitle(titleB),
		"both long titles must truncate to the same archive leaf")

	_, err = m.CreateSession(context.Background(), CreateSessionRequest{Title: titleA, RepoPath: repoPath, Program: "claude"})
	require.NoError(t, err, "first long-title create must succeed (shape-valid, bounded source worktree)")

	_, err = m.CreateSession(context.Background(), CreateSessionRequest{Title: titleB, RepoPath: repoPath, Program: "claude"})
	require.Error(t, err, "a second title truncating to the same archive leaf must be rejected, not silently aliased")
	// The branch-side guard (truncating at 200) or the archive-namespace guard
	// (truncating at 255) rejects it; both prove no silent on-disk aliasing.
	assert.True(t,
		strings.Contains(err.Error(), "already maps to archive directory") ||
			strings.Contains(err.Error(), "branch") || strings.Contains(err.Error(), "already"),
		"second long-title create must be refused at admission: %v", err)
}

// TestArchiveLongTitleArchiveNamespaceCollision exercises the archive-namespace
// guard in isolation (validateArchiveTitleLocked), so guarantee 9 is verified at
// the archive layer regardless of which order the branch-side guard runs. Two
// 300-byte titles sharing their first 255 sanitized bytes must collide in the
// portable archive namespace. This is host-safe (pure Manager map, no tmux/worktree).
func TestArchiveLongTitleArchiveNamespaceCollision(t *testing.T) {
	titleA := strings.Repeat("a", 300)
	titleB := strings.Repeat("a", 299) + "b"
	require.Equal(t, sanitizeArchiveTitle(titleA), sanitizeArchiveTitle(titleB))
	require.True(t, archiveTitlesCollide(titleA, titleB), "truncating long titles must collide in the archive namespace")

	m := &Manager{
		instances:             make(map[string]*session.Instance),
		reservedArchiveTitles: make(map[string]struct{}),
	}
	inst := &session.Instance{Title: titleA}
	m.instances[daemonInstanceKey("repo", titleA)] = inst

	err := m.validateArchiveTitleLocked("repo", titleB, nil, nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already maps to archive directory")
}
