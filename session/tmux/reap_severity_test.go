package tmux

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
)

// reapLogBuffer is a mutex-guarded log sink. Close reaps in its OWN goroutine,
// so the poll below reads these buffers while the reaper writes them through the
// logger. log.Logger serializes its own writes but nothing guards a concurrent
// READ, and a bytes.Buffer touched from two goroutines is a data race — caught
// by -race on the macOS gate, not by an ordinary local run. Same shape as the
// syncBuffer helpers in apiclient and daemon.
type reapLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *reapLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *reapLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureReapLogs redirects the WARNING and INFO loggers into buffers for the
// duration of the test and returns readers for both. Severity is the whole
// subject here, so a test that read only one of them could not tell a line that
// moved from one that vanished.
func captureReapLogs(t *testing.T) (warnings, infos func() string) {
	t.Helper()
	var warnBuf, infoBuf reapLogBuffer
	oldWarnOut, oldWarnFlags := log.WarningLog.Writer(), log.WarningLog.Flags()
	oldInfoOut, oldInfoFlags := log.InfoLog.Writer(), log.InfoLog.Flags()
	log.WarningLog.SetOutput(&warnBuf)
	log.WarningLog.SetFlags(0)
	log.InfoLog.SetOutput(&infoBuf)
	log.InfoLog.SetFlags(0)
	t.Cleanup(func() {
		log.WarningLog.SetOutput(oldWarnOut)
		log.WarningLog.SetFlags(oldWarnFlags)
		log.InfoLog.SetOutput(oldInfoOut)
		log.InfoLog.SetFlags(oldInfoFlags)
	})
	return warnBuf.String, infoBuf.String
}

// TestCloseDoesNotReportRequestedTeardownAsALeak is the regression test for
// #2765.
//
// Close IS the teardown someone asked for. Every process in the pane tree is
// supposed to die with the session, so a process the reaper has to SIGTERM there
// is the operation working, not a finding — and the most reliable way to produce
// one was to use a supported feature exactly as designed: `af sessions archive
// --self` makes its request from inside the pane tree it is asking to tear down,
// blocked on the very RPC doing the tearing, so it can never exit within the
// grace period. Every self-archive named the operator's own command as a "leaked
// process" at WARNING.
//
// The pane child here stands in for that caller: a process in the tree that
// outlives kill-session's SIGHUP and must be reaped. Against the pre-fix
// behaviour this logs `WARNING … reaping leaked process N (sleep) with SIGTERM`
// and the run stops on the assertion below that says a requested teardown must
// not call its own processes leaked.
func TestCloseDoesNotReportRequestedTeardownAsALeak(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_reap-severity-close"
	escapee := spawnSessionWithEscapee(t, name)
	require.True(t, proctree.AliveSame(escapee), "escapee must be alive before Close")

	warnings, infos := captureReapLogs(t)

	session := NewTmuxSessionFromSanitizedName(name, "sh")
	_, closeErr := session.Close()
	require.NoError(t, closeErr)

	// Close reaps asynchronously, so wait for the reap to actually happen before
	// reading the logs — otherwise an empty WARNING buffer would pass for the
	// wrong reason (nothing was reaped yet).
	require.Eventually(t, func() bool { return !proctree.AliveSame(escapee) },
		5*time.Second, 25*time.Millisecond,
		"SIGHUP-immune pane child survived Close — nothing was reaped, so this test proves nothing")
	// Wait on EITHER buffer, not just INFO: the reap signals before it logs, so
	// the escapee can be dead a moment before the line lands. Gating on INFO alone
	// would make the pre-fix behaviour — a line written at WARNING — time out here
	// reporting "nothing was logged", which is not what went wrong. Waiting for a
	// line at any severity lets the assertions below name the real defect.
	require.Eventually(t, func() bool { return infos() != "" || warnings() != "" },
		5*time.Second, 25*time.Millisecond,
		"the reap logged nothing at all, at either severity")

	require.NotContains(t, warnings(), "leaked",
		"a requested teardown reported its own pane processes as leaked — an operator grepping "+
			"for leaks finds every `af sessions archive --self` they ever ran (#2765)")
	require.Contains(t, infos(), "tearing down on request",
		"the reap must still be reported, at INFO, naming why the process was signalled")
	require.Contains(t, infos(), "tmux "+name+":",
		"the INFO line must name the session, and pass it as an argument (#1211)")
	require.NotContains(t, infos(), "%!",
		"format-string corruption markers must not appear on the INFO path either (#1211)")
}

// TestVanishedSessionSweepStillReportsALeak is the other half of #2765: the
// downgrade must not swallow the finding the reaper exists for.
//
// Here the tmux session is GONE and a marked helper is still alive — it outlived
// the pane tree that was supposed to contain it. Nobody asked for that, and it is
// the case the WARNING is worth reading.
func TestVanishedSessionSweepStillReportsALeak(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_reap-severity-vanished"
	home := t.TempDir()
	marked := spawnMarkedSessionWithEscapee(t, name, home)
	t.Setenv("AGENT_FACTORY_HOME", home)

	warnings, _ := captureReapLogs(t)

	require.NoError(t, CleanupSessions(vanishOnMarkerExecutor(t, name)))
	require.False(t, proctree.AliveSame(marked),
		"marked helper survived the sweep — nothing was reaped, so this test proves nothing")

	require.Contains(t, warnings(), "leaked past its pane tree",
		"a process that outlived its vanished tmux session must still be reported as a leak, at WARNING")
}

// TestReapReasonSeverityMapping pins the (reason, outcome) → severity table
// directly, including the tiers an end-to-end test cannot force: a process that
// ignores SIGTERM or survives SIGKILL is abnormal whoever asked for the teardown,
// so those must stay at WARNING even on the on-request path.
func TestReapReasonSeverityMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reason     reapReason
		outcome    proctree.ReapOutcome
		wantOnWarn bool
	}{
		{"on-request SIGTERM is routine", reapOnRequest, proctree.ReapSignalled, false},
		{"on-request ignored SIGTERM is not", reapOnRequest, proctree.ReapNeededKill, true},
		{"on-request unkillable is not", reapOnRequest, proctree.ReapUnkillable, true},
		{"escaped SIGTERM is a finding", reapEscaped, proctree.ReapSignalled, true},
		{"escaped ignored SIGTERM is a finding", reapEscaped, proctree.ReapNeededKill, true},
		{"escaped unkillable is a finding", reapEscaped, proctree.ReapUnkillable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings, infos := captureReapLogs(t)
			logReapOutcome(tc.reason, "af_probe", tc.outcome, "probe line")
			if tc.wantOnWarn {
				require.Contains(t, warnings(), "probe line")
				require.NotContains(t, infos(), "probe line")
				return
			}
			require.Contains(t, infos(), "probe line")
			require.NotContains(t, warnings(), "probe line")
		})
	}
}
