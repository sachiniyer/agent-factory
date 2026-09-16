package tmux

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

// The teardown mark (#4472) tracks one fact — af asked to tear down the session
// behind this name — so it may clear only on tmux ANSWERING that a session is
// live there, and must survive every failure that answered nothing. These tests
// pin both halves (Codex on #4473): a refused kill clears it, and a restart or
// restore that has not confirmed a live session keeps it.

// markTestTimeout replaces tmuxCommandTimeout where a probe is wedged; the wedge
// outlasts it by a wide margin so a loaded box cannot blur the two.
const (
	markTestTimeout = 200 * time.Millisecond
	markTestWedge   = 600 * time.Millisecond
)

// teardownMarkTmux is a hermetic tmux: every verb answers from these fields and
// nothing execs.
type teardownMarkTmux struct {
	alive       atomic.Bool // has-session answers "exists"
	killFails   atomic.Bool // kill-session answers a failure instead of removing the session
	captureOK   atomic.Bool // capture-pane succeeds
	probeWedged atomic.Bool // has-session stalls past the shortened deadline
	// nameGen, when set, is what display-message answers for the NAME target:
	// "$id pid created" — the session generation currently behind the name.
	// Unset answers empty, so monitors stay unbound (the pre-binding shape).
	nameGen atomic.Value
	// nameWedged stalls the name-targeted probe past the shortened deadline —
	// a server that never answers confirmedGeneration.
	nameWedged atomic.Bool
	// idGen, when set, is what display-message answers for a $id target:
	// "pid created" — the identity of whatever currently owns that id.
	// Unset answers the missing-id shape real tmux produces: exit 0 with the
	// server-level fields printed and the session-level ones empty
	// ("975304 " — measured on tmux 3.4; a dead id and an empty live server
	// answer identically). That short field list is the probe's determinate
	// "no session at this id" answer.
	idGen atomic.Value
	// idErr, while idErrOn is set, is the identity probe's failure instead of
	// an answer — an injected error for the paths that must stay retryable
	// (an exec-level failure) or corroborate through the session listing (an
	// unclassified exit 1). The gate exists because an atomic.Value cannot
	// be un-stored, and a transient failure's regression IS the recovery.
	idErr   atomic.Value
	idErrOn atomic.Bool
	// idWedged stalls the identity probe past the shortened deadline, so the
	// poll's timeout budget is spent on the probe alone.
	idWedged atomic.Bool
	// idList, when set, is what `tmux ls -F '#{session_id}'` answers — the
	// corroborating session-id listing for an unclassified probe failure.
	idList atomic.Value
	// captureCalls and idProbeCalls count tmux invocations per verb, so a test
	// can assert a wedged identity probe never pays a second timeout budget.
	captureCalls atomic.Int32
	idProbeCalls atomic.Int32
	// duringSetup, if set, runs inside Start's post-confirmation set-option call,
	// between the existence poll and the inner Restore.
	duringSetup func()
}

