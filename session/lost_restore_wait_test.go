package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
)

// The environmental wait (#5295) is the non-terminal counterpart of
// LostRestoreFailure: it must persist, project the same kind of idle reason,
// sanitize the durable error, and retire on the same edges.
func TestLostRestoreWaitPersistsAndProjectsWaitingReason(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	inst, err := NewInstance(InstanceOptions{
		Title: "waits-on-disk", Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
	require.True(t, inst.SetLostRestoreWait(3, 2, 5, errors.New("recover: no space left on device")))

	stored := inst.ToInstanceData().ForStorage()
	stored.Worktree = GitWorktreeData{
		RepoPath: stored.Path, WorktreePath: stored.Path, SessionName: stored.Title,
		ExternalWorktree: true,
	}
	require.NotNil(t, stored.LostRestoreWait)
	require.Equal(t, 3, stored.LostRestoreWait.Attempts)
	require.Equal(t, 2, stored.LostRestoreWait.DeadlineWaits)
	require.Equal(t, 5, stored.LostRestoreWait.TotalAttempts)
	require.Equal(t, "recover: no space left on device", stored.LostRestoreWait.Error)
	require.Equal(t, IdleReasonRestoreWaitingOnEnvironment, stored.ProjectIdleReason().IdleReason)

	payload, err := json.Marshal(stored)
	require.NoError(t, err)
	var decoded InstanceData
	require.NoError(t, json.Unmarshal(payload, &decoded))
	restored, err := FromInstanceData(decoded)
	require.NoError(t, err)
	restoredData := restored.ToInstanceData()
	require.Equal(t, LiveLost, restoredData.Liveness)
	require.Equal(t, IdleReasonRestoreWaitingOnEnvironment, restoredData.IdleReason)
	require.Equal(t, stored.LostRestoreWait, restoredData.LostRestoreWait)
	require.True(t, restored.LifecycleView().RestoreWaitingOnEnvironment)
	require.False(t, restored.LifecycleView().LostRestoreGaveUp)
}

func TestLostRestoreWaitDetailIsStructuredAndClearable(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "waiting", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.True(t, inst.SetLostRestoreWait(1, 0, 1, errors.New("disk full")))

	wait := inst.LostRestoreWaitSnapshot()
	require.NotNil(t, wait)
	require.Equal(t, "restore is waiting on the environment (1 environmental failure): disk full", wait.Detail())
	require.True(t, inst.ClearLostRestoreWait())
	require.Nil(t, inst.LostRestoreWaitSnapshot())
	// Clearing an already-clear wait is a no-op, not a mutation.
	require.False(t, inst.ClearLostRestoreWait())
}

func TestLostRestoreWaitSanitizesDurableError(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "waiting", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	raw := "spawn failed:\n\x1b[31mno space left on device\x1b[0m\r\n\t" + strings.Repeat("x", 2048)
	require.True(t, inst.SetLostRestoreWait(2, 0, 2, errors.New(raw)))

	wait := inst.LostRestoreWaitSnapshot()
	require.NotNil(t, wait)
	require.Contains(t, wait.Error, "spawn failed:")
	require.Contains(t, wait.Error, "no space left on device")
	require.Equal(t, -1, strings.IndexFunc(wait.Error, unicode.IsControl))
	require.LessOrEqual(t, len([]rune(wait.Error)), 512)
	require.True(t, strings.HasSuffix(wait.Error, "…"), "truncated error = %q", wait.Error)
}

// TestLostRestoreAttemptsPersistsAcrossStorage pins the #5295 restart
// property: the durable accounting record survives a cleared wait (the
// structural-charge window the record exists for), round-trips through
// instances.json, and clears on the zero tuple.
func TestLostRestoreAttemptsPersistsAcrossStorage(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "attempts", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))

	require.True(t, inst.SetLostRestoreAttempts(LostRestoreAttempts{Structural: 2, Preserve: 1, Total: 5}))
	stored := inst.ToInstanceData().ForStorage()
	stored.Worktree = GitWorktreeData{
		RepoPath: stored.Path, WorktreePath: stored.Path, SessionName: stored.Title,
		ExternalWorktree: true,
	}
	require.NotNil(t, stored.LostRestoreAttempts)
	require.Equal(t, LostRestoreAttempts{Structural: 2, Preserve: 1, Total: 5}, *stored.LostRestoreAttempts)

	payload, err := json.Marshal(stored)
	require.NoError(t, err)
	var decoded InstanceData
	require.NoError(t, json.Unmarshal(payload, &decoded))
	restored, err := FromInstanceData(decoded)
	require.NoError(t, err)
	snap := restored.LostRestoreAttemptsSnapshot()
	require.NotNil(t, snap)
	require.Equal(t, LostRestoreAttempts{Structural: 2, Preserve: 1, Total: 5}, *snap)

	// The zero tuple retires the record and reports a change once.
	require.True(t, restored.ClearLostRestoreAttempts())
	require.Nil(t, restored.LostRestoreAttemptsSnapshot())
	require.False(t, restored.ClearLostRestoreAttempts())
}

// TestLostRestoreAttemptsAbsentRecordStaysAbsent — a session that never saw a
// restore episode stores no record and reports no snapshot (the omitempty
// contract the restart seeder relies on).
func TestLostRestoreAttemptsAbsentRecordStaysAbsent(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "fresh", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.Nil(t, inst.LostRestoreAttemptsSnapshot())
	require.Nil(t, inst.ToInstanceData().ForStorage().LostRestoreAttempts)
	require.False(t, inst.SetLostRestoreAttempts(LostRestoreAttempts{}), "clearing an absent record is a no-op")
}

// The give-up reason is terminal and the wait is not: a record carrying both —
// possible only across a sanitize/mirror boundary — must read as gave-up, the
// same precedence IdleReasonFor applies.
func TestLostRestoreWaitYieldsToTerminalFailure(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "race", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
	require.True(t, inst.SetLostRestoreWait(2, 0, 2, errors.New("enospc")))
	require.True(t, inst.SetLostRestoreFailure(6, errors.New("agent exited")))

	reason, _, _, _ := inst.IdleReasonDetailSnapshot()
	require.Equal(t, IdleReasonRestoreGaveUp, reason)
	// The daemon clears the wait when the failure turns structural; simulating
	// that edge leaves only the terminal record.
	require.True(t, inst.ClearLostRestoreWait())
	require.Nil(t, inst.LostRestoreWaitSnapshot())
	require.NotNil(t, inst.LostRestoreFailureSnapshot())
}
