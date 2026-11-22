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

// #7754 property 4 + #7755 R5: a row a requested archive left fenced says what
// actually happened — "archive did not finish", whether its teardown refused or
// a committed archive rolled back — not a restore give-up that never ran and
// not a refusal the rollback rows never saw. The SAME durable field keeps
// gating the restore loop. Rows without a Kind (pre-existing or written by an
// older daemon) and rows with an unknown one keep the give-up wording and
// still fence.
func TestLostRestoreFailureArchiveDidNotFinishDetailAndBackwardCompat(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "lost", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.True(t, inst.SetLostRestoreFailureKind(1,
		errors.New("archive did not finish — retry the archive, or restore the session to resume it: pane processes 4242"),
		LostRestoreFailureArchiveDidNotFinish))

	failure := inst.LostRestoreFailureSnapshot()
	require.NotNil(t, failure)
	require.Equal(t, LostRestoreFailureArchiveDidNotFinish, failure.Kind)
	require.Equal(t,
		"archive did not finish — retry the archive, or restore the session to resume it: pane processes 4242",
		failure.Detail())
	require.True(t, inst.LifecycleView().LostRestoreGaveUp,
		"the kind is a label on the same fence — the restore-loop gate is unchanged")

	// A row written before Kind existed unmarshals with "" and still reads as
	// (and gates as) the restore loop's own give-up — the downgrade property.
	var decoded LostRestoreFailure
	require.NoError(t, json.Unmarshal([]byte(`{"attempts":3,"error":"old boom"}`), &decoded))
	require.Equal(t, "", decoded.Kind)
	require.Equal(t, "restore gave up after 3 attempts: old boom", decoded.Detail())

	// A kind this daemon does not know is still a terminal fence — it just has
	// no special wording yet, so the honest generic detail is what renders.
	unknown := LostRestoreFailure{Attempts: 2, Error: "later boom", Kind: "kind-from-the-future"}
	require.True(t, unknown.valid())
	require.Equal(t, "restore gave up after 2 attempts: later boom", unknown.Detail())
}