func (m *teardownMarkTmux) run(c *exec.Cmd) ([]byte, error) {
	args := strings.Join(c.Args, " ")
	switch {
	case strings.Contains(args, "has-session"):
		if m.probeWedged.Load() {
			time.Sleep(markTestWedge)
			return nil, errors.New("wedged tmux server never answered has-session")
		}
		if m.alive.Load() {
			return nil, nil
		}
		return nil, errors.New("can't find session")
	case strings.Contains(args, "kill-session"):
		if m.killFails.Load() {
			return nil, errors.New("cannot kill session")
		}
		m.alive.Store(false)
		return nil, nil
	case strings.Contains(args, "capture-pane"):
		m.captureCalls.Add(1)
		if m.captureOK.Load() {
			return []byte("pane content"), nil
		}
		return nil, errors.New("exit status 1")
	case strings.Contains(args, " ls "):
		if v := m.idList.Load(); v != nil {
			return []byte(v.(string)), nil
		}
		return nil, nil
	case strings.Contains(args, "display-message") && strings.Contains(args, "session_id"):
		// confirmedGeneration's name-targeted bind probe.
		if m.nameWedged.Load() {
			time.Sleep(markTestWedge)
			return nil, errors.New("wedged tmux server never answered the name probe")
		}
		if v := m.nameGen.Load(); v != nil {
			return []byte(v.(string)), nil
		}
		return nil, nil
	case strings.Contains(args, "display-message") && strings.Contains(args, "session_created"):
		// generationMatches' id-targeted identity probe.
		m.idProbeCalls.Add(1)
		if m.idWedged.Load() {
			time.Sleep(markTestWedge)
			return nil, errors.New("wedged tmux server never answered the identity probe")
		}
		if m.idErrOn.Load() {
			return nil, m.idErr.Load().(error)
		}
		if v := m.idGen.Load(); v != nil {
			return []byte(v.(string)), nil
		}
		// The id resolves to nothing: real tmux answers exit 0 with the
		// server-level field filled and the session-level one empty.
		return []byte("1 "), nil
	case strings.Contains(args, "show-options"):
		// importClientEnvironmentArgs: the ordinary first-session case.
		return nil, errors.New("no server running")
	case strings.Contains(args, "history-limit"):
		if m.duringSetup != nil {
			m.duringSetup()
		}
	}
	// list-panes and display-message answer empty: no pane PIDs to reap.
	return nil, nil
}

func (m *teardownMarkTmux) exec() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			_, err := m.run(c)
			return err
		},
		OutputFunc: m.run,
	}
}

// liveOnSpawn stands in for `tmux new-session`: the session exists once the
// launch command has run.
type liveOnSpawn struct {
	inner *MockPtyFactory
	m     *teardownMarkTmux
}

func (f liveOnSpawn) Start(c *exec.Cmd) (*os.File, error) {
	file, err := f.inner.Start(c)
	if err == nil {
		f.m.alive.Store(true)
	}
	return file, err
}

// newMarkedTeardownSession builds a monitored, live session, as the daemon holds
// one before af tears it down.
func newMarkedTeardownSession(t *testing.T) (*TmuxSession, *teardownMarkTmux) {
	t.Helper()
	m := &teardownMarkTmux{}
	m.alive.Store(true)
	m.captureOK.Store(true)
	session := newTmuxSession(toTmuxName("teardown-mark", ""), "claude", liveOnSpawn{NewMockPtyFactory(t), m}, m.exec())
	session.monitor = newStatusMonitor()
	return session, m
}

func captureInfoLog(t *testing.T) *logtest.Buffer {
	t.Helper()
	var buf logtest.Buffer
	prev := aflog.InfoLog.Writer()
	aflog.InfoLog.SetOutput(&buf)
	t.Cleanup(func() { aflog.InfoLog.SetOutput(prev) })
	return &buf
}

// pollVanish makes the session disappear and runs one status-monitor poll,
// returning which level the "going silent" line landed at.
func pollVanish(t *testing.T, session *TmuxSession, m *teardownMarkTmux) (atInfo, atError bool) {
	t.Helper()
	infos := captureInfoLog(t)
	errs := captureErrorLog(t)
	m.alive.Store(false)
	m.captureOK.Store(false)
	m.probeWedged.Store(false)
	session.HasUpdated()
	return strings.Contains(infos.String(), "going silent"), strings.Contains(errs.String(), "going silent")
}

