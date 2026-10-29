package session

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

var claudeConv = AgentConversationData{Agent: tmux.ProgramClaude, ID: "0f8d1c2e-1111-4222-8333-444455556666", CaptureKind: ConversationCaptureInjected}

// Launch sends the daemon's recorded conversation and records the one the
// server reports back (#5571 W7.2).
func TestRemoteLaunchSendsAndRecordsTheConversation(t *testing.T) {
	reported := AgentConversationData{Agent: tmux.ProgramCodex, ID: "019a0000-aaaa-bbbb-cccc-ddddeeeeffff", CaptureKind: ConversationCaptureCodexRollout}
	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true}).
		route("/v1/agent/launch", map[string]any{"ok": true, "conversation": reported})
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	inst := &Instance{Title: "sb", Tabs: []*Tab{newRemoteAgentTab()}}
	inst.Tabs[0].Conversation = claudeConv
	s := &remoteAgentServer{rc: srv.client(t, ts.URL), inst: inst}
	inst.remoteClient = s.rc

	require.NoError(t, s.Launch(true))

	srv.mu.Lock()
	body := srv.bodies["/v1/agent/launch"]
	srv.mu.Unlock()
	conv, ok := body["conversation"].(map[string]any)
	require.True(t, ok, "the recorded conversation must ride the launch request: %v", body)
	assert.Equal(t, claudeConv.ID, conv["id"])
	assert.Equal(t, tmux.ProgramClaude, conv["agent"])
	assert.Equal(t, reported, inst.AgentConversation(), "the conversation the server launched is the one recorded")
}

// A server that predates the field reports nothing; that never erases a record.
func TestRemoteLaunchWithoutReportKeepsTheRecord(t *testing.T) {
	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true}).
		route("/v1/agent/launch", map[string]any{"ok": true})
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	inst := &Instance{Title: "sb", Tabs: []*Tab{newRemoteAgentTab()}}
	inst.Tabs[0].Conversation = claudeConv
	s := &remoteAgentServer{rc: srv.client(t, ts.URL), inst: inst}
	inst.remoteClient = s.rc

	require.NoError(t, s.Launch(true))
	assert.Equal(t, claudeConv, inst.AgentConversation())

	// No record and nothing to send: the field is absent from the request.
	fresh := &Instance{Title: "sb2", Tabs: []*Tab{newRemoteAgentTab()}}
	s = &remoteAgentServer{rc: srv.client(t, ts.URL), inst: fresh}
	fresh.remoteClient = s.rc
	require.NoError(t, s.Launch(true))
	srv.mu.Lock()
	_, sent := srv.bodies["/v1/agent/launch"]["conversation"]
	srv.mu.Unlock()
	assert.False(t, sent)
}

// A conversation the server captures after launch (Codex) arrives on a later
// snapshot and is recorded on the daemon's row.
func TestRemoteSnapshotRecordsALateConversation(t *testing.T) {
	late := AgentConversationData{Agent: tmux.ProgramCodex, ID: "019a1111-aaaa-bbbb-cccc-ddddeeeeffff", CaptureKind: ConversationCaptureCodexRollout}
	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true}).
		route("/v1/agent/snapshot", map[string]any{"updated": true, "content": "x", "conversation": late})
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	inst := &Instance{Title: "sb", Tabs: []*Tab{newRemoteAgentTab()}}
	s := &remoteAgentServer{rc: srv.client(t, ts.URL), inst: inst}
	inst.remoteClient = s.rc

	_, err := s.Snapshot()
	require.NoError(t, err)
	assert.Equal(t, late, inst.AgentConversation())
}

// A launch report that arrives before the agent tab exists is applied when the
// backend seeds the tab.
func TestReportedConversationBeforeTabExists(t *testing.T) {
	inst := &Instance{Title: "sb"}
	assert.False(t, inst.RecordReportedConversationForTest(AgentConversationData{}), "an empty report is a no-op")
	inst.RecordReportedConversationForTest(claudeConv)
	assert.Empty(t, inst.Tabs)

	b := &remoteAgentBackend{}
	inst.agentSrv = &fakeLaunchServer{}
	require.NoError(t, b.Launch(inst, true))
	assert.Equal(t, claudeConv, inst.AgentConversation())
}

