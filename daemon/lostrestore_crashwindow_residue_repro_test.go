package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestRestoreLostSessions_RefusesRebuildOverCrashWindowResidue is the
// daemon-level regression for the #7804/#7807 crash window on the restore side.
// It drives the production lost-restore loop (manager.RestoreLostSessions) over
// a real tmux-backed session whose worktree was renamed to a
// `.af-source-<32hex>` quarantine by a daemon death between the quarantine and
// publish renames of a cross-device archive move, and asserts:
//
//  1. the loop refuses to rebuild the worktree (the row stays Lost, the recorded
//     path stays absent), preserving the stranded complete worktree with its
//     uncommitted changes rather than silently re-checking out the branch;
//  2. a subsequent archive still refuses over the residue — the rescue window
//     the archive-side bare-ENOENT arbitration (#7804/#7807) needs is preserved,
//     not bypassed the way a rebuild that re-creates the recorded path would.
//
// Before the fix, the rebuild re-created the recorded path from the branch tip,
// pruned git's stale registration, and left the `.af-source-*` quarantine
// permanently unreferenced — so every later archive routed through the
// source-present path and the residue refusal never ran again (silent loss of
// the uncommitted work).
//
// This test drives a real tmux server (it creates and kills a real session),
// so it is a CI daemon test rather than a local-check one — see
// daemon/lostrestore_test.go and TestOutageEndToEnd_LostRestoreAndReplay for the
// same convention.
func TestRestoreLostSessions_RefusesRebuildOverCrashWindowResidue(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	// Cheap real agent: prints a banner and idles, so readiness is the generic
	// any-output heuristic (#1132) — no agent token, no injected flags.
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{"claude": "sh -c 'echo agent-ready; exec sleep 600'"}
	require.NoError(t, config.SaveConfig(cfg))

	manager, err := NewManager(cfg)
	require.NoError(t, err)

	data, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "crash-window-repro",
		RepoPath: repoPath,
		Program:  "claude",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		// Remove the stranded quarantine if the test left it, so a failure
		// does not orphan it under the temp home; the kill tears down the
		// (refused, never-respawned) instance's record.
		_, _ = manager.KillSession(KillSessionRequest{Title: "crash-window-repro", RepoID: repo.ID})
		_ = os.RemoveAll(filepath.Join(filepath.Dir(data.Worktree.WorktreePath),
			".af-source-"+strings.Repeat("a", 32)))
	})
	key := daemonInstanceKey(repo.ID, "crash-window-repro")
	manager.mu.Lock()
	inst := manager.instances[key]
	manager.mu.Unlock()
	require.NotNil(t, inst)
	wtPath := data.Worktree.WorktreePath
	require.NotEmpty(t, wtPath)
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted"), 0o644))

	// ---- The outage: the tmux session dies out from under the live record.
	out, err := exec.Command("tmux", "kill-session", "-t", "="+data.TmuxName).CombinedOutput()
	require.NoError(t, err, string(out))
	waitUntil(t, 10*time.Second, "the vanished session to be classified Lost", func() bool {
		manager.RefreshStatuses()
		return inst.GetStatus() == session.Lost
	})

	// ---- The crash-window footprint: the complete worktree is renamed
	// wholesale to a `.af-source-<32hex>` sibling (raw rename, so git's
	// registration still names the now-absent recorded path), with no
	// relocation record. This is exactly where a cross-device archive move cut
	// off between its quarantine and publish renames leaves the session, and
	// exactly the state the lost-restore loop restarts onto.
	quarantine := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("a", 32))
	require.NoError(t, os.Rename(wtPath, quarantine))

	// ---- The lost-restore loop must refuse to rebuild over the quarantine.
	zeroRestoreBackoff(t)
	for i := 0; i < 3; i++ {
		manager.RestoreLostSessions()
	}
	assert.Equal(t, session.Lost, inst.GetStatus(),
		"the lost-restore loop must not rebuild over the crash-window quarantine")

	// The stranded worktree survives untouched with its uncommitted file — the
	// exact loss the residue arbitration exists to prevent.
	dirty, rerr := os.ReadFile(filepath.Join(quarantine, "dirty.txt"))
	require.NoError(t, rerr, "the stranded worktree must survive the refused restore")
	assert.Equal(t, "uncommitted", string(dirty))
	// The recorded path stays absent: the archive-side bare-ENOENT route is
	// still reachable on a retry.
	_, statErr := os.Stat(wtPath)
	assert.True(t, os.IsNotExist(statErr),
		"the recorded path must stay absent so the archive-side residue arbitration still runs")

	// ---- A subsequent archive must refuse over the residue the refused
	// rebuild left in place — proving the rescue window is preserved rather
	// than bypassed (the buggy rebuild re-creates the source, so the archive
	// takes the source-present path and the residue refusal never runs).
	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "crash-window-repro", RepoID: repo.ID})
	require.Error(t, err, "the archive must refuse over the residue the refused rebuild left in place")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"the archive refusal must be shaped as an unresolved relocation like the archive-side crash-window guard")
	assert.Contains(t, err.Error(), ".af-source-", "the archive refusal must name the stranded residue")
}
