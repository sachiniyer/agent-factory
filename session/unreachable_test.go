package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The unreachable flag (#5571 W1.3) is the additive observation half of the
// remote-loss debounce: the probes stopped being answered, so the row says so
// explicitly — under the same locking, sanitization, and projection-only
// discipline LostRestoreFailure established, but as live state that ForStorage
// must scrub.
func TestUnreachableSetAnchorsSinceAndRefreshesError(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "outage", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	inst.SetUnreachable(errors.New("dial tcp: connection refused"))
	first := inst.UnreachableSnapshot()
	require.NotNil(t, first)
	require.False(t, first.Since.IsZero())
	require.Equal(t, "dial tcp: connection refused", first.Error)

	// A later failure refreshes the error but never moves the anchor: the
	// projected age measures the whole outage, not the last tick of it.
	time.Sleep(time.Millisecond)
	inst.SetUnreachable(errors.New("i/o timeout"))
	second := inst.UnreachableSnapshot()
	require.NotNil(t, second)
	require.Equal(t, first.Since, second.Since)
	require.Equal(t, "i/o timeout", second.Error)

	require.True(t, inst.ClearUnreachable())
	require.Nil(t, inst.UnreachableSnapshot())
	// Clearing an already-clear flag is a no-op, not a mutation.
	require.False(t, inst.ClearUnreachable())
}

func TestUnreachableErrorIsSanitizedAndTokenRedacted(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "redact", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	// A transport error can embed a URL carrying an access_token query param —
	// the exact shape a remote REST failure produces. The stored text crosses
	// every client boundary af owns, so the token never survives SetUnreachable.
	inst.SetUnreachable(errors.New("Get \"http://sandbox.local:9899/v1/agent/snapshot?access_token=secrettoken123\": EOF\n\textra whitespace"))
	snap := inst.UnreachableSnapshot()
	require.NotNil(t, snap)
	require.NotContains(t, snap.Error, "secrettoken123")
	require.Contains(t, snap.Error, "access_token=")
	require.NotContains(t, snap.Error, "\n")

	// The stored text is bounded: a pathological server message cannot bloat a
	// snapshot row.
	huge := errors.New("boom: " + strings.Repeat("x", 10_000))
	inst.SetUnreachable(huge)
	require.NotNil(t, inst.UnreachableSnapshot())
	require.LessOrEqual(t, len(inst.UnreachableSnapshot().Error), 600)
}

func TestUnreachableLifecycleActionSuppressedButKillStays(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "flagged", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.ID = "unreachable-lifecycle"
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.Equal(t, LifecycleActionArchive, inst.LifecycleAction())
	require.True(t, inst.CanKill())

	inst.SetUnreachable(errors.New("transport down"))
	// Destructive verbs hide for the outage: Archive would try to push a
	// worktree af cannot reach and Restore's recheck would refuse — both would
	// fail on press exactly like an op-fence verb (#5571 W1.3). Kill stays:
	// unreachable is the deliberate escape hatch for a wedged remote.
	require.Equal(t, LifecycleActionNone, inst.LifecycleAction())
	require.True(t, inst.CanKill())

	// A Lost row is no different: the flag rides liveness it does not own.
	require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
	require.Equal(t, LifecycleActionNone, inst.LifecycleAction())
	require.True(t, inst.CanKill())

	require.True(t, inst.ClearUnreachable())
	require.Equal(t, LifecycleActionRestore, inst.LifecycleAction())
}

func TestUnreachableClearAtObservationIsGenerationGated(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "gated", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetUnreachable(errors.New("down"))

	gen := inst.agentObservationTarget().runtime.generation
	require.True(t, inst.AgentObservationCurrent(gen))

	// A stale predecessor generation cannot retire the successor's flag.
	require.False(t, inst.ClearUnreachableAtObservation(AgentObservationGeneration{value: gen.value + 50}))
	require.NotNil(t, inst.UnreachableSnapshot())
	// The zero generation never matches anything.
	require.False(t, inst.ClearUnreachableAtObservation(AgentObservationGeneration{}))
	require.NotNil(t, inst.UnreachableSnapshot())

	require.True(t, inst.ClearUnreachableAtObservation(gen))
	require.Nil(t, inst.UnreachableSnapshot())
}

func TestUnreachableProjectsAndRoundTrips(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	inst, err := NewInstance(InstanceOptions{Title: "rides", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	// A live-started row makes FromInstanceData attempt a real pane restore
	// (tmux respawn) — environment-dependent. The flagged-Lost row is the
	// headline unreachable shape anyway, and its restore path is inert here.
	require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
	inst.SetUnreachable(errors.New("tunnel down"))

	data := inst.ToInstanceData()
	require.NotNil(t, data.Unreachable)
	require.Equal(t, "tunnel down", data.Unreachable.Error)
	require.Equal(t, LifecycleActionNone, data.LifecycleAction)
	data.Worktree = GitWorktreeData{
		RepoPath: data.Path, WorktreePath: data.Path, SessionName: data.Title,
		ExternalWorktree: true,
	}

	payload, err := json.Marshal(data)
	require.NoError(t, err)
	var decoded InstanceData
	require.NoError(t, json.Unmarshal(payload, &decoded))
	restored, err := FromInstanceData(decoded)
	require.NoError(t, err)
	// The TUI rebuilds its rows through FromInstanceData; the flag, and the
	// LifecycleAction it implies, must survive that hop or TUI and web would
	// disagree about the row.
	require.NotNil(t, restored.UnreachableSnapshot())
	require.Equal(t, data.Unreachable.Error, restored.UnreachableSnapshot().Error)
	require.Equal(t, LifecycleActionNone, restored.LifecycleAction())

	// The flag is live observation: ForStorage scrubs it so a restarted daemon
	// re-derives the claim from its own probes rather than inheriting an outage
	// it never observed.
	stored := data.ForStorage()
	require.Nil(t, stored.Unreachable)
	diskPayload, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NotContains(t, string(diskPayload), "unreachable")
}

func TestUnreachableReconcileMirrorsDaemonTruth(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "sync", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.True(t, inst.ReconcileUnreachable(&UnreachableData{Since: time.Now(), Error: "down"}))
	require.NotNil(t, inst.UnreachableSnapshot())
	// Idempotent: the same flag reconciles to a no-op.
	require.False(t, inst.ReconcileUnreachable(inst.UnreachableSnapshot()))
	// The daemon clearing the flag clears the TUI-side row too.
	require.True(t, inst.ReconcileUnreachable(nil))
	require.Nil(t, inst.UnreachableSnapshot())
	// A half-formed projection is not a flag.
	require.False(t, inst.ReconcileUnreachable(&UnreachableData{Error: "no since"}))
	require.Nil(t, inst.UnreachableSnapshot())
}
