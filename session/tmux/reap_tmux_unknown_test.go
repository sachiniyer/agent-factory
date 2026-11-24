package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// #7238's three-valued contract: TmuxUnknown is a third answer, not the
// negative, and "Only NotTmux licenses treating a process as an ordinary one."
// The vanished-session reaper's two boundary walks — classifyCaptureAncestry
// in reap_protected.go and cohortMembership.admits in reap_membership.go —
// reused a `TmuxIdentityOf(...) == Tmux` predicate from #7433's protective
// direction, where crossing an unproven link is the safe choice. In the
// opposite, admission-gating direction the safe answer for TmuxUnknown is the
// REVERSE: an unproven carrier behind an unreadable tmux server must be
// refused and never signalled, and descent proof must stop at the server, so
// membership cannot be minted through it to a proven ancestor above. These
// tests pin that fix at both sites, staging the production race — a boundary
// node the sweep's snapshot captured as tmux (its kernel task name says so)
// whose live process-table read answers TmuxUnknown — with a high pid that
// was never alive, which reads TmuxUnknown with no zombie/subreaper timing.

// startTmuxBoundarySleeper runs a `tmux`-named sleeper the test owns, so a
// live, positively-identified tmux process is available for the readable
// control. Both reads the boundary classifier makes answer `tmux`: argv[0]'s
// basename and the kernel task name. The test process is the parent, so the
// cleanup Wait reaps it — no init/subreaper race.
func startTmuxBoundarySleeper(t *testing.T) (tmuxPID int, tmuxComm string) {
	t.Helper()
	tmuxPath := sleeper.Named(t, filepath.Join(t.TempDir(), "tmux"))
	cmd := exec.Command(tmuxPath, "300")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	tmuxPID = cmd.Process.Pid
	require.Eventually(t, func() bool { return proctree.TmuxIdentityOf(tmuxPID) == proctree.Tmux },
		5*time.Second, 10*time.Millisecond,
		"the tmux-named sleeper must read as Tmux while alive or the fixture means nothing")
	p, err := proctree.Lookup(tmuxPID)
	require.NoError(t, err)
	require.Equal(t, "tmux", p.Comm,
		"the sleeper's kernel task name must be `tmux` — the snapshot half of the boundary")
	return tmuxPID, p.Comm
}

// classifyCaptureAncestry must treat a snapshot-known tmux server whose live
// identity reads TmuxUnknown as a boundary, the same verdict a positive live
// read produces — not cross it and leave the unproven carrier eligible for the
// default arm's tier-2 admission. The readable control co-locates the existing
// behavior; the over-trigger control pins #7433's darwin path (a routinely-
// unknown NON-tmux link is still crossed, so a refused-argv launchd ancestor
// does not become a boundary).
func TestClassifyCaptureAncestryTreatsAnUnreadableTmuxServerAsABoundary(t *testing.T) {
	self := os.Getpid()
	aliveTmuxPID, aliveTmuxComm := startTmuxBoundarySleeper(t)

	// Readable control: a positively-identified live tmux parent is the
	// boundary, ending the walk with the verdict the `case boundary:` arm of
	// markedOrphanProcesses consumes (reason="", boundary=true).
	readableSnap := map[int]proctree.Process{
		aliveTmuxPID: {PID: aliveTmuxPID, PPID: 1, Comm: aliveTmuxComm},
		700001:       {PID: 700001, PPID: aliveTmuxPID, Comm: "sleep"},
	}
	reason, positive, boundary, _ := classifyCaptureAncestry(readableSnap, 700001, self, nil)
	require.Equal(t, "", reason, "a boundary carrier ends the walk eligible — boundary does not protect")
	require.False(t, positive)
	require.True(t, boundary, "control: a positively-identified tmux parent is a boundary")

	// The production race: the snapshot still maps the chain through a tmux
	// server (its kernel task name says `tmux`), but the live process-table
	// read failed at walk time — TmuxIdentityOf answers TmuxUnknown. A high
	// pid that was never alive reads exactly that, with no timing to race.
	const tmuxBoundaryPID = 700000
	require.Equal(t, proctree.TmuxUnknown, proctree.TmuxIdentityOf(tmuxBoundaryPID),
		"the boundary pid must read TmuxUnknown or this is not the race the fix targets")
	unreadableSnap := map[int]proctree.Process{
		tmuxBoundaryPID: {PID: tmuxBoundaryPID, PPID: 1, Comm: "tmux"},
		700001:          {PID: 700001, PPID: tmuxBoundaryPID, Comm: "sleep"},
	}
	reason, positive, boundary, _ = classifyCaptureAncestry(unreadableSnap, 700001, self, nil)
	require.Equal(t, "", reason, "the unreadable boundary keeps the carrier out of every protective placement")
	require.False(t, positive)
	require.True(t, boundary,
		"an unreadable tmux server the snapshot's task name still identifies as tmux must remain a boundary — "+
			"the walk must not cross it and send the unproven carrier to the default arm")

	// Over-trigger control (#7433's darwin path stays open): a routinely-
	// unknown NON-tmux link is still crossed. darwin refuses KERN_PROCARGS2 for
	// a foreign-uid ancestor like launchd, so TmuxUnknown is routine there, and
	// a broad stop on every Unknown re-broke the same five tests. The snapshot
	// task name does not name tmux here, so the walk proceeds — a `sleep`-comm
	// ancestor that cannot be read is ordinary, not a server boundary.
	const sleepBoundaryPID = 700010
	require.Equal(t, proctree.TmuxUnknown, proctree.TmuxIdentityOf(sleepBoundaryPID))
	sleepSnap := map[int]proctree.Process{
		sleepBoundaryPID: {PID: sleepBoundaryPID, PPID: 1, Comm: "sleep"},
		700001:           {PID: 700001, PPID: sleepBoundaryPID, Comm: "sleep"},
	}
	_, _, sleepBoundary, _ := classifyCaptureAncestry(sleepSnap, 700001, self, nil)
	require.False(t, sleepBoundary,
		"a routinely-unknown non-tmux link is not a boundary — only the live read's NotTmux licenses crossing")
}

