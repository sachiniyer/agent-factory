package app

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// newStartedInstance builds a local projection that Started reports as true.
// Several TUI tests share it; it intentionally has no worktree so PR refresh
// tests prove the TUI sends identity without re-deriving daemon eligibility.
func newStartedInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: title, Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	return inst
}

func TestRefreshPRInfoCmdNilInstanceReturnsNil(t *testing.T) {
	assert.Nil(t, refreshPRInfoCmd(nil, "repo-a", false))
	assert.Nil(t, refreshPRInfoCmd(nil, "repo-a", true))
}

// TestRefreshPRInfoCmdSendsIdentityOnly pins #3296's client boundary. A TUI
// selection sends a poke even though this local projection has no worktree
// object to inspect; the daemon owns eligibility, gh lookup, and the PR fields.
func TestRefreshPRInfoCmdSendsIdentityOnly(t *testing.T) {
	inst := newStartedInstance(t, "daemon-owned")
	var got daemon.RefreshPRInfoRequest
	restore := SetPRInfoRefresherForTest(func(req daemon.RefreshPRInfoRequest) error {
		got = req
		return nil
	})
	defer restore()

	cmd := refreshPRInfoCmd(inst, "repo-a", true)
	require.NotNil(t, cmd)
	msg, ok := cmd().(prInfoRefreshFinishedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	require.Equal(t, daemon.RefreshPRInfoRequest{
		ID: inst.ID, Title: inst.Title, RepoID: "repo-a",
	}, got)
	require.Nil(t, inst.GetPRInfo(),
		"the poke must not fetch, derive, or paint PR state in the TUI")
}

func TestRefreshPRInfoCmdThrottlesSelectionButTickForcesPoke(t *testing.T) {
	inst := newStartedInstance(t, "selected")
	restore := SetPRInfoRefresherForTest(func(daemon.RefreshPRInfoRequest) error { return nil })
	defer restore()

	require.NotNil(t, refreshPRInfoCmd(inst, "repo-a", false), "first selection must poke")
	require.Less(t, inst.PRInfoAge(), time.Second)
	assert.Nil(t, refreshPRInfoCmd(inst, "repo-a", false), "rapid selection events must be throttled")
	assert.NotNil(t, refreshPRInfoCmd(inst, "repo-a", true), "the minute tick must still poke")
}

// The completion message carries no projection. Even success or failure can
// only log; the next daemon Snapshot is what changes the displayed badge.
func TestPRInfoRefreshFinishedDoesNotMutateProjection(t *testing.T) {
	h := newTestHome(t)
	inst := newStartedInstance(t, "selected")
	h.store.AddInstance(inst)

	_, cmd := h.Update(prInfoRefreshFinishedMsg{
		target: captureSessionActionTarget(inst, h.repoID),
		err:    errors.New("daemon unavailable"),
	})
	require.Nil(t, cmd)
	require.Nil(t, inst.GetPRInfo())
}

func TestSnapshotAppliesDaemonProjectedPRInfo(t *testing.T) {
	h := newTestHome(t)
	inst := newStartedInstance(t, "projected")
	d := inst.ToInstanceData()
	d.PRInfo = session.PRInfoData{
		Number: 42, Title: "from daemon", URL: "https://example.com/pr/42", State: "OPEN", Branch: "feature/x",
	}

	require.True(t, h.updateInstanceFromSnapshot(inst, d))
	got := inst.GetPRInfo()
	require.NotNil(t, got)
	require.Equal(t, 42, got.Number)
	require.Equal(t, "feature/x", got.Branch)
}