type fakeLaunchServer struct{ AgentServer }

func (*fakeLaunchServer) Launch(bool) error { return nil }

// Resume is offered only where this host's provider store holds the transcript:
// a fresh sandbox's store does not hold the old one's history, and a resume flag
// for an id the provider cannot find makes the agent exit on launch.
func TestConversationResumableHere(t *testing.T) {
	assert.False(t, ConversationResumableHere("claude", t.TempDir(), AgentConversationData{}), "no id")
	assert.False(t, ConversationResumableHere("aider", t.TempDir(), AgentConversationData{Agent: "aider", ID: "x"}), "an agent af cannot inspect")

	work := t.TempDir()
	cfg := t.TempDir()
	program := "CLAUDE_CONFIG_DIR=" + cfg + " claude"
	assert.False(t, ConversationResumableHere(program, work, claudeConv), "no transcript on this host")

	configDir, launchDir, err := claudeTranscriptLaunchContext(program, work)
	require.NoError(t, err)
	dir := filepath.Join(configDir, "projects", claudeProjectName(launchDir))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, claudeConv.ID+".jsonl"), []byte("{}\n"), 0o600))
	assert.True(t, ConversationResumableHere(program, work, claudeConv), "the transcript is here")
}

// SetResumeConversation feeds the launch planner the existing carry path.
func TestSetResumeConversationPlansAResume(t *testing.T) {
	inst := &Instance{ID: "id-1"}
	inst.SetResumeConversation(claudeConv)
	rewritten, conv := planCreateConversation(inst, "claude")
	assert.Contains(t, rewritten, "--resume "+claudeConv.ID)
	assert.Equal(t, claudeConv, conv)
}

// A conversation reported LATE (a Codex rollout, found after launch) is a
// durable change of its own (#5826): it is flagged for the daemon's poll
// persistence, survives the row's storage round trip, and a restored row sends
// it back on its next launch so the agent resumes exactly that conversation.
func TestLateReportedConversationSurvivesRestartAndResumes(t *testing.T) {
	late := AgentConversationData{Agent: tmux.ProgramCodex, ID: "019a2222-aaaa-bbbb-cccc-ddddeeeeffff", CaptureKind: ConversationCaptureCodexRollout}
	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true}).
		route("/v1/agent/snapshot", map[string]any{"updated": true, "content": "x", "conversation": late}).
		route("/v1/agent/launch", map[string]any{"ok": true})
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	inst := &Instance{Title: "sb", Path: t.TempDir(), Program: "codex", Tabs: []*Tab{newRemoteAgentTab()}}
	s := &remoteAgentServer{rc: srv.client(t, ts.URL), inst: inst}
	inst.remoteClient = s.rc
	assert.False(t, inst.ConversationUnpersisted())

	_, err := s.Snapshot()
	require.NoError(t, err)
	require.True(t, inst.ConversationUnpersisted(), "a reported identity is memory-only until the daemon writes it")

	// The daemon's write: the row's storage projection carries the identity.
	data := inst.ToInstanceData()
	require.NotNil(t, data.AgentConversation)
	assert.Equal(t, late, *data.AgentConversation)
	require.NotEmpty(t, data.Tabs)
	require.NotNil(t, data.Tabs[0].Conversation)
	written := *data.Tabs[0].Conversation

	// A failed write leaves it pending; the successful one clears it, and only for
	// the identity that was actually written.
	inst.MarkConversationPersisted(AgentConversationData{Agent: tmux.ProgramCodex, ID: "something-else"})
	assert.True(t, inst.ConversationUnpersisted(), "a different identity's write must not clear the flag")
	inst.MarkConversationPersisted(written)
	assert.False(t, inst.ConversationUnpersisted())

	// Restart: the stored row is rebuilt and still holds the handle.
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var stored InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	// The row the daemon wrote is a sandbox session: it reloads inert, and its
	// recorded conversation is what a Lost restore sends back.
	stored.ID, stored.Branch, stored.BackendType, stored.Liveness = "id-late", "root/s", "sandbox", LiveRunning
	restored, err := FromInstanceData(stored)
	require.NoError(t, err)
	assert.Equal(t, late, restored.AgentConversation(), "the resume handle survives the restart")
	// And the reloaded row, which has no agent tab until Launch seeds one, writes
	// it back on every checkpoint instead of dropping it.
	rewritten := restored.ToInstanceData()
	require.NotNil(t, rewritten.AgentConversation, "a checkpoint of the reloaded row must keep the handle")
	assert.Equal(t, late.ID, rewritten.AgentConversation.ID)

	// And the restored row resumes with it.
	s2 := &remoteAgentServer{rc: srv.client(t, ts.URL), inst: restored}
	restored.remoteClient = s2.rc
	require.NoError(t, s2.Launch(true))
	srv.mu.Lock()
	body := srv.bodies["/v1/agent/launch"]
	srv.mu.Unlock()
	conv, ok := body["conversation"].(map[string]any)
	require.True(t, ok, "the restored row must send its conversation on launch: %v", body)
	assert.Equal(t, late.ID, conv["id"])
}

