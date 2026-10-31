package tmux

import (
	"errors"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

// reattachRefreshTmux is a hermetic tmux mock for the reattach-refresh gating
// tests. It answers the generation probe and counts every set-environment and
// set-option command the refresh issues, so a test can assert the refresh ran
// (or was skipped) for a given (answered, confirmedFresh) pair. setEnvFails,
// when set, makes every set-environment and set-option command return an
// error so the error-wrapping path can be exercised.
type reattachRefreshTmux struct {
	nameGen        atomic.Value // "$id pid created" for the generation probe
	setEnvCalls    atomic.Int32
	setOptionCalls atomic.Int32
	setEnvFails    atomic.Bool
}

func (m *reattachRefreshTmux) run(c *exec.Cmd) ([]byte, error) {
	args := strings.Join(c.Args, " ")
	switch {
	case strings.Contains(args, "display-message") && strings.Contains(args, "session_id"):
		// confirmedGeneration's name-targeted bind probe.
		if v := m.nameGen.Load(); v != nil {
			return []byte(v.(string)), nil
		}
		return nil, nil
	case strings.Contains(args, "set-environment"):
		m.setEnvCalls.Add(1)
		if m.setEnvFails.Load() {
			return nil, errors.New("set-environment refused")
		}
		return nil, nil
	case strings.Contains(args, "set-option"):
		m.setOptionCalls.Add(1)
		if m.setEnvFails.Load() {
			return nil, errors.New("set-option refused")
		}
		return nil, nil
	}
	return nil, nil
}

func (m *reattachRefreshTmux) exec() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			_, err := m.run(c)
			return err
		},
		OutputFunc: m.run,
	}
}

// accountScopedRefreshSession builds a session whose account is registered and
// whose tmux interactions are mock-backed, so reattach's refresh path can be
// exercised without a real tmux server.
func accountScopedRefreshSession(t *testing.T, m *reattachRefreshTmux) *TmuxSession {
	t.Helper()
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)
	// These tests model the daemon's restore, the one process that writes a
	// session's default-command (#4731).
	forceLaunchCapabilityOwner(t, true)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	if _, err := agentaccount.Register(home, ProgramCodex, "work"); err != nil {
		t.Fatalf("register fixture account: %v", err)
	}
	session := newTmuxSession(toTmuxName("reattach-refresh", ""), ProgramCodex, NewMockPtyFactory(t), m.exec())
	session.SetAccountForAgent(ProgramCodex, "work")
	return session
}

// TestReattachSkipsAccountRefreshOnConfirmedFresh is the Start inner-attach
// finding: when confirmedFresh is true, Start's new-session queue already wrote
// the -e session-env entries and default-command atomically, so the refresh is
// pure redundancy. A single set-environment failure there used to make Start
// call t.Close() and destroy the freshly created, healthy pane. The refresh must
// not run at all on this path, even when the probe answered.
func TestReattachSkipsAccountRefreshOnConfirmedFresh(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	m := &reattachRefreshTmux{}
	m.nameGen.Store("$5 111 222")
	session := accountScopedRefreshSession(t, m)

	require.NoError(t, session.reattach(reattachOptions{answered: true, confirmedFresh: true, refreshAccount: true, baseline: true}))
	require.Zero(t, m.setEnvCalls.Load(),
		"the refresh must not issue set-environment on a confirmedFresh reattach — Start's queue already wrote them")
	require.Zero(t, m.setOptionCalls.Load(),
		"the refresh must not issue set-option default-command on a confirmedFresh reattach")
}

// TestReattachSkipsAccountRefreshOnUnansweredProbe is the wedged-probe finding:
// when the existence probe did not answer, the package's own documented
// discipline promises "one bounded has-session, not two." The refresh's first
// set-environment would pay a full tmuxCommandTimeout for the same non-answer,
// twelve times over. The refresh must not run on this path.
func TestReattachSkipsAccountRefreshOnUnansweredProbe(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	m := &reattachRefreshTmux{}
	session := accountScopedRefreshSession(t, m)

	require.NoError(t, session.reattach(reattachOptions{refreshAccount: true, baseline: true}))
	require.Zero(t, m.setEnvCalls.Load(),
		"the refresh must not issue set-environment when the probe went unanswered — a wedged server already spent one timeout")
	require.Zero(t, m.setOptionCalls.Load(),
		"the refresh must not issue set-option when the probe went unanswered")
}

