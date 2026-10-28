package session

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The relaunch route rides the same envelope as every other control call, and
// CanRelaunch reads the /v1/health feature token — so an older sandbox build
// that lacks the token is never sent a route it does not have.
func TestRemoteAgentServer_RelaunchWire(t *testing.T) {
	srv := newTabWireServer(t).
		route("/v1/agent/relaunch", map[string]any{"ok": true}).
		route("/v1/health", map[string]any{"ok": true, "version": "x", "features": []string{"tab-api", RelaunchFeature}})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	as := &remoteAgentServer{rc: srv.client(t, httpSrv.URL)}

	can, err := as.CanRelaunch()
	require.NoError(t, err)
	assert.True(t, can)

	require.NoError(t, as.Relaunch())
	assert.Equal(t, "POST", srv.methods["/v1/agent/relaunch"])
	srv.assertAuth(t, "/v1/agent/relaunch")
}

func TestRemoteAgentServer_CanRelaunchFalseWithoutTheFeature(t *testing.T) {
	srv := newTabWireServer(t).
		route("/v1/health", map[string]any{"ok": true, "version": "old", "features": []string{"tab-api"}})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	as := &remoteAgentServer{rc: srv.client(t, httpSrv.URL)}
	can, err := as.CanRelaunch()
	require.NoError(t, err)
	assert.False(t, can, "an agent-server that does not advertise relaunch must not be sent the route")
}

func TestRemoteAgentServer_CanRelaunchSurfacesUnreachable(t *testing.T) {
	srv := newTabWireServer(t)
	httpSrv := httptest.NewServer(srv.mux)
	url := httpSrv.URL
	httpSrv.Close() // nothing answers: unknown, not "no"
	as := &remoteAgentServer{rc: srv.client(t, url)}
	_, err := as.CanRelaunch()
	assert.Error(t, err, "an unanswered probe must stay distinguishable from 'cannot relaunch'")
}

func TestDeadRemoteAgentServerCannotRelaunch(t *testing.T) {
	as := &deadRemoteAgentServer{title: "t"}
	can, err := as.CanRelaunch()
	assert.False(t, can)
	assert.ErrorIs(t, err, ErrRemoteSandboxNotProvisioned)
	assert.ErrorIs(t, as.Relaunch(), ErrRemoteSandboxNotProvisioned)
}

// Re-provisioning after a failed relaunch is reserved for "cannot relaunch" and
// "no sandbox". A reachable sandbox that refused, or a transport failure, still
// holds the work and must never be replaced.
func TestRelaunchFallsBackToReprovisionOnlyWhenTheSandboxIsAbsentOrOld(t *testing.T) {
	assert.True(t, relaunchFallsBackToReprovision(ErrRelaunchUnsupported))
	assert.True(t, relaunchFallsBackToReprovision(fmt.Errorf("wrapped: %w", ErrRemoteSandboxNotProvisioned)))
	assert.False(t, relaunchFallsBackToReprovision(errors.New("connection refused")))
	assert.False(t, relaunchFallsBackToReprovision(errors.New("relaunch: cannot confirm the agent stopped")))
	assert.False(t, relaunchFallsBackToReprovision(nil))
}

// Two relaunch requests racing — the daemon timed out and retried while the
// first was still running — must run ONE stop/start, not two. The second joins
// the first and gets its result.
func TestRelaunchRequestsRaceOntoOneAttempt(t *testing.T) {
	srv := &localAgentServer{}
	var runs atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	run := func() error {
		if runs.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil
	}
	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(1)
	go func() { defer wg.Done(); results[0] = srv.relaunchWith(run) }()
	<-started // the first is mid-flight: its stop/start has begun
	wg.Add(1)
	go func() { defer wg.Done(); results[1] = srv.relaunchWith(run) }()
	time.Sleep(50 * time.Millisecond) // let the second arrive and join
	close(release)
	wg.Wait()
	assert.EqualValues(t, 1, runs.Load(), "overlapping relaunches must collapse onto one attempt")
	assert.NoError(t, results[0])
	assert.NoError(t, results[1])

	// A request after it finished is a new relaunch, not a stale join.
	require.NoError(t, srv.relaunchWith(func() error { runs.Add(1); return nil }))
	assert.EqualValues(t, 2, runs.Load())
}

