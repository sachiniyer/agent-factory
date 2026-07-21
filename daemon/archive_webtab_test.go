package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webTabsIn returns the web tabs of a persisted record, in order.
func webTabsIn(tabs []session.TabData) []session.TabData {
	var out []session.TabData
	for _, t := range tabs {
		if t.Kind == session.TabKindWeb {
			out = append(out, t)
		}
	}
	return out
}

// TestArchiveSession_PersistsWebTabs is the #1809 regression at the daemon layer:
// archiving a session must carry its web tabs — kind, name and target URL — into
// the ARCHIVED ON-DISK RECORD. The bug erased them at archive time, so restore had
// nothing to rebuild from and the URLs were unrecoverable.
func TestArchiveSession_PersistsWebTabs(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddWebTabForTest("webpreview", "http://localhost:3000")
	inst.AddTabForTest("watcher", session.TabKindProcess)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	require.Equal(t, session.Archived, inst.GetStatus())

	rec := recordFor(t, repoID, "worker")
	require.NotNil(t, rec)
	web := webTabsIn(rec.Tabs)
	require.Len(t, web, 1, "the archived record must retain the web tab (#1809)")
	assert.Equal(t, "webpreview", web[0].Name)
	assert.Equal(t, "http://localhost:3000", web[0].URL, "the target URL must survive archive")
	assert.Equal(t, "", web[0].TmuxName, "a web tab has no tmux session")

	// The process tab still drops: the archive teardown tore its tmux down.
	require.Len(t, rec.Tabs, 2, "the archived record holds the agent tab + the web tab only")
	assert.Equal(t, session.TabKindAgent, rec.Tabs[0].Kind)
}

// TestRestoreArchived_BringsBackWebTabs closes the #1809 loop end to end through
// the daemon: web tab → archive → restore, and the restored live session exposes
// the web tab again with the same URL, while the agent tab is re-spawned and
// process tabs stay dropped.
func TestRestoreArchived_BringsBackWebTabs(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddWebTabForTest("webpreview", "http://localhost:3000")
	inst.AddTabForTest("watcher", session.TabKindProcess)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	_, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	require.Equal(t, session.Running, inst.GetStatus(), "a restored session is Running")

	tabs := inst.GetTabs()
	require.Len(t, tabs, 2, "a restored session has its agent tab and its web tab")
	assert.Equal(t, session.TabKindAgent, tabs[0].Kind, "the agent tab is re-spawned")
	assert.Equal(t, session.TabKindWeb, tabs[1].Kind, "the web tab renders again after restore (#1809)")
	assert.Equal(t, "webpreview", tabs[1].Name)
	assert.Equal(t, "http://localhost:3000", tabs[1].URL, "the target URL survives archive → restore")

	// The restored record on disk agrees with the live instance.
	rec := recordFor(t, repoID, "worker")
	require.NotNil(t, rec)
	restored := webTabsIn(rec.Tabs)
	require.Len(t, restored, 1, "the restored record retains the web tab")
	assert.Equal(t, "http://localhost:3000", restored[0].URL)
}

// TestArchiveSession_MultipleWebTabsKeepOrder: several web tabs all survive and
// keep their relative order across archive → restore. Tab addressing (panes, the
// 1-9 number keys) is position-sensitive today, so a restore must not shuffle them.
func TestArchiveSession_MultipleWebTabsKeepOrder(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddWebTabForTest("first", "http://localhost:3001")
	inst.AddTabForTest("watcher", session.TabKindProcess)
	inst.AddWebTabForTest("second", "http://localhost:3002")

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	_, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	tabs := inst.GetTabs()
	require.Len(t, tabs, 3, "agent + both web tabs; the interleaved process tab drops")
	assert.Equal(t, "first", tabs[1].Name, "web tabs keep their original relative order")
	assert.Equal(t, "http://localhost:3001", tabs[1].URL)
	assert.Equal(t, "second", tabs[2].Name)
	assert.Equal(t, "http://localhost:3002", tabs[2].URL)
}

// TestArchiveSession_ReuseArchivedNameKeepsWebTabs guards the interaction between
// #1809 and the reuse-archived-name rename path: creating a session whose title
// collides with an archived one renames the archived session out of the way
// (relocating its worktree to a new per-repo archive dir). That rename must carry
// the web tabs — it re-persists the record from the live instance, so a regression
// there would silently erase the URLs the archive just preserved.
func TestArchiveSession_ReuseArchivedNameKeepsWebTabs(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddWebTabForTest("webpreview", "http://localhost:3000")

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	disk, derr := loadRepoInstanceData(repoID)
	require.NoError(t, derr)
	manager.mu.Lock()
	renamed, rerr := manager.renameArchivedForReuseLocked(repoID, repoPath, "worker", "claude", runtimeNamespaceLocalTmux, &disk)
	manager.mu.Unlock()
	require.NoError(t, rerr)
	require.NotNil(t, renamed, "the archived collision must be renamed out of the way")
	require.NotEqual(t, "worker", renamed.Title, "the archived session takes a disambiguated title")

	web := webTabsIn(renamed.Tabs)
	require.Len(t, web, 1, "the renamed archived record keeps its web tab")
	assert.Equal(t, "http://localhost:3000", web[0].URL, "the URL survives the reuse-name rename")

	// And it is durable under the new title, not just in memory.
	rec := recordFor(t, repoID, renamed.Title)
	require.NotNil(t, rec)
	require.Len(t, webTabsIn(rec.Tabs), 1, "the persisted renamed record keeps its web tab")
	assert.Equal(t, "http://localhost:3000", webTabsIn(rec.Tabs)[0].URL)
}
