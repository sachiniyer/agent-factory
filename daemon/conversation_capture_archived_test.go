package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// captureArchiveTestInstance stands up a tracked, running codex session whose
// conversation id CaptureAgentConversation will resolve from a rollout file, and
// persists its initial (id-less) record. It mirrors the setup in
// TestCaptureAgentConversationPersistsCodexRolloutID so the only difference these
// tests exercise is the archived gate.
func captureArchiveTestInstance(t *testing.T) (*Manager, *session.Instance, string, string, session.ConversationCaptureSnapshot) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "codex-worker",
		Path:    repoPath,
		Program: tmux.ProgramCodex,
	})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSession("codex-worker", tmux.ProgramCodex))
	inst.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repo.ID, inst.Title)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()
	require.NoError(t, appendInstanceData(repo.ID, inst.ToInstanceData()))

	snap := session.BeginConversationCapture()
	writeDaemonCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	return manager, inst, repo.ID, key, snap
}

func persistedConversationID(t *testing.T, repoID string) string {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	var stored []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Len(t, stored, 1)
	if stored[0].AgentConversation == nil {
		return ""
	}
	return stored[0].AgentConversation.ID
}

// TestCaptureAgentConversation_RejectsAlreadyArchivedSession is the #2451
// front-door half: a session already archived when the async capture lands must
// not have a conversation id (nor anything else ToInstanceData snapshots) written
// over the record the archive committed. Archive is inert in BOTH directions
// (#1809). Mirrors TestSetPRInfo_RejectsAlreadyArchivedSession.
func TestCaptureAgentConversation_RejectsAlreadyArchivedSession(t *testing.T) {
	manager, inst, repoID, key, snap := captureArchiveTestInstance(t)

	inst.SetStatusForTest(session.Archived)

	manager.captureAgentConversation(repoID, key, inst, snap, inst.AgentRuntimeToken(), time.Second)

	if id := persistedConversationID(t, repoID); id != "" {
		t.Fatalf("archived session gained a persisted conversation id %q; capture must refuse an archived record", id)
	}
}

// TestCaptureAgentConversation_ArchiveWinningOpLockRaceKeepsCleanRecord is the
// #2451 race half. captureAgentConversation resolved a LIVE session and did its
// subprocess capture, then must serialize its mutate+persist behind this
// session's op-lock — the lock ArchiveSession holds while it commits LiveArchived
// and persists. With no op-lock (the pre-fix behavior) the capture would not park
// at all: it would interleave INSIDE the archive and write a conversation id over
// the record the archive just committed. Mirrors
// TestSetPRInfo_ArchiveWinningOpLockRaceKeepsPRInfo.
func TestCaptureAgentConversation_ArchiveWinningOpLockRaceKeepsCleanRecord(t *testing.T) {
	manager, inst, repoID, key, snap := captureArchiveTestInstance(t)

	opLock := manager.opLockFor(key)
	opLock.Lock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.captureAgentConversation(repoID, key, inst, snap, inst.AgentRuntimeToken(), time.Second)
	}()

	// The capture must PARK on the op-lock: it resolved a live session and finished
	// its subprocess capture, so the only interleaving that reaches the post-lock
	// archived gate is one that queues behind the operation holding the lock.
	// Without the op-lock it returns immediately and the race is never exercised.
	select {
	case <-done:
		opLock.Unlock()
		t.Fatal("captureAgentConversation returned while the op-lock was held; it never took the op-lock and so cannot serialize against an archive")
	case <-time.After(150 * time.Millisecond):
	}

	// The archive commits LiveArchived under the op-lock and leaves the same
	// pointer tracked, exactly as ArchiveSession does before releasing the lock.
	inst.SetStatusForTest(session.Archived)
	opLock.Unlock()

	<-done

	if id := persistedConversationID(t, repoID); id != "" {
		t.Fatalf("capture that queued behind an archive wrote conversation id %q over the archived record", id)
	}
}
