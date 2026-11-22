package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/afcli"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

// The #7752 tests: `af sessions archive --self` run inside the pane is the
// teardown REQUESTER and, to the reaper, an af argv this build's parse cannot
// decide — a non-positive, cleanup-blocking protected verdict (#7496 R1). The
// reaper correctly never signalled it, but its survivor reconstruction re-added
// every still-live non-positive exclusion (#7435 R3), the requester among them,
// so the close refused on the very process waiting for its answer, the archive
// rolled back to Lost, and the restore loop re-spawned the agent.
//
// The property pinned here is narrow on purpose: a REGISTERED requester — a
// kernel-verified (pid, startID) identity — is not a survivor. An unregistered
// claimant with the same verdict, or one whose registration names a different
// process instance, still blocks cleanup exactly as before.

// undecidableClaimantSet is the protected set the reaper computes for a
// captured process whose own argv is an undecidable af-daemon claim: this
// process positively, and pid as the blocking claimant itself.
func undecidableClaimantSet(pid int) protectedSet {
	set := selfProtectedSet()
	set[pid] = protectedVerdict{reason: protectReasonDaemonUndecidableSelf(), positive: false, unverifiedDaemon: pid}
	return set
}

func TestReapSessionProcessesRequesterIsNotASurvivor(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	requester := spawnRequesterSleeper(t)
	untrack := TrackTeardownRequester(requester)
	defer untrack()

	remaining := reapSessionProcesses(reapOnRequest, "af_requester_undecidable", []proctree.Process{requester},
		200*time.Millisecond, 300*time.Millisecond, undecidableClaimantSet(requester.PID), nil)

	require.Empty(t, remaining,
		"a registered requester blocked on this teardown's reply is not a survivor, whatever its argv verdict")
	require.True(t, proctree.AliveSame(requester),
		"premise: the requester is still alive — the excusal is registration, not exit")
	require.Contains(t, infoBuf.String(),
		fmt.Sprintf("teardown requester pid %d (%s) is blocked on this teardown's reply; not counting it as a survivor", requester.PID, requester.Comm),
		"the excusal is logged so a close that ignored a live protected pid is explainable")
	require.NotContains(t, warnBuf.String(), fmt.Sprintf("%d", requester.PID),
		"the requester must never appear in a reap WARNING")
}

func TestReapSessionProcessesUnverifiedClaimantStillBlocks(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	claimant := spawnRequesterSleeper(t)

	remaining := reapSessionProcesses(reapOnRequest, "af_claimant_unregistered", []proctree.Process{claimant},
		200*time.Millisecond, 300*time.Millisecond, undecidableClaimantSet(claimant.PID), nil)

	require.Equal(t, []proctree.Process{claimant}, remaining,
		"an unregistered undecidable claimant must still be reported as a survivor, so cleanup refuses (#7435 R3)")
	require.True(t, proctree.AliveSame(claimant), "a protected claimant is never signalled")
	require.NotContains(t, infoBuf.String(), "not counting it as a survivor",
		"only a registered requester earns the survivor excusal")
}

func TestReapSessionProcessesRequesterIdentityMismatch(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	claimant := spawnRequesterSleeper(t)
	// Registered under the same pid but a DIFFERENT process instance: a
	// recycled pid must not inherit a predecessor's excusal (#2103's rule).
	untrack := TrackTeardownRequester(proctree.Process{PID: claimant.PID, StartID: claimant.StartID + 1})
	defer untrack()
	require.False(t, isTeardownRequester(claimant), "premise: the live instance is not the registered one")

	remaining := reapSessionProcesses(reapOnRequest, "af_claimant_mismatch", []proctree.Process{claimant},
		200*time.Millisecond, 300*time.Millisecond, undecidableClaimantSet(claimant.PID), nil)

	require.Equal(t, []proctree.Process{claimant}, remaining,
		"the excusal is pinned to the registered (pid, startID) identity, not the pid slot")
	require.True(t, proctree.AliveSame(claimant), "a protected claimant is never signalled")
	require.NotContains(t, infoBuf.String(), "not counting it as a survivor")
}

// The requester can register AFTER this reap scanned the registry at entry —
// its RPC reaching its handler while this teardown is already running (Codex
// on #5186). The survivor check must consult the registry when it runs, not an
// entry-time snapshot. The registration is landed deterministically from
// inside the reap: the moment the reaper logs its protected-set exclusion for
// this pid — after the entry scan, before the survivor reconstruction — the
// INFO writer registers it.
func TestReapSessionProcessesRequesterRegisteredMidReapIsNotASurvivor(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	requester := spawnRequesterSleeper(t)
	exclusionLine := fmt.Sprintf("process pid %d (%s) is %s; excluding it from signalling (#7435)",
		requester.PID, requester.Comm, protectReasonDaemonUndecidableSelf())
	var (
		once    sync.Once
		untrack = func() {}
	)
	log.InfoLog.SetOutput(&onWriteBuffer{buf: &infoBuf, onWrite: func(p []byte) {
		if strings.Contains(string(p), exclusionLine) {
			once.Do(func() { untrack = TrackTeardownRequester(requester) })
		}
	}})
	t.Cleanup(func() { untrack() })
	require.False(t, isTeardownRequester(requester), "premise: unregistered when the reap starts")

	remaining := reapSessionProcesses(reapOnRequest, "af_requester_mid_reap", []proctree.Process{requester},
		200*time.Millisecond, 300*time.Millisecond, undecidableClaimantSet(requester.PID), nil)

	require.True(t, isTeardownRequester(requester),
		"premise: the registration landed inside the reap — the exclusion line was logged")
	require.NotContains(t, infoBuf.String(), "excluding it from signalling (#5182)",
		"premise: the entry-time registry scan did NOT see the requester")
	require.Empty(t, remaining,
		"a requester registered mid-reap is excused at the survivor check, not lost to an entry-time scan")
	require.True(t, proctree.AliveSame(requester))
}

