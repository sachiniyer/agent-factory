package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
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
// membership cannot be minted through it to a proven ancestor above.
//
// #7848 then split the protection walk's use of that fix: a tmux-named link
// the live read could not classify SETS the boundary flag the walk reports
// but never ENDS it — the verdict is the UNION of the flag and whatever
// decisive verdict the chain still reaches above the link, so a positive
// protection (the reaper's own subtree, a proven af daemon's) survives a
// NON-positive read exactly as it did before #7834. The membership walk's
// stop is unchanged: minting descent proof through an unreadable server is
// still the crossing #7834 closed.
//
// Every fixture pid here is PROVABLY not live — never assumed free (#7848):
// the deadPID* constants sit above every pid ceiling a test host can carry
// (Linux's kernel.pid_max cannot exceed 2^22 = 4194304, and macOS's maxpid
// is 99999), so a snapshot entry keyed on one reads TmuxUnknown on any host,
// with no zombie/subreaper timing to race. The earlier 700000-range pids
// were merely assumed free and could be live on a host whose pid_max is
// 4194304 — the require guard made a collision fail red rather than pass
// falsely, but a provably-dead pid is what the fixture owes.
const (
	// deadTmuxLinkPID plays the unreadable tmux-named boundary link.
	deadTmuxLinkPID = 0x500000
	// deadCarrierPID plays the captured candidate under test.
	deadCarrierPID = 0x500001
	// deadLeaderPID plays the proven member above the link in the descent
	// fixtures.
	deadLeaderPID = 0x500002
	// deadNonTmuxPID plays the routinely-unknown NON-tmux link of the
	// over-trigger controls (#7433's darwin path).
	deadNonTmuxPID = 0x500010
)

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

// requireDeadFixturePID pins the premise every deadPID* fixture rests on:
// the live reads the walk makes on it must answer TmuxUnknown — a pid above
// every platform's pid ceiling is not merely assumed free, it cannot exist.
func requireDeadFixturePID(t *testing.T, pid int, role string) {
	t.Helper()
	require.Equal(t, proctree.TmuxUnknown, proctree.TmuxIdentityOf(pid),
		"%s must read TmuxUnknown — it is above every platform's pid ceiling, so this is a fact, not an assumption", role)
	require.Empty(t, proctree.Argv(pid),
		"%s must have no readable argv or the daemon classifier would read the live process instead", role)
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
		aliveTmuxPID:   {PID: aliveTmuxPID, PPID: 1, Comm: aliveTmuxComm},
		deadCarrierPID: {PID: deadCarrierPID, PPID: aliveTmuxPID, Comm: "sleep"},
	}
	reason, positive, boundary, _ := classifyCaptureAncestry(readableSnap, deadCarrierPID, self, nil)
	require.Equal(t, "", reason, "a boundary carrier ends the walk eligible — boundary does not protect")
	require.False(t, positive)
	require.True(t, boundary, "control: a positively-identified tmux parent is a boundary")

	// The production race: the snapshot still maps the chain through a tmux
	// server (its kernel task name says `tmux`), but the live process-table
	// read failed at walk time — TmuxIdentityOf answers TmuxUnknown. A pid
	// above every pid ceiling reads exactly that, with no timing to race.
	requireDeadFixturePID(t, deadTmuxLinkPID, "the boundary pid")
	requireDeadFixturePID(t, deadCarrierPID, "the carrier pid")
	unreadableSnap := map[int]proctree.Process{
		deadTmuxLinkPID: {PID: deadTmuxLinkPID, PPID: 1, Comm: "tmux"},
		deadCarrierPID:  {PID: deadCarrierPID, PPID: deadTmuxLinkPID, Comm: "sleep"},
	}
	reason, positive, boundary, _ = classifyCaptureAncestry(unreadableSnap, deadCarrierPID, self, nil)
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
	requireDeadFixturePID(t, deadNonTmuxPID, "the non-tmux link pid")
	sleepSnap := map[int]proctree.Process{
		deadNonTmuxPID: {PID: deadNonTmuxPID, PPID: 1, Comm: "sleep"},
		deadCarrierPID: {PID: deadCarrierPID, PPID: deadNonTmuxPID, Comm: "sleep"},
	}
	_, _, sleepBoundary, _ := classifyCaptureAncestry(sleepSnap, deadCarrierPID, self, nil)
	require.False(t, sleepBoundary,
		"a routinely-unknown non-tmux link is not a boundary — only the live read's NotTmux licenses crossing")
}