// TestReattachRunsAccountRefreshOnAnsweredNonFresh is the legitimate-heal
// finding: when the probe answered and the session is not fresh (a restore of a
// pre-existing live session whose boundary environment predates or lost its
// scope), the refresh must still run. This is the use case
// TestAccountScopedShellTabInheritsSelectedCredentials pins end-to-end and
// TestLoad_AccountAgentEnvironmentRefreshFailureRetainsHandle pins the error
// contract for — both on the answered==true, confirmedFresh==false path.
func TestReattachRunsAccountRefreshOnAnsweredNonFresh(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	m := &reattachRefreshTmux{}
	m.nameGen.Store("$5 111 222")
	session := accountScopedRefreshSession(t, m)

	require.NoError(t, session.reattach(reattachOptions{answered: true, refreshAccount: true, baseline: true}))
	require.Positive(t, m.setEnvCalls.Load(),
		"the refresh must issue set-environment on an answered, non-fresh reattach — this is the heal path")
	require.Positive(t, m.setOptionCalls.Load(),
		"the refresh must issue set-option default-command on an answered, non-fresh reattach")
}

// TestReattachAnsweredNonFreshRefreshFailureWrapsError verifies the error
// contract is preserved on the one path the refresh still runs: a set-environment
// failure must surface as ErrAccountEnvironmentRefresh so the backend's
// retainsInertInstance keeps the live agent handle.
func TestReattachAnsweredNonFreshRefreshFailureWrapsError(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	m := &reattachRefreshTmux{}
	m.nameGen.Store("$5 111 222")
	m.setEnvFails.Store(true)
	session := accountScopedRefreshSession(t, m)

	err := session.reattach(reattachOptions{answered: true, refreshAccount: true, baseline: true})
	require.ErrorIs(t, err, ErrAccountEnvironmentRefresh)
}

// TestReattachOnlyUnansweredSkipsAccountRefresh verifies the public ReattachOnly
// API on the wedged-probe path: ReattachOnly(workDir, false) is the process-tab
// rebind whose doc promises "one bounded has-session, not two." The refresh must
// not add a second deadline.
func TestReattachOnlyUnansweredSkipsAccountRefresh(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	m := &reattachRefreshTmux{}
	session := accountScopedRefreshSession(t, m)

	require.NoError(t, session.ReattachOnly(t.TempDir(), false))
	require.Zero(t, m.setEnvCalls.Load(),
		"ReattachOnly(false) must not issue set-environment — the doc promises one bounded has-session, not two")
	require.Zero(t, m.setOptionCalls.Load())
}

// TestReattachOnlyAnsweredRunsAccountRefresh verifies the public ReattachOnly API
// on the answered path: ReattachOnly(workDir, true) is a rebind of a live session
// whose boundary environment may need healing, so the refresh must still run.
func TestReattachOnlyAnsweredRunsAccountRefresh(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	m := &reattachRefreshTmux{}
	m.nameGen.Store("$5 111 222")
	session := accountScopedRefreshSession(t, m)

	require.NoError(t, session.ReattachOnly(t.TempDir(), true))
	require.Positive(t, m.setEnvCalls.Load(),
		"ReattachOnly(true) must issue set-environment — this is the heal path for a live session")
	require.Positive(t, m.setOptionCalls.Load())
}

// ReattachInert is the load path of a worktree-missing row (#5102): it rebinds
// a live pane and nothing else, so a refresh that would fail cannot fail it —
// the row stays archivable rather than loading startup-unknown.
func TestReattachInertSkipsAccountRefresh(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	m := &reattachRefreshTmux{}
	m.nameGen.Store("$5 111 222")
	m.setEnvFails.Store(true)
	session := accountScopedRefreshSession(t, m)

	require.NoError(t, session.ReattachInert())
	require.Zero(t, m.setEnvCalls.Load(), "an inert reattach must not refresh the account environment")
	require.Zero(t, m.setOptionCalls.Load(), "nor rewrite default-command")
}