func TestRelaunchJoinerSharesALeaderFailureAndNeverStartsASecond(t *testing.T) {
	srv := &localAgentServer{}
	var runs atomic.Int32
	release, started := make(chan struct{}), make(chan struct{})
	boom := errors.New("cannot confirm the agent stopped")
	leader := make(chan error, 1)
	go func() {
		leader <- srv.relaunchWith(func() error { runs.Add(1); close(started); <-release; return boom })
	}()
	<-started
	joined := make(chan error, 1)
	go func() { joined <- srv.relaunchWith(func() error { runs.Add(1); return nil }) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	assert.ErrorIs(t, <-leader, boom)
	assert.ErrorIs(t, <-joined, boom)
	assert.EqualValues(t, 1, runs.Load())
}

type fakeStopper struct {
	state tmux.PaneState
	blind bool
	err   error
	calls int
}

func (f *fakeStopper) CloseAndWaitForPaneExitReportingBlindness() (tmux.PaneState, bool, error) {
	f.calls++
	return f.state, f.blind, f.err
}

// The agent's tmux session is gone but a child it spawned survives: the stop's
// survivor sweep refuses, and the relaunch must NOT start a replacement beside
// it. The stop has to run for a session that is already dead — skipping it
// because the agent "is not alive" is exactly what let that child through.
func TestRelaunchStopsADeadSessionAndRefusesWhenAChildSurvives(t *testing.T) {
	survivor := &fakeStopper{state: tmux.PaneStateUnknown, blind: true,
		err: errors.New("pane processes 4242 are still alive after bounded teardown")}
	respawned := 0
	err := stopThenRespawn("t", survivor, false, func() error { respawned++; return nil })
	require.Error(t, err)
	assert.Equal(t, 1, survivor.calls, "the stop (and its survivor sweep) must run even though the session is gone")
	assert.Zero(t, respawned, "no replacement may start beside a surviving child")
}

func TestRelaunchOfADeadSessionWithNoSurvivorsRespawns(t *testing.T) {
	clean := &fakeStopper{state: tmux.PaneStateKnown, blind: true} // vanished, sweep found nothing
	respawned := 0
	require.NoError(t, stopThenRespawn("t", clean, false, func() error { respawned++; return nil }))
	assert.Equal(t, 1, clean.calls)
	assert.Equal(t, 1, respawned)
}

func TestRelaunchOfARunningAgentRefusesAnUnobservedVanish(t *testing.T) {
	vanished := &fakeStopper{state: tmux.PaneStateKnown, blind: true}
	respawned := 0
	err := stopThenRespawn("t", vanished, true, func() error { respawned++; return nil })
	require.ErrorContains(t, err, "detached child")
	assert.Zero(t, respawned)

	stuck := &fakeStopper{state: tmux.PaneStateKnown, err: errors.New("kill failed")}
	require.Error(t, stopThenRespawn("t", stuck, true, func() error { respawned++; return nil }))
	assert.Zero(t, respawned)
}

// --force-reap asks for the sandbox to be replaced. Relaunching instead would keep
// the work the operator asked to discard — and against an unreachable sandbox the
// relaunch's own probe would fail before the replacement ever ran.
func TestForceReapBypassesRelaunch(t *testing.T) {
	inst := &Instance{Title: "t"}
	inst.SetForceSandboxReplacement(true)
	relaunched, replaced := 0, 0
	err := decideRecovery(inst,
		func(*Instance) error { relaunched++; return errors.New("health probe failed") },
		func(*Instance) error { replaced++; return nil })
	require.NoError(t, err)
	assert.Zero(t, relaunched, "a forced replacement must not try the relaunch first")
	assert.Equal(t, 1, replaced)
}

func TestWithoutForceAnUnreachableSandboxIsNotReplaced(t *testing.T) {
	inst := &Instance{Title: "t"}
	replaced := 0
	err := decideRecovery(inst,
		func(*Instance) error { return errors.New("connection refused") },
		func(*Instance) error { replaced++; return nil })
	require.Error(t, err)
	assert.Zero(t, replaced)

	err = decideRecovery(inst,
		func(*Instance) error { return ErrRelaunchUnsupported },
		func(*Instance) error { replaced++; return nil })
	require.NoError(t, err)
	assert.Equal(t, 1, replaced, "an older sandbox falls back to replacement")
}