// #7848 — in the protection walk an unreadable tmux-named link SETS the
// boundary flag but never ENDS it: the verdict is the UNION of the flag and
// the decisive verdict the chain still reaches above the link. #7834 made the
// same link END the walk at the boundary — right for an unproven carrier, but
// it also dropped the frame reaper → X(comm `tmux: server`, live read
// TmuxUnknown) → captured C onto the eligible verdict, letting the capture
// path signal a descendant of the reaper a positive protection covered before.
// These frames pin the union end to end, at the classifier and inside the
// protected set the capture path reads.
func TestClassifyCaptureAncestryKeepsProtectionsAboveAnUnreadableTmuxLink(t *testing.T) {
	self := os.Getpid()
	selfProc, err := proctree.Lookup(self)
	require.NoError(t, err)
	requireDeadFixturePID(t, deadTmuxLinkPID, "the boundary link pid")
	requireDeadFixturePID(t, deadCarrierPID, "the carrier pid")

	// The #7848 frame: the captured process descends from the reaper through
	// a link the live read could not classify while the snapshot's task name
	// still calls it tmux. Before the fix the link ended the walk
	// boundary-eligible — a NON-positive read lifting a POSITIVE protection.
	selfSnap := map[int]proctree.Process{
		deadCarrierPID:  {PID: deadCarrierPID, PPID: deadTmuxLinkPID, Comm: "sleep"},
		deadTmuxLinkPID: {PID: deadTmuxLinkPID, PPID: self, Comm: "tmux: server"},
		self:            {PID: self, PPID: 1, Comm: selfProc.Comm},
	}
	reason, positive, boundary, unverifiedDaemon := classifyCaptureAncestry(selfSnap, deadCarrierPID, self, nil)
	require.Equal(t, protectReasonSelfSubtree, reason,
		"the reaper above an unreadable tmux-named link keeps its protection — the verdict is the union")
	require.True(t, positive,
		"the self-subtree placement stays POSITIVE — proven foreign, named rather than blocking")
	require.True(t, boundary,
		"the flag still reports the tmux-named link the walk crossed — union, not replacement")
	require.Zero(t, unverifiedDaemon)

	protected := reapProtectedSet(selfSnap, []proctree.Process{selfSnap[deadCarrierPID]}, nil)
	verdict, ok := protected[deadCarrierPID]
	require.True(t, ok,
		"the protected set still excludes the reaper's descendant across the unreadable link — "+
			"the capture path must not be able to signal it")
	require.Equal(t, protectReasonSelfSubtree, verdict.reason)
	require.True(t, verdict.positive)

	// The same frame with X POSITIVELY identified keeps today's eligible
	// verdict: a real tmux server between the reaper and the carrier still
	// ends the walk at the boundary — protection a positive server lifts.
	aliveTmuxPID, aliveTmuxComm := startTmuxBoundarySleeper(t)
	positiveSnap := map[int]proctree.Process{
		deadCarrierPID: {PID: deadCarrierPID, PPID: aliveTmuxPID, Comm: "sleep"},
		aliveTmuxPID:   {PID: aliveTmuxPID, PPID: self, Comm: aliveTmuxComm},
		self:           {PID: self, PPID: 1, Comm: selfProc.Comm},
	}
	reason, positive, boundary, _ = classifyCaptureAncestry(positiveSnap, deadCarrierPID, self, nil)
	require.Equal(t, "", reason,
		"a positively-identified tmux link still ends the walk eligible at the boundary — unchanged")
	require.False(t, positive)
	require.True(t, boundary)
	require.False(t,
		reapProtectedSet(positiveSnap, []proctree.Process{positiveSnap[deadCarrierPID]}, nil).protects(deadCarrierPID),
		"a carrier behind a REAL server stays out of the protected set — the capture path still reaps it")

	// And nothing protective above the link: the unproven carrier keeps the
	// `case boundary:` refusal #7834 added — the union flag still reports on
	// the eligible verdict, so the carrier is refused, not admitted.
	topSnap := map[int]proctree.Process{
		deadCarrierPID:  {PID: deadCarrierPID, PPID: deadTmuxLinkPID, Comm: "sleep"},
		deadTmuxLinkPID: {PID: deadTmuxLinkPID, PPID: 1, Comm: "tmux: server"},
	}
	reason, positive, boundary, _ = classifyCaptureAncestry(topSnap, deadCarrierPID, self, nil)
	require.Equal(t, "", reason,
		"no protective verdict above the link — the carrier stays unattributable, not admitted")
	require.False(t, positive)
	require.True(t, boundary,
		"the union flag keeps the boundary refusal an unproven carrier behind the link needs")
}