// TestCloseTeardownMarkFollowsTmuxAnswer is the decision table for close():
// only a kill tmux refused AND a probe answering "still live" clears the mark.
func TestCloseTeardownMarkFollowsTmuxAnswer(t *testing.T) {
	cases := []struct {
		name      string
		stage     func(m *teardownMarkTmux)
		wantErr   error
		wantMark  bool
		rationale string
	}{
		{
			name:      "kill succeeds",
			stage:     func(*teardownMarkTmux) {},
			wantMark:  true,
			rationale: "the vanish that follows is the request completing",
		},
		{
			name: "kill refused, session already gone",
			stage: func(m *teardownMarkTmux) {
				m.killFails.Store(true)
				m.alive.Store(false)
			},
			wantMark:  true,
			rationale: "gone is the end state af asked for (#967)",
		},
		{
			name: "kill refused, session still live",
			stage: func(m *teardownMarkTmux) {
				m.killFails.Store(true)
			},
			wantErr:   ErrSessionStillAlive,
			wantMark:  false,
			rationale: "tmux answered that the session survived, so no af request describes it",
		},
		{
			name: "kill refused, probe timed out",
			stage: func(m *teardownMarkTmux) {
				m.killFails.Store(true)
				m.probeWedged.Store(true)
			},
			wantErr:   ErrTmuxTimeout,
			wantMark:  true,
			rationale: "af asked and nothing answered that the request failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortTmuxTimeout(t, markTestTimeout)
			session, m := newMarkedTeardownSession(t)
			tc.stage(m)

			_, err := session.Close()
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
			}
			require.Equal(t, tc.wantMark, session.TeardownInitiated(), tc.rationale)
		})
	}
}

// TestRefusedKillLeavesLaterVanishAtError is the first Codex finding end to
// end: the account swap keeps monitoring a session whose kill tmux refused, so
// its later unrequested vanish must still reach ERROR.
func TestRefusedKillLeavesLaterVanishAtError(t *testing.T) {
	session, m := newMarkedTeardownSession(t)
	m.killFails.Store(true)

	_, err := session.Close()
	require.ErrorIs(t, err, ErrSessionStillAlive)

	atInfo, atError := pollVanish(t, session, m)
	require.True(t, atError, "a live session af failed to kill vanished on its own: that is the anomaly ERROR exists for")
	require.False(t, atInfo)
}

// TestFailedRestartKeepsTeardownAtInfo is the second Codex finding end to end:
// an agent swap closes the session and calls Start on the same object, and a
// Start that fails before any replacement is live must not turn af's own
// teardown into an ERROR on the next poll.
func TestFailedRestartKeepsTeardownAtInfo(t *testing.T) {
	cases := []struct {
		name  string
		stage func(t *testing.T, m *teardownMarkTmux)
		want  error
	}{
		{
			name: "existence probe timed out",
			stage: func(t *testing.T, m *teardownMarkTmux) {
				m.probeWedged.Store(true)
			},
			want: ErrTmuxTimeout,
		},
		{
			name: "environment preparation failed",
			stage: func(t *testing.T, _ *teardownMarkTmux) {
				previous := sessionEnvExecutable
				sessionEnvExecutable = func() (string, error) { return "", errors.New("no executable") }
				t.Cleanup(func() { sessionEnvExecutable = previous })
			},
			want: ErrSessionNotStarted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortTmuxTimeout(t, markTestTimeout)
			session, m := newMarkedTeardownSession(t)
			_, err := session.Close()
			require.NoError(t, err)

			tc.stage(t, m)
			require.ErrorIs(t, session.Start(t.TempDir()), tc.want)
			require.True(t, session.TeardownInitiated(),
				"no live replacement was confirmed, so the mark still describes the session af closed")

			atInfo, atError := pollVanish(t, session, m)
			require.True(t, atInfo)
			require.False(t, atError, "af closed this session itself; its disappearance is not an ERROR")
		})
	}
}

// The other half: once tmux answers that a session is live behind the name, the
// mark no longer describes it. Top-level tests, not subtests: MockPtyFactory
// builds a file path from t.Name(), which a subtest's "/" breaks.

// TestStartNameTakenClearsTeardownMark: Start's gate finding the name live.
func TestStartNameTakenClearsTeardownMark(t *testing.T) {
	session, m := newMarkedTeardownSession(t)
	_, err := session.Close()
	require.NoError(t, err)

	m.alive.Store(true) // recreated behind af's back
	require.ErrorIs(t, session.Start(t.TempDir()), ErrSessionNotStarted)
	require.False(t, session.TeardownInitiated(), "a live session af did not just close holds the name")

	_, atError := pollVanish(t, session, m)
	require.True(t, atError)
}