// cohortMembership.admits must stop its descent at an unreadable tmux server,
// the same way it stops at a positively-identified one — so a proven member
// sitting ABOVE the server in the chain cannot vouch for the panes below it
// when that server became unreadable mid-sweep. The over-trigger control
// pins the inverse: a routinely-unknown non-tmux link is crossed, so descent
// still reaches the proven leader (darwin's launchd-ancestor shape).
func TestCohortMembershipAdmitsStopsDescentAtAnUnreadableTmuxServer(t *testing.T) {
	const (
		leaderPID        = 700002
		leaderStartID    = 42
		childPID         = 700001
		tmuxBoundaryPID  = 700000
		sleepBoundaryPID = 700010
	)
	// Precondition: the synthetic boundary pids are unreadable (the live
	// state of the production race), and the leader pid is unused so the
	// proven entry is a snapshot fact the walk consults, not a live process.
	require.Equal(t, proctree.TmuxUnknown, proctree.TmuxIdentityOf(tmuxBoundaryPID))
	require.Equal(t, proctree.TmuxUnknown, proctree.TmuxIdentityOf(sleepBoundaryPID))
	membership := cohortMembership{Proven: map[int]uint64{leaderPID: leaderStartID}}

	// Readable control: a positively-identified live tmux server stops the
	// descent — the #7435 invariant ("a pane that launched its own tmux
	// server does not make that server's panes ours").
	aliveTmuxPID, aliveTmuxComm := startTmuxBoundarySleeper(t)
	readableSnap := map[int]proctree.Process{
		aliveTmuxPID: {PID: aliveTmuxPID, PPID: leaderPID, Comm: aliveTmuxComm, StartID: 5},
		childPID:     {PID: childPID, PPID: aliveTmuxPID, Comm: "sleep", StartID: 1},
		leaderPID:    {PID: leaderPID, PPID: 1, Comm: "sleep", StartID: leaderStartID},
	}
	require.False(t, membership.admits(readableSnap, readableSnap[childPID], nil),
		"control: a readable tmux server stops descent — the proven leader above it cannot vouch for the child")

	// The bug: the same chain once the server's live read fails (TmuxUnknown).
	// Without the fix the descent crosses the server and the proven leader
	// vouches for the child — membership minted through the server.
	unreadableSnap := map[int]proctree.Process{
		tmuxBoundaryPID: {PID: tmuxBoundaryPID, PPID: leaderPID, Comm: "tmux", StartID: 5},
		childPID:        {PID: childPID, PPID: tmuxBoundaryPID, Comm: "sleep", StartID: 1},
		leaderPID:       {PID: leaderPID, PPID: 1, Comm: "sleep", StartID: leaderStartID},
	}
	require.False(t, membership.admits(unreadableSnap, unreadableSnap[childPID], nil),
		"an unreadable tmux server the snapshot still identifies as tmux must stop descent — "+
			"the proven leader above it must not vouch for the panes below")

	// Over-trigger control: a routinely-unknown non-tmux link is crossed, so
	// descent reaches the proven leader and admits the child. The fix must
	// not turn every TmuxUnknown into a stop, or it re-breaks #7433's darwin.
	sleepSnap := map[int]proctree.Process{
		sleepBoundaryPID: {PID: sleepBoundaryPID, PPID: leaderPID, Comm: "sleep", StartID: 5},
		childPID:         {PID: childPID, PPID: sleepBoundaryPID, Comm: "sleep", StartID: 1},
		leaderPID:        {PID: leaderPID, PPID: 1, Comm: "sleep", StartID: leaderStartID},
	}
	require.True(t, membership.admits(sleepSnap, sleepSnap[childPID], nil),
		"a routinely-unknown non-tmux link is crossed — descent reaches the proven leader, as before")
}

// A candidate proven by its OWN (pid, StartID) pair is admitted before the
// descent ever reaches a server in its chain — the fix stops only ancestors
// that carry no proof of their own, so a readable live server never strips a
// proven member of its own proof. This re-pins the #7435 behavior the fix's
// comment explicitly preserves.
func TestCohortMembershipAdmitsStillAdmitsAProvenMemberBelowAReadableServer(t *testing.T) {
	aliveTmuxPID, aliveTmuxComm := startTmuxBoundarySleeper(t)
	const childStartID = 9
	snap := map[int]proctree.Process{
		aliveTmuxPID: {PID: aliveTmuxPID, PPID: 1, Comm: aliveTmuxComm, StartID: 5},
		700001:       {PID: 700001, PPID: aliveTmuxPID, Comm: "sleep", StartID: childStartID},
	}
	// The child is itself proven by its own captured (pid, StartID) pair; the
	// server in its chain is irrelevant — proof vouches before any stop.
	membership := cohortMembership{Proven: map[int]uint64{700001: childStartID}}
	require.True(t, membership.admits(snap, snap[700001], nil),
		"a proven member is admitted even with a tmux server in its chain — proof vouches first")
}