// The daemon-ancestor variant of the #7848 union: X(comm `tmux: server`,
// live read TmuxUnknown) sits between a positively-identified af daemon and
// the captured process. The daemon's POSITIVE placement — the one a bare
// argv claim cannot fake — survives the non-positive read between it and the
// captured process, exactly as it did before #7834.
func TestClassifyCaptureAncestryKeepsDaemonProtectionAboveAnUnreadableTmuxLink(t *testing.T) {
	// A live VERIFIED daemon ancestor: the test binary is an af-module
	// build, so argv `af --daemon` claims what the kernel-attested
	// executable proves (#7491) — the same fixture TestReportableSurvivors
	// drives. An unverified claim would pin the weaker direction; the issue
	// asks for the positive placement.
	daemon := exec.Command(os.Args[0])
	daemon.Args = []string{"af", "--daemon"}
	daemon.Env = append(os.Environ(), afDaemonSleepEnv+"=1")
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, daemon.Start())
	t.Cleanup(func() { _ = daemon.Process.Kill(); _, _ = daemon.Process.Wait() })
	require.Eventually(t, func() bool {
		return proctree.ArgvIsAFDaemon(proctree.Argv(daemon.Process.Pid))
	}, 5*time.Second, 20*time.Millisecond, "the daemon argv must land before the verdict is read")
	daemonProc, err := proctree.Lookup(daemon.Process.Pid)
	require.NoError(t, err)
	require.Equal(t, afDaemonIs, afDaemonClass(daemonProc, nil),
		"the fixture daemon must positively classify or this test means nothing")

	requireDeadFixturePID(t, deadTmuxLinkPID, "the boundary link pid")
	requireDeadFixturePID(t, deadCarrierPID, "the carrier pid")

	snap := map[int]proctree.Process{
		deadCarrierPID:     {PID: deadCarrierPID, PPID: deadTmuxLinkPID, Comm: "sleep"},
		deadTmuxLinkPID:    {PID: deadTmuxLinkPID, PPID: daemon.Process.Pid, Comm: "tmux: server"},
		daemon.Process.Pid: {PID: daemon.Process.Pid, PPID: os.Getpid(), Comm: daemonProc.Comm, StartID: daemonProc.StartID},
	}
	reason, positive, boundary, unverifiedDaemon := classifyCaptureAncestry(snap, deadCarrierPID, os.Getpid(), nil)
	require.Equal(t, protectReasonDaemonDescendant(daemon.Process.Pid), reason,
		"a proven af daemon above an unreadable tmux-named link keeps its protection — the verdict is the union")
	require.True(t, positive,
		"a kernel-attested daemon subtree is a POSITIVE placement — named, not blocking")
	require.True(t, boundary,
		"the flag still reports the tmux-named link the walk crossed — union, not replacement")
	require.Zero(t, unverifiedDaemon)

	protected := reapProtectedSet(snap, []proctree.Process{snap[deadCarrierPID]}, nil)
	verdict, ok := protected[deadCarrierPID]
	require.True(t, ok,
		"the daemon's descendant across the unreadable link stays excluded — the capture path must not signal it")
	require.Equal(t, protectReasonDaemonDescendant(daemon.Process.Pid), verdict.reason)
	require.True(t, verdict.positive)
}