// TestStartConfirmedReplacementKeepsOldMonitorMark pins the window between the
// existence poll confirming the replacement and the inner Restore installing
// the fresh monitor: the OLD monitor keeps its mark through that window, so an
// in-flight poll of the session af closed still reads its own generation's
// attribution (Codex on #4473) — while the fresh monitor the replacement is
// polled on starts unmarked.
func TestStartConfirmedReplacementKeepsOldMonitorMark(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	forceSessionEnvExecutable(t, "/test/af")
	session, m := newMarkedTeardownSession(t)
	_, err := session.Close()
	require.NoError(t, err)

	var setupRan, markDuringSetup atomic.Bool
	m.duringSetup = func() {
		setupRan.Store(true)
		markDuringSetup.Store(session.TeardownInitiated())
	}

	require.NoError(t, session.Start(t.TempDir()))
	require.True(t, setupRan.Load(), "the observation point must actually run")
	require.True(t, markDuringSetup.Load(),
		"the old monitor must keep its mark until the swap — clearing it here is the in-flight-poll race Codex found")
	require.False(t, session.TeardownInitiated(), "the fresh monitor for the confirmed replacement starts unmarked")
}

// TestRestoreKeepsTeardownMarkOnUnansweredProbe pins the third site with the
// same shape: RestoreWithResult rebinds on "exists OR unknown", and only an
// answered probe is evidence of a live session. The answered case is
// TestHasUpdatedExpectedTeardownLogsInfo's restore step.
func TestRestoreKeepsTeardownMarkOnUnansweredProbe(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	session, m := newMarkedTeardownSession(t)
	_, err := session.Close()
	require.NoError(t, err)

	m.probeWedged.Store(true)
	result, err := session.RestoreWithResult("/some/work/dir")
	require.NoError(t, err)
	require.Equal(t, RestoreReattached, result, "a wedged probe still rebinds rather than respawning (#1962)")
	require.True(t, session.TeardownInitiated(), "a timed-out probe is not evidence of a live session")

	atInfo, atError := pollVanish(t, session, m)
	require.True(t, atInfo)
	require.False(t, atError, "af closed this session itself; a wedged probe in between changes nothing")
}

// TestSurvivedTeardownRetiresMarkOnProvenLiveness is the first Codex finding's
// remaining half: a close() that returns while its session demonstrably
// survived must not leave the mark standing, or the session's LATER unrelated
// vanish is misattributed to a teardown that never happened. A capture that
// began after the request settled and still succeeded is that proof — and it
// is deliberately NOT just "the next successful poll": one that straddled the
// close proves nothing about the request's outcome.
func TestSurvivedTeardownRetiresMarkOnProvenLiveness(t *testing.T) {
	session, m := newMarkedTeardownSession(t)

	// af asks for the teardown, and close() returns with the request settled —
	// the shape a timed-out kill leaves behind when tmux recovers and the
	// session kept running.
	_, err := session.Close()
	require.NoError(t, err)
	require.True(t, session.TeardownInitiated())

	// A poll that began after the request settled captures successfully —
	// proof the session outlived af's request — so the mark retires.
	m.alive.Store(true)
	m.captureOK.Store(true)
	session.HasUpdated()
	require.False(t, session.TeardownInitiated(),
		"post-settle liveness proves the teardown did not take; the mark must retire")

	// The session then dies on its own — unrelated to the request that
	// failed — and the monitor must say so at ERROR.
	atInfo, atError := pollVanish(t, session, m)
	require.True(t, atError)
	require.False(t, atInfo)
}

// TestClosedConclusivelyLiveAgainClearsTeardownMark: the account swap's skip
// check finding the name live again retires the mark with the latch.
func TestClosedConclusivelyLiveAgainClearsTeardownMark(t *testing.T) {
	session, _ := newMarkedTeardownSession(t)
	session.setClosedConclusively(true)
	session.markTeardownInitiated()

	require.False(t, session.ClosedConclusivelyAndStillAbsent())
	require.False(t, session.ClosedConclusively())
	require.False(t, session.TeardownInitiated(), "the name is live again and af has not asked for it")
}