// A RETIRED runtime's in-flight snapshot must not overwrite the replacement's
// conversation identity: the report is bound to the runtime that produced it and
// validated under the instance lock (#5862).
func TestRetiredRuntimeSnapshotCannotOverwriteTheReplacementsConversation(t *testing.T) {
	stale := AgentConversationData{Agent: tmux.ProgramCodex, ID: "019aaaaa-aaaa-bbbb-cccc-ddddeeeeffff", CaptureKind: ConversationCaptureCodexRollout}
	fresh := AgentConversationData{Agent: tmux.ProgramCodex, ID: "019bbbbb-aaaa-bbbb-cccc-ddddeeeeffff", CaptureKind: ConversationCaptureCodexRollout}

	oldSrv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true}).
		route("/v1/agent/snapshot", map[string]any{"updated": true, "content": "x", "conversation": stale})
	oldTS := httptest.NewServer(oldSrv.mux)
	defer oldTS.Close()
	newSrv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true}).
		route("/v1/agent/snapshot", map[string]any{"updated": true, "content": "x", "conversation": fresh})
	newTS := httptest.NewServer(newSrv.mux)
	defer newTS.Close()

	inst := &Instance{Title: "sb", Tabs: []*Tab{newRemoteAgentTab()}}
	retired := &remoteAgentServer{rc: oldSrv.client(t, oldTS.URL), inst: inst}
	replacement := &remoteAgentServer{rc: newSrv.client(t, newTS.URL), inst: inst}

	// The old runtime is current: its report is accepted (anti-vacuous: the fence
	// is not simply refusing everything).
	inst.remoteClient = retired.rc
	_, err := retired.Snapshot()
	require.NoError(t, err)
	require.Equal(t, stale, inst.AgentConversation())

	// The runtime is replaced and the replacement records its own identity.
	inst.remoteClient = replacement.rc
	_, err = replacement.Snapshot()
	require.NoError(t, err)
	require.Equal(t, fresh, inst.AgentConversation())

	// The retired runtime's snapshot, in flight across the replacement, lands now.
	_, err = retired.Snapshot()
	require.NoError(t, err)
	assert.Equal(t, fresh, inst.AgentConversation(), "a retired runtime's report must be dropped")
	assert.Equal(t, fresh, inst.reportedConversation)

	// And after teardown (no runtime bound) nothing is recorded at all.
	inst.remoteClient = nil
	_, err = replacement.Snapshot()
	require.NoError(t, err)
	assert.Equal(t, fresh, inst.AgentConversation())
}
