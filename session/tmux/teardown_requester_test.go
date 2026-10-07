package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

// The #5182 tests: a teardown requester — the process blocked on the
// archive/kill/close-tab reply — lives INSIDE the pane tree being reaped, so
// before the fix the reaper waited out reapGraceWait on a caller that could
// not exit, SIGTERMed it, and logged it as a leak, and the committed
// teardown's reply died unread.
//
// Everything here is fake process trees and mock executors: no daemon, no
// tmux server. The "requester" is a real `sleep` child standing in for the
// blocked `af` process — its liveness is what the assertions read.

// spawnRequesterSleeper returns the process identity of a real live sleeper
// that survives until the test kills it. setsid makes it its own kernel
// session leader, so proctree.SessionMembers for it contains only itself —
// the same isolation a real tmux pane root has.
func spawnRequesterSleeper(t *testing.T) proctree.Process {
	t.Helper()
	child := exec.Command("sleep", "300")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, child.Start())
	process := processIdentity(t, child.Process.Pid)
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})
	return process
}

func TestTrackTeardownRequesterScopesByIdentity(t *testing.T) {
	requester := proctree.Process{PID: 424242, StartID: 777}
	require.False(t, isTeardownRequester(requester))

	untrack := TrackTeardownRequester(requester)
	t.Cleanup(untrack)
	require.True(t, isTeardownRequester(requester),
		"the registered process must read as a teardown requester")

	// A pid recycled into a NEW process instance is not the requester: the
	// (pid, start-stamp) pair is the identity, not the slot (#2103's rule).
	require.False(t, isTeardownRequester(proctree.Process{PID: requester.PID, StartID: requester.StartID + 1}),
		"a same-pid different-start identity must not inherit the exemption")

	untrack()
	require.False(t, isTeardownRequester(requester),
		"the exemption ends when the handler returns")
	untrack() // a second unregister is a no-op, not a panic
}

// Two overlapping handlers from the same process — concurrent calls on one
// net/rpc connection, or a second connection carrying the same kernel peer —
// share one registry identity. The FIRST to return must not drop the
// exemption the still-blocked second call needs, or its reap would signal
// the requester whose reply is still outstanding (Codex on #5186).
func TestTrackTeardownRequesterCountsOverlappingRegistrations(t *testing.T) {
	requester := proctree.Process{PID: 424243, StartID: 888}
	first := TrackTeardownRequester(requester)
	second := TrackTeardownRequester(requester)

	first()
	require.True(t, isTeardownRequester(requester),
		"one handler returning must not unregister a second still in flight")

	second()
	require.False(t, isTeardownRequester(requester),
		"the exemption ends when the last overlapping handler returns")
}

// TrackTeardownRequester tolerates an unresolvable identity: the daemon calls
// it with whatever proctree.Lookup produced, and a failure there must degrade
// to "no requester" rather than tracking garbage or crashing the handler.
func TestTrackTeardownRequesterZeroProcessIsNoop(t *testing.T) {
	untrack := TrackTeardownRequester(proctree.Process{})
	require.NotNil(t, untrack)
	untrack()
	require.False(t, isTeardownRequester(proctree.Process{PID: 0, StartID: 0}))
}

// The requester is never reported as a leaked process — the whole point of
// #5182(a): it is never waited on and never signalled, and where a reap log
// line does mention it, it is the exclusion notice, not a leak report.
func TestReapSessionProcessesNeverWaitsOnOrSignalsRequester(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	requester := spawnRequesterSleeper(t)
	untrack := TrackTeardownRequester(requester)
	defer untrack()

	// A grace far longer than the call should take: the exemption must make
	// the wait a non-event, not merely a shorter one.
	grace := 2 * time.Second
	start := time.Now()
	remaining := reapSessionProcesses(reapOnRequest, "af_requester_reap", []proctree.Process{requester}, grace, 300*time.Millisecond)
	elapsed := time.Since(start)

	require.Empty(t, remaining, "the exempt requester is not a leftover — nothing was reaped")
	require.True(t, proctree.AliveSame(requester),
		"the requester must never be signalled — it is blocked on the reply this teardown is about to send")
	require.Less(t, elapsed, grace,
		"teardown must not stall the grace period on a requester that cannot exit until the reply lands")

	require.NotContains(t, warnBuf.String(), fmt.Sprintf("%d", requester.PID),
		"the requester must never appear in a reap WARNING — it is not a leak")
	require.Contains(t, infoBuf.String(), fmt.Sprintf("%d", requester.PID),
		"the exclusion is logged at INFO so the sweep's silence about it is explainable")
}

