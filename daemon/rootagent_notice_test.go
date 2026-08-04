package daemon

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #2629 suite. #2616 made the three carry outcomes legible in the
// application log and stopped there, deliberately: a root that genuinely could
// not resume still came back Ready, with a rail row identical to one that
// resumed cleanly. You learned it from a log grep, not from `af`.

// TestEnsureRootAgentsMarksARootThatCameBackWithoutItsHistory is the headline
// assertion: the re-created root's RECORD says its context is fresh. The record
// is what every rail renders and what survives a restart, so this is the fact
// that makes the loss discoverable rather than merely logged.
func TestEnsureRootAgentsMarksARootThatCameBackWithoutItsHistory(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.EnsureRootAgents()

	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	require.Equal(t, session.RootRecreateContextNone, first.RootRecreateContext(),
		"a first-ever root is not a re-create and has nothing to report")

	// The #1104 outage class: tmux vanished under a healthy daemon. This root
	// never recorded a conversation, so there is nothing to resume — the case the
	// carry (#2616) cannot rescue, and therefore the one that most needs saying.
	first.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()

	second := findRootInstance(t, manager, repoPath)
	require.NotNil(t, second)
	require.NotSame(t, first, second, "the vanished root must have been reaped and re-created")
	require.Equal(t, session.RootRecreateContextFresh, second.RootRecreateContext())
	require.Equal(t, "fresh context", second.RootRecreateContext().Note())

	// And it is durable, not just in memory: the outage that loses a root is the
	// same event that restarts the daemon.
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	stored := loadRootRecordForTest(t, repo.ID)
	require.Equal(t, session.RootRecreateContextFresh, stored.RootRecreateContext,
		"the notice must be on disk; a restart during the outage must not erase it")
}

// TestEnsureRootAgentsLeavesNoNoticeWhenTheRootResumed: the marker is a
// notice, not a re-create counter. A root that came back on exactly the
// conversation it had lost nothing, and putting a note on that row would train
// users to ignore the one that matters.
func TestEnsureRootAgentsLeavesNoNoticeWhenTheRootResumed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	prior := session.AgentConversationData{}
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		if opts.ResumeConversation.HasID() {
			// Stand in for a launch that really did come up on the carried
			// conversation: the fake backend spawns nothing, so the commit
			// LocalBackend.launch would make has to be made here.
			prior = opts.ResumeConversation
		}
		return resumingFakeBackend{readyFakeBackend{fake}, &prior}, nil
	})
	t.Cleanup(restore)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.EnsureRootAgents()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	seedRootConversation(t, first)

	first.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()

	second := findRootInstance(t, manager, repoPath)
	require.NotNil(t, second)
	require.NotSame(t, first, second)
	require.Equal(t, priorRootConversationID, second.AgentConversation().ID,
		"fixture guard: this test is only meaningful if the replacement really resumed")
	require.Equal(t, session.RootRecreateContextNone, second.RootRecreateContext(),
		"a root that resumed its conversation lost nothing and must carry no notice")
}

// resumingFakeBackend commits the carried conversation onto the new instance's
// agent tab at launch, the way LocalBackend.launch does for a real create. The
// plain fake spawns nothing and records nothing, which would make every heal
// classify as "unknown" and hide the resumed case entirely.
type resumingFakeBackend struct {
	readyFakeBackend
	conversation *session.AgentConversationData
}

// Start, not Launch: embedding gives no virtual dispatch, so FakeBackend.Start
// would call its OWN Launch and never reach this override.
func (b resumingFakeBackend) Start(i *session.Instance, first bool) error {
	if err := b.readyFakeBackend.Start(i, first); err != nil {
		return err
	}
	if b.conversation.HasID() {
		// A conversation lives on the agent TAB, and the fake backend never binds
		// one. Bind a (process-less) tmux session so the tab exists, then commit —
		// the same two steps Provision + the launch plan take for a real create.
		i.SetTmuxSession(tmux.NewTmuxSession(i.Title, tmux.ProgramClaude))
		i.SetAgentConversation(*b.conversation)
	}
	return nil
}

// loadRootRecordForTest reads the persisted root record straight off disk.
func loadRootRecordForTest(t *testing.T, repoID string) session.InstanceData {
	t.Helper()
	all, err := loadRepoInstanceData(repoID)
	require.NoError(t, err)
	for _, d := range all {
		if d.Title == session.RootSessionTitle {
			return d
		}
	}
	t.Fatalf("no persisted %q record for repo %s", session.RootSessionTitle, repoID)
	return session.InstanceData{}
}

