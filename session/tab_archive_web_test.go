package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArchiveTeardown_KeepsWebTabsDropsProcessTabs is the #1809 headline: a web
// tab is pure metadata (a URL string, no tmux session, no process), so archive —
// the documented RESTORABLE reap path — must carry it into the archived record.
// Process/shell tabs still drop: their tmux sessions are torn down by the archive
// teardown, so only the agent returns for them (#1028).
func TestArchiveTeardown_KeepsWebTabsDropsProcessTabs(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_archive_web")
	_, err := inst.AddWebTab("http://localhost:3000", "webpreview")
	require.NoError(t, err)
	_, err = inst.AddProcessTab("sleep 300", "watcher")
	require.NoError(t, err)
	require.Equal(t, 3, inst.TabCount(), "agent + web + process")

	// The mock worktree has no real git repo behind it, so the relocation errors;
	// finalize (the tab reconciliation under test) runs regardless.
	_ = inst.ArchiveTeardown(t.TempDir())

	tabs := inst.GetTabs()
	require.Len(t, tabs, 2, "the agent tab and the web tab survive archive; the process tab drops")
	assert.Equal(t, TabKindAgent, tabs[0].Kind)
	assert.Equal(t, agentTabName, tabs[0].Name)
	assert.Equal(t, TabKindWeb, tabs[1].Kind, "the web tab must survive archive (#1809)")
	assert.Equal(t, "webpreview", tabs[1].Name, "the web tab keeps its name")
	assert.Equal(t, "http://localhost:3000", tabs[1].URL, "the web tab keeps its target URL")
	assert.Nil(t, tabs[1].tmux, "a web tab never carries a tmux session")
}

// TestArchiveTeardown_WebTabsSurvivePersistRoundTrip closes the #1809 loop end to
// end: the tabs kept by archive must serialize into the archived record and come
// back — with the URL intact — when the record is restored from disk. This is the
// property the bug broke: the roster was destroyed at archive time, so restore had
// nothing to rebuild from.
func TestArchiveTeardown_WebTabsSurvivePersistRoundTrip(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_archive_web_persist")
	_, err := inst.AddWebTab("http://localhost:4321", "livepreview")
	require.NoError(t, err)

	_ = inst.ArchiveTeardown(t.TempDir())

	// Serialize the archived record exactly as the daemon persists it, then rebuild
	// it the way a restore does.
	data := inst.ToInstanceData()
	restored := &Instance{}
	restoreLocalTabs(restored, data)

	require.Len(t, restored.Tabs, 2, "the archived record round-trips agent + web")
	rw := restored.Tabs[1]
	assert.Equal(t, TabKindWeb, rw.Kind)
	assert.Equal(t, "livepreview", rw.Name)
	assert.Equal(t, "http://localhost:4321", rw.URL, "the target URL survives archive → persist → restore")
	assert.Nil(t, rw.tmux, "a restored web tab has no tmux session")
}

// TestArchiveTeardown_PreservesWebTabRelativeOrder: kept tabs retain their original
// relative order rather than being re-appended in an arbitrary one. Tab addressing
// (panes, number keys) is position-sensitive today, so a restored session must not
// shuffle its web tabs.
func TestArchiveTeardown_PreservesWebTabRelativeOrder(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_archive_web_order")
	_, err := inst.AddWebTab("http://localhost:3001", "first")
	require.NoError(t, err)
	_, err = inst.AddProcessTab("sleep 300", "watcher")
	require.NoError(t, err)
	_, err = inst.AddWebTab("http://localhost:3002", "second")
	require.NoError(t, err)

	_ = inst.ArchiveTeardown(t.TempDir())

	tabs := inst.GetTabs()
	require.Len(t, tabs, 3, "agent + both web tabs; the interleaved process tab drops")
	assert.Equal(t, agentTabName, tabs[0].Name)
	assert.Equal(t, "first", tabs[1].Name, "web tabs keep their original relative order")
	assert.Equal(t, "second", tabs[2].Name)
}

// TestArchiveTeardown_AgentOnlyUnchanged: the common case — a session with no web
// tabs — still reduces to exactly the agent tab, with its tmux name-holder binding
// kept so a rollback Recover can re-spawn it (#1028).
func TestArchiveTeardown_AgentOnlyUnchanged(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_archive_agent_only")
	_, err := inst.AddProcessTab("sleep 300", "watcher")
	require.NoError(t, err)

	_ = inst.ArchiveTeardown(t.TempDir())

	tabs := inst.GetTabs()
	require.Len(t, tabs, 1, "only the agent tab survives when there are no web tabs")
	assert.Equal(t, TabKindAgent, tabs[0].Kind)
	assert.NotNil(t, tabs[0].tmux, "the agent tab keeps its tmux name-holder for a rollback Recover")
}

// TestWebTabServeBlocked_CoversTeardownNotRestore pins the exact shape of the
// serve-side inert window, both halves of which are deliberate.
//
// It is BROADER than the settled LiveArchived on the teardown side: BeginArchive
// raises OpArchiving before tmux comes down and the worktree moves, and commits
// the archive only at the end, so a settled-only gate would serve a preserved
// loopback URL throughout the teardown.
//
// It is deliberately NARROWER on the restore side: BeginRestore moves the session
// to LiveLost + OpRestoring, but both callers move the worktree home BEFORE that
// transition, so a restoring session's worktree is already in place and the tab it
// serves is the one it will serve a moment later anyway. Blocking there would only
// blank a pane that is about to work.
func TestWebTabServeBlocked_CoversTeardownNotRestore(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	for _, tc := range []struct {
		name    string
		arrange func(i *Instance)
		blocked bool
		reason  string
	}{
		{"live", func(*Instance) {}, false, "a live session serves its web tab"},
		{"mid-archive", func(i *Instance) {
			require.NoError(t, i.Transition(BeginArchive()))
		}, true, "archive must be inert from the moment it starts, not only once it commits"},
		{"archived", func(i *Instance) {
			require.NoError(t, i.Transition(BeginArchive()))
			require.NoError(t, i.Transition(CommitArchive()))
		}, true, "a settled archive is inert"},
		{"restoring", func(i *Instance) {
			require.NoError(t, i.Transition(BeginArchive()))
			require.NoError(t, i.Transition(CommitArchive()))
			require.NoError(t, i.Transition(BeginRestore()))
		}, false, "the worktree is home before OpRestoring is raised, so serving is safe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := startedMockInstance(t, "af_serve_gate_"+tc.name)
			_, err := inst.AddWebTab("http://localhost:3000", "webpreview")
			require.NoError(t, err)
			tc.arrange(inst)

			err = inst.WebTabServeBlocked()
			if tc.blocked {
				require.Error(t, err, tc.reason)
				assert.Contains(t, err.Error(), "archived", "the refusal must name archive so the proxy message stays actionable")
			} else {
				require.NoError(t, err, tc.reason)
			}
		})
	}
}