// cohortMembership.admits must stop its descent at an unreadable tmux server,
// the same way it stops at a positively-identified one — so a proven member
// sitting ABOVE the server in the chain cannot vouch for the panes below it
// when that server became unreadable mid-sweep. The over-trigger control
// pins the inverse: a routinely-unknown non-tmux link is crossed, so descent
// still reaches the proven leader (darwin's launchd-ancestor shape).
func TestCohortMembershipAdmitsStopsDescentAtAnUnreadableTmuxServer(t *testing.T) {
	const leaderStartID = 42
	// Precondition: the synthetic boundary pids are unreadable (the live
	// state of the production race), and the leader pid is provably dead so
	// the proven entry is a snapshot fact the walk consults, not a live
	// process.
	requireDeadFixturePID(t, deadTmuxLinkPID, "the boundary pid")
	requireDeadFixturePID(t, deadNonTmuxPID, "the non-tmux link pid")
	requireDeadFixturePID(t, deadLeaderPID, "the leader pid")
	requireDeadFixturePID(t, deadCarrierPID, "the carrier pid")
	membership := cohortMembership{Proven: map[int]uint64{deadLeaderPID: leaderStartID}}

	// Readable control: a positively-identified live tmux server stops the
	// descent — the #7435 invariant ("a pane that launched its own tmux
	// server does not make that server's panes ours").
	aliveTmuxPID, aliveTmuxComm := startTmuxBoundarySleeper(t)
	readableSnap := map[int]proctree.Process{
		aliveTmuxPID:   {PID: aliveTmuxPID, PPID: deadLeaderPID, Comm: aliveTmuxComm, StartID: 5},
		deadCarrierPID: {PID: deadCarrierPID, PPID: aliveTmuxPID, Comm: "sleep", StartID: 1},
		deadLeaderPID:  {PID: deadLeaderPID, PPID: 1, Comm: "sleep", StartID: leaderStartID},
	}
	require.False(t, membership.admits(readableSnap, readableSnap[deadCarrierPID], nil),
		"control: a readable tmux server stops descent — the proven leader above it cannot vouch for the child")

	// The bug: the same chain once the server's live read fails (TmuxUnknown).
	// Without the fix the descent crosses the server and the proven leader
	// vouches for the child — membership minted through the server.
	unreadableSnap := map[int]proctree.Process{
		deadTmuxLinkPID: {PID: deadTmuxLinkPID, PPID: deadLeaderPID, Comm: "tmux", StartID: 5},
		deadCarrierPID:  {PID: deadCarrierPID, PPID: deadTmuxLinkPID, Comm: "sleep", StartID: 1},
		deadLeaderPID:   {PID: deadLeaderPID, PPID: 1, Comm: "sleep", StartID: leaderStartID},
	}
	require.False(t, membership.admits(unreadableSnap, unreadableSnap[deadCarrierPID], nil),
		"an unreadable tmux server the snapshot still identifies as tmux must stop descent — "+
			"the proven leader above it must not vouch for the panes below")

	// Over-trigger control: a routinely-unknown non-tmux link is crossed, so
	// descent reaches the proven leader and admits the child. The fix must
	// not turn every TmuxUnknown into a stop, or it re-breaks #7433's darwin.
	sleepSnap := map[int]proctree.Process{
		deadNonTmuxPID: {PID: deadNonTmuxPID, PPID: deadLeaderPID, Comm: "sleep", StartID: 5},
		deadCarrierPID: {PID: deadCarrierPID, PPID: deadNonTmuxPID, Comm: "sleep", StartID: 1},
		deadLeaderPID:  {PID: deadLeaderPID, PPID: 1, Comm: "sleep", StartID: leaderStartID},
	}
	require.True(t, membership.admits(sleepSnap, sleepSnap[deadCarrierPID], nil),
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
		aliveTmuxPID:   {PID: aliveTmuxPID, PPID: 1, Comm: aliveTmuxComm, StartID: 5},
		deadCarrierPID: {PID: deadCarrierPID, PPID: aliveTmuxPID, Comm: "sleep", StartID: childStartID},
	}
	// The child is itself proven by its own captured (pid, StartID) pair; the
	// server in its chain is irrelevant — proof vouches before any stop.
	membership := cohortMembership{Proven: map[int]uint64{deadCarrierPID: childStartID}}
	require.True(t, membership.admits(snap, snap[deadCarrierPID], nil),
		"a proven member is admitted even with a tmux server in its chain — proof vouches first")
}
