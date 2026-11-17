package session

import (
	"os/exec"
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

// #7435 R17 — the anchor retires only on an ANSWERED mismatch. A query tmux
// could not answer proves nothing about the recorded pair, so the claim's
// pair stays as capture evidence AND the anchor it seeded stays too: a
// restore that merely timed out must not lose its tier-2 root.
//
// The fixture is a LIVE server holding ZERO sessions: `list-panes` on it
// answers "no current target" — exit 1 naming neither the session nor a
// socket, which missingTmuxSession deliberately does NOT classify as an
// answered absence (#3469's documented shape), so the pane query comes back
// an unanswerable error. The server must be pinned with `set -s exit-empty
// off` BEFORE its last session dies — spelled exactly the way
// testguard.KeepTmuxServerOnEmpty does it. Anything else is an answered
// absence: a server that exited with its session answers "no server
// running on" (ECONNREFUSED) or the ENOENT connect line, both classified,
// and the anchor correctly retires — which is what this test's earlier
// `tmux -f "exit-empty off"` fixture silently produced, because -f names a
// config FILE, not a server option.
func TestSettleReattachVerificationKeepsTheAnchorOnAnUnansweredQuery(t *testing.T) {
	testguard.IsolateTmux(t)
	out, err := exec.Command("tmux", "new-session", "-d", "-s", "holder", "sleep", "300").CombinedOutput()
	require.NoError(t, err, "start the holder session: %s", out)
	testguard.KeepTmuxServerOnEmpty(t)
	require.NoError(t, exec.Command("tmux", "kill-session", "-t", "holder").Run(),
		"leaving a live server with zero sessions")

	ts := tmux.NewTmuxSessionFromSanitizedName("af_unanswered_anchor", "sleep 300")
	ts.SeedPaneRootEvidence(4242, 99)

	inst := &Instance{Title: "anchor", Path: t.TempDir(), Program: "sleep 300"}
	inst.runtimeProgram = "sleep 300"
	inst.runtimePID, inst.runtimeStartID = 4242, 99

	verified, claimRetired := inst.settleReattachVerification(ts)
	require.False(t, verified)
	require.True(t, claimRetired,
		"the runtime claim retires either way — only the anchor's fate differs")
	pid, startID, ok := ts.PaneRootEvidence()
	require.True(t, ok,
		"an unanswered query disproves nothing — the anchor stays as capture evidence")
	require.Equal(t, 4242, pid)
	require.Equal(t, uint64(99), startID)
	require.Equal(t, 4242, inst.captureEvidence.pid,
		"the kept capture evidence is the same pair the anchor holds")
}