// TestOpeningTheSessionPaneClearsTheRecreateNotice drives the real acknowledge
// path: a client dials the PTY stream — the one route a TUI attach, a CLI
// attach, and a web pane render all arrive on — and the notice is gone from the
// instance AND from disk afterwards.
//
// The round trip is the point. A unit call to acknowledgeRootRecreate would
// prove the clearing works while leaving it unreachable from any surface, which
// is the same "nothing tells you" failure #2629 is about.
func TestOpeningTheSessionPaneClearsTheRecreateNotice(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	const title = "root"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_2629_root")
	inst.NoteRecreateContext()
	require.Equal(t, session.RootRecreateContextFresh, inst.RootRecreateContext(),
		"fixture guard: the session must carry a notice for the clear to be observable")
	require.NoError(t, persistInstanceData(repo.ID, inst.ToInstanceData()))

	srv := httptest.NewServer(newHTTPMux(&controlServer{manager: manager}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/sessions/" + title + "/stream?repo_id=" + repo.ID
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err, "the stream must upgrade; the clear happens once the client has the pane")
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	require.Eventually(t, func() bool {
		return inst.RootRecreateContext() == session.RootRecreateContextNone
	}, 5*time.Second, 20*time.Millisecond,
		"opening the session's pane is the acknowledgement; the notice must clear")

	require.Eventually(t, func() bool {
		return loadRootRecordForTest(t, repo.ID).RootRecreateContext == session.RootRecreateContextNone
	}, 5*time.Second, 20*time.Millisecond,
		"the clear must be durable, or the notice returns on the next daemon start")
}

// TestASecondHealCarriesTheUnacknowledgedNotice is the #2814 Codex P2 at the
// daemon seam: the pending notice has to ride the reap into the next create, or
// the floor inside the instance has nothing to floor against.
func TestASecondHealCarriesTheUnacknowledgedNotice(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.EnsureRootAgents()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	require.Empty(t, (*seen)[0].PendingRecreateNotice, "a first-ever root inherits no notice")

	first.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()
	second := findRootInstance(t, manager, repoPath)
	require.NotNil(t, second)
	require.Equal(t, session.RootRecreateContextFresh, second.RootRecreateContext(),
		"fixture guard: the first heal must leave a notice for the second to inherit")

	// tmux dies again before anyone opened the pane.
	second.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()

	require.Len(t, *seen, 3)
	require.Equal(t, session.RootRecreateContextFresh, (*seen)[2].PendingRecreateNotice,
		"the unacknowledged notice must reach the replacement, or a clean second heal erases it")
	third := findRootInstance(t, manager, repoPath)
	require.NotNil(t, third)
	require.Equal(t, session.RootRecreateContextFresh, third.RootRecreateContext())
}

// TestStreamShowsAgentPane is the #2814 Codex P2 that #2628 made reachable: a
// healed root now comes back WITH its shell/process tabs, so a pane can stream
// one of those. Streaming a terminal says nothing about whether the agent kept
// its context, and acknowledging there retires the notice before it has told
// anybody anything.
func TestStreamShowsAgentPane(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{Title: "root", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	// Binding a tmux session materializes the agent tab (with its stable id), and
	// a web tab stands in for any second pane a healed root now brings back.
	inst.SetTmuxSession(tmux.NewTmuxSession("root", tmux.ProgramClaude))
	inst.AddWebTabForTest("web", "http://localhost:5173/")
	tabs := inst.GetTabs()
	require.Len(t, tabs, 2)
	agentID, otherID := tabs[0].ID, tabs[1].ID
	require.NotEmpty(t, agentID)
	require.NotEmpty(t, otherID)

	assert.True(t, streamShowsAgentPane(inst, "", 0), "no id and no ordinal is the agent pane")
	assert.True(t, streamShowsAgentPane(inst, agentID, 0), "the agent tab addressed by its stable id")
	assert.True(t, streamShowsAgentPane(inst, agentID, 1),
		"a supplied id is the authority; a stale ordinal beside it must not decide")
	assert.False(t, streamShowsAgentPane(inst, "", 1), "a second tab by ordinal is not the agent pane")
	assert.False(t, streamShowsAgentPane(inst, otherID, 0), "a second tab by id is not the agent pane")
	assert.False(t, streamShowsAgentPane(inst, "tab-gone", 0),
		"an id that matches no tab is unknown, and unknown must not acknowledge")
	assert.False(t, streamShowsAgentPane(nil, "", 0))
}

// TestAcknowledgeKeepsTheNoticeWhenThePersistFails is the #2814 Codex P2. The
// clear is committed to memory before it is committed to disk, so a failed
// write used to leave THIS daemon's snapshots without the notice while disk
// still carried it — and every later pane open took the "nothing to clear" fast
// path and never retried. The user lost the warning until a restart reloaded
// the stale record. A failed persist must leave the note exactly as it was.
func TestAcknowledgeKeepsTheNoticeWhenThePersistFails(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, "root", "af_2629_persistfail")
	inst.NoteRecreateContext()
	require.Equal(t, session.RootRecreateContextFresh, inst.RootRecreateContext())

	// Make the targeted write fail the way a drifted store does: persistInstanceData
	// refuses when no record with that title is on disk, rather than inventing one.
	require.NoError(t, config.LoadState().SaveInstances(repo.ID, []byte("[]")))
	require.Error(t, persistInstanceData(repo.ID, inst.ToInstanceData()),
		"fixture guard: the write this test needs to fail must actually fail")

	manager.acknowledgeRootRecreate(inst)

	require.Equal(t, session.RootRecreateContextFresh, inst.RootRecreateContext(),
		"a clear that could not be persisted must not be applied in memory either")
}
