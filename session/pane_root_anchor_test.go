package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7435 R11 — when an answered reattach retires the instance's recorded
// launch claim, the pane-root anchor seeded from that claim retires with
// it. Without this the TmuxSession keeps a disproven (pid, StartID) pair and
// the vanished-session sweep's tier 2 anchors on a root tmux just proved is
// not the session's — lowering the admission floor for the wrong processes.
func TestSettleReattachVerificationRetiresThePaneRootAnchor(t *testing.T) {
	testguard.IsolateTmux(t)
	ts := tmux.NewTmuxSessionFromSanitizedName("af_retire_anchor_test", "sleep 300")
	ts.SeedPaneRootEvidence(4242, 99)

	inst := &Instance{Title: "anchor", Path: t.TempDir(), Program: "sleep 300"}
	inst.runtimeProgram = "sleep 300"
	inst.runtimePID, inst.runtimeStartID = 4242, 99

	// The session name does not exist on the isolated server: tmux answers
	// that no pane roots at the recorded pair, so the claim retires.
	verified, claimRetired := inst.settleReattachVerification(ts)
	require.False(t, verified)
	require.True(t, claimRetired,
		"a session tmux cannot place the recorded pair in retires the claim")
	_, _, ok := ts.PaneRootEvidence()
	require.False(t, ok,
		"the anchor seeded from the claim retires with it — a disproven root cannot floor tier 2")

	// An anchor stamped by a NEWER live resolution is a different claim and
	// survives the old one's retirement.
	ts.SeedPaneRootEvidence(7777, 55)
	inst.runtimeProgram = "sleep 300"
	inst.runtimePID, inst.runtimeStartID = 4242, 99
	_, claimRetired = inst.settleReattachVerification(ts)
	require.True(t, claimRetired)
	pid, startID, ok := ts.PaneRootEvidence()
	require.True(t, ok,
		"evidence from a different claim is untouched by this one's retirement")
	require.Equal(t, 7777, pid)
	require.Equal(t, uint64(55), startID)
}
