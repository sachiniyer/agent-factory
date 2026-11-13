package daemon

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7215: from the moment the incoming pane can write a transcript, the durable
// record names the incoming agent, its conversation and the mission it is owed.
// It never names the outgoing agent for a pane that runs the incoming one. The
// handoff used to persist only after the swap, so a crash between the spawn and
// that checkpoint left the disk naming the outgoing agent: load reattached the
// live incoming pane as the outgoing agent, and Recover later resumed the
// outgoing conversation with the incoming one in no record.

// spawnObservingBackend is handoffBackend with LocalBackend's swap order made
// observable: the post-stop capture, the step immediately before the start,
// then the start itself, at which point atSpawn sees what a crash at that
// instant would leave on disk.
type spawnObservingBackend struct {
	*handoffBackend
	mu      sync.Mutex
	spawned int
	atSpawn func(*session.Instance)
}

func (b *spawnObservingBackend) SwapAgent(i *session.Instance, plan session.AgentSwapPlan) error {
	if err := plan.CaptureAfterStop(); err != nil {
		return err
	}
	if err := plan.RunBeforeStart(); err != nil {
		return err
	}
	b.mu.Lock()
	b.spawned++
	atSpawn := b.atSpawn
	b.mu.Unlock()
	if atSpawn != nil {
		atSpawn(i)
	}
	i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram()))
	return nil
}

func (b *spawnObservingBackend) spawns() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spawned
}

func newSpawnObservingBackend() *spawnObservingBackend {
	return &spawnObservingBackend{handoffBackend: &handoffBackend{FakeBackend: session.NewFakeBackend()}}
}

// The disk at the instant the incoming pane starts is what a crash there
// leaves, and a restart loads exactly that record.
func TestHandoffSession_IncomingRecordIsDurableWhenTheIncomingPaneStarts(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := newSpawnObservingBackend()
	var onDisk session.InstanceData
	backend.atSpawn = func(*session.Instance) { onDisk = persistedInstance(t, repoID) }
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "durable-handoff", backend)
	require.Equal(t, tmux.ProgramClaude, persistedInstance(t, repoID).Program, "precondition: the disk names the outgoing agent")

	_, err := manager.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repoID, To: tmux.ProgramCodex})
	require.NoError(t, err)
	require.Equal(t, 1, backend.spawns())

	require.Equal(t, tmux.ProgramCodex, onDisk.Program,
		"a crash as the incoming pane starts must find the incoming agent on disk, not the outgoing one")
	require.NotEmpty(t, onDisk.PendingHandoffMission, "the record owes the incoming agent its mission")
	require.Equal(t, session.PromptNotDelivered, onDisk.HandoffDeliveryStatus)
	require.Empty(t, onDisk.RuntimeProgram,
		"the outgoing runtime's launch claim describes a process that is gone; the record claims none")
	require.Len(t, onDisk.Tabs[0].Handoffs, 1, "the ledger entry is durable with the swap it records")

	// What a restart loads names nothing of the outgoing agent's as running:
	// with no launch claim, load's reattach keeps the live pane as Program's
	// agent, and the outgoing conversation is not the agent tab's.
	for _, conv := range []*session.AgentConversationData{onDisk.Tabs[0].Conversation, onDisk.AgentConversation} {
		if conv != nil {
			require.NotEqual(t, "conv-outgoing-42", conv.ID,
				"the outgoing conversation must not be resumable as the incoming agent's")
		}
	}
}

// A write that cannot record the incoming agent refuses its start: nothing runs
// that the record does not name, and the outgoing record goes back to disk.
func TestHandoffSession_RefusesTheStartWhenTheIncomingRecordCannotBeWritten(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := newSpawnObservingBackend()
	inst := registerHandoffSubject(t, manager, repoID, repoPath, "undurable-handoff", backend)

	var mu sync.Mutex
	var refused int
	var afterRefusal []session.InstanceData
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		mu.Lock()
		defer mu.Unlock()
		if data.Title != inst.Title {
			return nil
		}
		if data.Program == tmux.ProgramCodex {
			refused++
			return errors.New("disk is full")
		}
		if refused > 0 {
			afterRefusal = append(afterRefusal, data)
		}
		return nil
	}

	_, err := manager.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repoID, To: tmux.ProgramCodex})
	require.Error(t, err, "a handoff whose incoming record cannot be written must not start the incoming agent")
	require.False(t, isMutationCommitted(err))
	require.Zero(t, backend.spawns(), "nothing may start that the record does not name")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, refused, "the incoming record is written before the start")
	require.NotEmpty(t, afterRefusal, "the abort re-persists the reverted record")
	require.Equal(t, tmux.ProgramClaude, afterRefusal[len(afterRefusal)-1].Program)
	require.Empty(t, afterRefusal[len(afterRefusal)-1].PendingHandoffMission)
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(), "the in-memory record is reverted too")
	require.Equal(t, tmux.ProgramClaude, persistedInstance(t, repoID).Program)
}
