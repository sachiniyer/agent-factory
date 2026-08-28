package session

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
)

func TestLostRestoreFailurePersistsAndProjectsTerminalReason(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	inst, err := NewInstance(InstanceOptions{
		Title: "always-exits", Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
	require.True(t, inst.SetLostRestoreFailure(6, errors.New("agent exited at startup")))

	stored := inst.ToInstanceData().ForStorage()
	stored.Worktree = GitWorktreeData{
		RepoPath: stored.Path, WorktreePath: stored.Path, SessionName: stored.Title,
		ExternalWorktree: true,
	}
	require.NotNil(t, stored.LostRestoreFailure)
	require.Equal(t, 6, stored.LostRestoreFailure.Attempts)
	require.Equal(t, "agent exited at startup", stored.LostRestoreFailure.Error)
	require.Equal(t, IdleReasonRestoreGaveUp, stored.ProjectIdleReason().IdleReason)

	payload, err := json.Marshal(stored)
	require.NoError(t, err)
	var decoded InstanceData
	require.NoError(t, json.Unmarshal(payload, &decoded))
	restored, err := FromInstanceData(decoded)
	require.NoError(t, err)
	restoredData := restored.ToInstanceData()
	require.Equal(t, LiveLost, restoredData.Liveness)
	require.Equal(t, IdleReasonRestoreGaveUp, restoredData.IdleReason)
	require.Equal(t, stored.LostRestoreFailure, restoredData.LostRestoreFailure)
	require.True(t, restored.LifecycleView().LostRestoreGaveUp)
}

func TestLostRestoreFailureDetailIsStructuredAndClearable(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "lost", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.True(t, inst.SetLostRestoreFailure(1, errors.New("boom")))

	failure := inst.LostRestoreFailureSnapshot()
	require.NotNil(t, failure)
	require.Equal(t, "restore gave up after 1 attempt: boom", failure.Detail())
	require.True(t, inst.ClearLostRestoreFailure())
	require.Nil(t, inst.LostRestoreFailureSnapshot())
}

func TestLostRestoreFailureSanitizesDurableError(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "lost", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	raw := "container log:\n\x1b[31mstartup failed\x1b[0m\r\n\t" + strings.Repeat("x", 2048)
	require.True(t, inst.SetLostRestoreFailure(6, errors.New(raw)))

	failure := inst.LostRestoreFailureSnapshot()
	require.NotNil(t, failure)
	require.Contains(t, failure.Error, "container log:")
	require.Contains(t, failure.Error, "startup failed")
	require.Equal(t, -1, strings.IndexFunc(failure.Error, unicode.IsControl))
	require.LessOrEqual(t, len([]rune(failure.Error)), 512)
	require.True(t, strings.HasSuffix(failure.Error, "…"), "truncated error = %q", failure.Error)
}