func TestReapSessionProcessesStillReapsGenuineLeaksBesideRequester(t *testing.T) {
	requester := spawnRequesterSleeper(t)
	leak := spawnRequesterSleeper(t)
	untrack := TrackTeardownRequester(requester)
	defer untrack()

	remaining := reapSessionProcesses(reapOnRequest, "af_requester_and_leak",
		[]proctree.Process{requester, leak}, 200*time.Millisecond, 500*time.Millisecond)

	require.Empty(t, remaining, "a plain sleeper dies to SIGTERM — nothing should outlive the reap")
	require.True(t, proctree.AliveSame(requester),
		"the requester stays exempt even in a mixed capture")
	require.False(t, proctree.AliveSame(leak),
		"the genuine leak is waited on, signalled, and gone — exactly as before #5182")
}

// A requester can be registered AFTER the reap's set was already captured:
// teardown A holds this client in its grace wait while the client's own
// destructive RPC is only now reaching its handler (Codex on #5186). The
// registry is re-consulted before every signal tier, so a requester tracked
// mid-grace is still spared rather than SIGTERMed.
func TestReapSessionProcessesSparesRequesterRegisteredMidGrace(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	requester := spawnRequesterSleeper(t)
	// Deliberately NOT tracked at capture time — the registration lands while
	// the reap is already inside its grace wait, as a still-in-flight RPC
	// handler would.
	untrackCh := make(chan func(), 1)
	go func() {
		time.Sleep(250 * time.Millisecond)
		untrackCh <- TrackTeardownRequester(requester)
	}()
	t.Cleanup(func() { (<-untrackCh)() })

	remaining := reapSessionProcesses(reapOnRequest, "af_requester_late",
		[]proctree.Process{requester}, 900*time.Millisecond, 300*time.Millisecond)

	require.Empty(t, remaining, "the late-tracked requester leaves the signal set, not the wait")
	require.True(t, proctree.AliveSame(requester),
		"a requester registered during the grace wait must still never be signalled")
	require.NotContains(t, warnBuf.String(), fmt.Sprintf("%d", requester.PID),
		"the late-tracked requester must never appear in a reap WARNING")
	require.Contains(t, infoBuf.String(), "registered while this reap was already waiting",
		"the mid-reap exemption is logged so the missing SIGTERM line is explainable")
}

// The exemption is scoped to the handler: once the reply is in flight the
// unregister has run, and a stale registration cannot hide the process from a
// LATER reap that legitimately owns it.
func TestReapSessionProcessesReapsRequesterAfterUnregister(t *testing.T) {
	requester := spawnRequesterSleeper(t)
	untrack := TrackTeardownRequester(requester)
	untrack()

	reapSessionProcesses(reapOnRequest, "af_requester_untracked", []proctree.Process{requester},
		200*time.Millisecond, 500*time.Millisecond)
	require.False(t, proctree.AliveSame(requester),
		"an unregistered process is reaped like anything else — the exemption does not leak past its scope")
}

// addOrReplaceOrphanCandidate is the vanished-session sweep's ingestion point;
// the requester must never be admitted there either, or the sweep's grace
// passes would stall on it and mark it for reaping.
func TestOrphanCandidatesNeverAdmitRequester(t *testing.T) {
	requester := proctree.Process{PID: 400010, StartID: 1, Comm: "af"}
	other := proctree.Process{PID: 400011, StartID: 2, Comm: "leftover"}
	untrack := TrackTeardownRequester(requester)
	defer untrack()

	byPID := map[int]int{}
	got := addOrReplaceOrphanCandidate(nil, byPID, requester)
	require.Empty(t, got, "a tracked requester is never an orphan candidate")
	require.NotContains(t, byPID, requester.PID,
		"the requester's pid slot must stay free — a later real process with it would otherwise be skipped")

	got = addOrReplaceOrphanCandidate(got, byPID, other)
	require.Equal(t, []proctree.Process{other}, got, "non-requesters are admitted unchanged")
}