// onWriteBuffer is a log writer that calls onWrite after recording each line —
// the deterministic in-reap hook the mid-reap registration test needs. The
// logger writes synchronously from the logging goroutine, so the hook runs at
// exactly the point the line is emitted.
type onWriteBuffer struct {
	buf     *logtest.Buffer
	onWrite func([]byte)
}

func (w *onWriteBuffer) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	w.onWrite(p)
	return n, err
}

// spawnUndecidableAFRequester starts a live, setsid'd process whose argv[0] is
// `af` and whose tail this build's daemon-claim parse cannot decide — the
// shape `af sessions archive --self` reads as to the reaper. It is a shell
// blocked in its `read` builtin, so it has no children of its own; the
// unknown `-c` is what makes af's own root parse undecidable. bash where
// present (no darwin /bin/sh shim between exec and the argv read), else sh.
func spawnUndecidableAFRequester(t *testing.T) proctree.Process {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		shell, err = exec.LookPath("sh")
	}
	require.NoError(t, err, "fixture needs a POSIX shell")
	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	child := &exec.Cmd{
		Path:        shell,
		Args:        []string{"af", "-c", "read -r _", "af", "sessions", "archive", "--self"},
		Stdin:       stdinR,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	require.NoError(t, child.Start())
	_ = stdinR.Close()
	t.Cleanup(func() {
		_ = stdinW.Close()
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})
	process := processIdentity(t, child.Process.Pid)
	require.Equal(t, afcli.DaemonClaimUndecidable, proctree.ArgvDaemonClaim(proctree.Argv(process.PID)),
		"fixture precondition: the requester's argv must classify as an undecidable af-daemon claim (argv %q)",
		proctree.Argv(process.PID))
	return process
}

// closeTwoPaneFixture stands in for tmux: list-panes answers two panes — the
// pane root display-message names, and the af requester as a second pane root
// — and kill-session SIGKILLs the pane root, as tmux's SIGHUP would end it.
// Both are children of this test binary (comm tmux.test), which is what the
// capture's tmux-server parent check reads.
func closeTwoPaneFixture(paneRoot, requester proctree.Process, killed *bool) cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "kill-session") {
				*killed = true
				_ = proctree.Signal(paneRoot, syscall.SIGKILL)
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			switch {
			case strings.Contains(c.String(), "list-panes"):
				return []byte(fmt.Sprintf("%d\n%d\n", paneRoot.PID, requester.PID)), nil
			case strings.Contains(c.String(), "display-message"):
				return []byte(fmt.Sprintf("%d\n", paneRoot.PID)), nil
			}
			return nil, nil
		},
	}
}

// The incident end to end: the requester is in the captured tree but is NOT
// the pane root, so waitForPaneExitOrRequester — which already excused a
// registered pane root — never sees it, and the survivors gate refused on it.
// Fixed, the close concludes Known while the requester stays alive to read
// its reply.
func TestCloseAndWaitForPaneExitExcusesUndecidableRequesterInCapturedTree(t *testing.T) {
	require.True(t, strings.HasPrefix(testProcessComm(t), "tmux"),
		"fixture requires this test binary's comm (%q) to verify as a tmux server", testProcessComm(t))

	paneRoot := spawnRequesterSleeper(t)
	requester := spawnUndecidableAFRequester(t)
	untrack := TrackTeardownRequester(requester)
	defer untrack()

	killed := false
	session := newTmuxSession(toTmuxName("close-undecidable-requester", ""), "af",
		NewMockPtyFactory(t), closeTwoPaneFixture(paneRoot, requester, &killed))
	state, err := session.CloseAndWaitForPaneExit()
	require.NoError(t, err,
		"the only live process left is the caller blocked on this teardown's answer — not a survivor")
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed, "kill-session still ran")
	require.False(t, proctree.AliveSame(paneRoot), "premise: the pane root itself is gone")
	require.True(t, proctree.AliveSame(requester),
		"the requester survived its own teardown request — it exits when the reply lands")
}

// The control for the test above: the same capture with the requester NOT
// registered. The undecidable claimant must still be reported and still
// refuse cleanup with its named remedy — the success above is registration,
// not an accident of the fixture.
func TestCloseAndWaitForPaneExitRefusesUnregisteredUndecidableClaimant(t *testing.T) {
	require.True(t, strings.HasPrefix(testProcessComm(t), "tmux"),
		"fixture requires this test binary's comm (%q) to verify as a tmux server", testProcessComm(t))

	paneRoot := spawnRequesterSleeper(t)
	claimant := spawnUndecidableAFRequester(t)

	killed := false
	session := newTmuxSession(toTmuxName("close-undecidable-claimant", ""), "af",
		NewMockPtyFactory(t), closeTwoPaneFixture(paneRoot, claimant, &killed))
	state, err := session.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state, "an unverified claimant still blocks worktree cleanup")
	require.Contains(t, err.Error(), fmt.Sprintf("pane processes %d are still alive after bounded teardown", claimant.PID))
	require.Contains(t, err.Error(), daemonClaimRemedy(claimant.PID),
		"the refusal names the claimant's own remedy (#7496 R3)")
	require.True(t, proctree.AliveSame(claimant), "a protected claimant is never signalled")
}