// The end-to-end close seam: the requester IS the pane's whole captured tree
// (a session whose only pane process is the blocked `af`). Unfixed code waits
// paneExitWait on it and SIGTERMs it in the reaper; fixed code recognizes it
// at every seam and lets the close conclude cleanly while it stays alive to
// read its reply.
//
// The fixture stands in for tmux with a MockCmdExec: display-message and
// list-panes both answer the sleeper's pid as the pane root, and kill-session
// succeeds. The capture's pane-root verification wants a process whose parent
// comm starts with "tmux" — this package's own test binary is named
// tmux.test, so a child of the test process satisfies it honestly.
func TestCloseAndWaitForPaneExitNeverReapsItsRequester(t *testing.T) {
	require.True(t, strings.HasPrefix(testProcessComm(t), "tmux"),
		"fixture requires this test binary's comm (%q) to verify as a tmux server", testProcessComm(t))

	requester := spawnRequesterSleeper(t)
	untrack := TrackTeardownRequester(requester)
	defer untrack()

	killed := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error {
			killed = true
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "list-panes") || strings.Contains(cmd.String(), "display-message") {
				return []byte(fmt.Sprintf("%d\n", requester.PID)), nil
			}
			return nil, nil
		},
	}

	session := newTmuxSession(toTmuxName("close-requester-pane", ""), "af", NewMockPtyFactory(t), cmdExec)
	state, err := session.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state,
		"teardown must conclude: the only thing left alive is the caller waiting on this answer")
	require.True(t, killed, "kill-session still ran")
	require.True(t, proctree.AliveSame(requester),
		"the requester survived its own teardown request — it exits when the reply lands, not at SIGTERM")
}

// The other half of the contract, same fixture without registration: an
// UNTRACKED process occupying the same pane slot is still waited on and
// reaped. This is what pins the exemption to kernel-verified registration —
// a process cannot get itself spared merely by being in the captured set.
func TestCloseAndWaitForPaneExitReapsUntrackedCallerInSameTree(t *testing.T) {
	shrinkReapWaits(t)
	require.True(t, strings.HasPrefix(testProcessComm(t), "tmux"),
		"fixture requires this test binary's comm (%q) to verify as a tmux server", testProcessComm(t))

	stranger := spawnRequesterSleeper(t)

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "list-panes") || strings.Contains(cmd.String(), "display-message") {
				return []byte(fmt.Sprintf("%d\n", stranger.PID)), nil
			}
			return nil, nil
		},
	}

	session := newTmuxSession(toTmuxName("close-untracked-pane", ""), "af", NewMockPtyFactory(t), cmdExec)
	state, err := session.CloseAndWaitForPaneExit()
	require.NoError(t, err, "a reaped pane is still a successful teardown")
	require.Equal(t, PaneStateKnown, state)
	require.False(t, proctree.AliveSame(stranger),
		"an untracked process in the same tree is reaped exactly as before — registration is the only exemption")
}

// testProcessComm reports this test binary's comm — the value
// CaptureSessionProcessTrees's tmux-child verification reads for the parent
// of a sleeper spawned here.
func testProcessComm(t *testing.T) string {
	t.Helper()
	self, err := proctree.Lookup(os.Getpid())
	require.NoError(t, err)
	return self.Comm
}

// redirectReapLogs points both reap-severity loggers at buffers for the test,
// so "never logged as leaked" is an assertion on bytes, not an inference.
func redirectReapLogs(t *testing.T, info, warn *logtest.Buffer) {
	t.Helper()
	oldInfoOut, oldInfoFlags := log.InfoLog.Writer(), log.InfoLog.Flags()
	oldWarnOut, oldWarnFlags := log.WarningLog.Writer(), log.WarningLog.Flags()
	log.InfoLog.SetOutput(info)
	log.InfoLog.SetFlags(0)
	log.WarningLog.SetOutput(warn)
	log.WarningLog.SetFlags(0)
	t.Cleanup(func() {
		log.InfoLog.SetOutput(oldInfoOut)
		log.InfoLog.SetFlags(oldInfoFlags)
		log.WarningLog.SetOutput(oldWarnOut)
		log.WarningLog.SetFlags(oldWarnFlags)
	})
}
