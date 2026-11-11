package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7077 review. A project config that cannot be read is UNKNOWN, never
// "no project config": P1-2 is what a refresh pays for such a row, P1-3 is
// what a create picks for its default program and account, P2-1 is how the
// lost-restore loop accounts for it.

func unknownProjectConfig(path string) error {
	return &session.ProjectConfigUnknownError{
		Path: path, Attempts: 1,
		Err: fmt.Errorf("read checkout marker for %s: %w", path, context.DeadlineExceeded),
	}
}

// isolateDeferredConfigRows gives the test an empty deferral registry and
// probes it can observe; nothing outlives it.
func isolateDeferredConfigRows(t *testing.T) *[]string {
	t.Helper()
	deferredConfigRows.Lock()
	saved, savedProbes := deferredConfigRows.rows, deferredConfigRows.probes
	deferredConfigRows.rows = map[string]*deferredConfigRow{}
	deferredConfigRows.probes = map[string]bool{}
	deferredConfigRows.Unlock()
	var probes []string
	prevStart, prevPrime, prevLoad := startDeferredConfigProbe, primeRestoreContextForRefresh, fromInstanceDataWithContextForRefresh
	startDeferredConfigProbe = func(func()) { probes = append(probes, "started") }
	t.Cleanup(func() {
		startDeferredConfigProbe, primeRestoreContextForRefresh, fromInstanceDataWithContextForRefresh = prevStart, prevPrime, prevLoad
		deferredConfigRows.Lock()
		deferredConfigRows.rows, deferredConfigRows.probes = saved, savedProbes
		deferredConfigRows.Unlock()
	})
	return &probes
}

// unknownOnLoad makes every load of title report its project config unknown,
// counting the loads.
func unknownOnLoad(t *testing.T, title string) *int {
	t.Helper()
	loads := 0
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		if d.Title == title {
			loads++
			return nil, unknownProjectConfig("/repo/" + title)
		}
		return &session.Instance{ID: d.ID, Title: d.Title}, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })
	return &loads
}

func expireDeferral(t *testing.T, key string) {
	t.Helper()
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	row := deferredConfigRows.rows[key]
	require.NotNil(t, row, "precondition: the row is deferred")
	row.next = time.Now().Add(-time.Second)
}

// P1-2. refreshLocked holds m.mu while it materializes rows, so a row whose
// load reported its project config unknown must not be loaded again on the
// next tick: every caller of that lock would pay the row's config read once
// per tick for as long as the box stays loaded.
func TestRefreshDoesNotReloadARowWhoseProjectConfigIsUnknownOnEveryTick(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	isolateDeferredConfigRows(t)
	loads := unknownOnLoad(t, "slow")
	seedPollRepo(t, "repo-a", "slow", "fine")

	first, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	require.Equal(t, 1, *loads)
	require.NotNil(t, first[daemonInstanceKey("repo-a", "fine")])

	for tick := 0; tick < 5; tick++ {
		next, _, _, _, rerr := refreshDaemonInstances(first)
		require.NoError(t, rerr)
		first = next
	}
	assert.Equal(t, 1, *loads, "a deferred row is skipped without being loaded until its backoff expires")
	assert.Nil(t, first[daemonInstanceKey("repo-a", "slow")], "the row stays unloaded while deferred")
}

// P1-2, the hand-off: once the backoff expires the refresh starts a probe
// instead of loading — the probe reads the config off the lock — and the tick
// after it primed a snapshot loads the row from that snapshot, never through
// the reading loader.
func TestRefreshLoadsADeferredRowFromTheSnapshotItsProbePrimed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	probes := isolateDeferredConfigRows(t)
	loads := unknownOnLoad(t, "slow")
	seedPollRepo(t, "repo-a", "slow")
	key := daemonInstanceKey("repo-a", "slow")

	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	expireDeferral(t, key)

	var probe func()
	startDeferredConfigProbe = func(p func()) { probe = p; *probes = append(*probes, "started") }
	existing, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Len(t, *probes, 1, "an expired deferral starts a probe")
	require.NotNil(t, probe)
	require.Equal(t, 1, *loads, "the refresh does not load the row itself")

	primed := session.NewRestoreContext()
	primeRestoreContextForRefresh = func(string) (*session.RestoreContext, error) { return primed, nil }
	probe() // off the refresh: reads the config and primes the snapshot

	var loadedWith *session.RestoreContext
	fromInstanceDataWithContextForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		loadedWith = rc
		return &session.Instance{ID: d.ID, Title: d.Title}, nil
	}
	refreshed, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.NotNil(t, refreshed[key], "the row loads once its config was read")
	require.Same(t, primed, loadedWith, "the load answers from the primed snapshot")
	require.Equal(t, 1, *loads, "the reading loader is never asked again")

	deferredConfigRows.Lock()
	_, stillDeferred := deferredConfigRows.rows[key]
	deferredConfigRows.Unlock()
	require.False(t, stillDeferred, "a loaded row leaves the deferral registry")
}

// A probe that still cannot read the config backs the row off again — longer —
// and the refresh keeps skipping it without a load.
func TestRefreshBacksOffADeferredRowWhoseProbeStaysUnknown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	isolateDeferredConfigRows(t)
	loads := unknownOnLoad(t, "slow")
	seedPollRepo(t, "repo-a", "slow")
	key := daemonInstanceKey("repo-a", "slow")

	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	expireDeferral(t, key)
	startDeferredConfigProbe = func(p func()) { p() }
	primeRestoreContextForRefresh = func(path string) (*session.RestoreContext, error) {
		return session.NewRestoreContext(), unknownProjectConfig(path)
	}
	_, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)

	deferredConfigRows.Lock()
	row := deferredConfigRows.rows[key]
	deferredConfigRows.Unlock()
	require.NotNil(t, row)
	assert.Equal(t, 2, row.failures)
	assert.True(t, row.next.After(time.Now()), "the row is backed off again")
	assert.Nil(t, row.primed)
	assert.Equal(t, 1, *loads, "an unknown probe never turns into a load under the refresh")
}

// A deferred row that left the disk leaves the registry with it.
func TestRefreshForgetsADeferredRowThatLeftTheDisk(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	isolateDeferredConfigRows(t)
	unknownOnLoad(t, "slow")
	seedPollRepo(t, "repo-a", "slow")

	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	seedPollRepo(t, "repo-a")
	_, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)

	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	assert.Empty(t, deferredConfigRows.rows)
}

// P1-3. The create's default program: a project config that cannot be read is
// refused, never answered with the global default — a project whose
// default_program is codex would otherwise start the global claude with one
// missed probe and nothing to show for it.
func TestCreateDefaultProgramRefusesAnUnknownProjectConfig(t *testing.T) {
	prev := resolveProjectConfigForLaunch
	resolveProjectConfigForLaunch = func(path string) (*config.ResolvedConfig, error) {
		return nil, unknownProjectConfig(path)
	}
	t.Cleanup(func() { resolveProjectConfigForLaunch = prev })

	program, err := defaultProgramFor(tmux.ProgramClaude, "/repo")

	require.Error(t, err, "an unknown project config is not the global default")
	require.True(t, session.IsProjectConfigUnknown(err))
	require.Empty(t, program)
}

// The precedence the create and the pickers share: an unknown is an error to
// every reader, and only a repo with no readable project config gets the
// global default.
func TestDefaultProgramFromResolutionTellsUnknownFromAbsent(t *testing.T) {
	_, err := defaultProgramFromResolution(tmux.ProgramClaude, nil, unknownProjectConfig("/repo"))
	require.Error(t, err)

	program, err := defaultProgramFromResolution(tmux.ProgramClaude, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, tmux.ProgramClaude, program, "no project config: the global default")

	resolved := &config.ResolvedConfig{}
	resolved.DefaultProgram = tmux.ProgramCodex
	program, err = defaultProgramFromResolution(tmux.ProgramClaude, resolved, nil)
	require.NoError(t, err)
	assert.Equal(t, tmux.ProgramCodex, program)
}

// P1-3. The create's default account: the global default_accounts entry must
// not stand in for a project config that could not be read — the project may
// set its own account or opt out of the global one, and scoping the session to
// the global account then runs it as an identity the project turned off.
func TestCreateDefaultAccountRefusesAnUnknownProjectConfig(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prev := checkProjectConfigForLaunch
	checkProjectConfigForLaunch = func(path string) (*config.ResolvedConfig, error) {
		return nil, unknownProjectConfig(path)
	}
	t.Cleanup(func() { checkProjectConfigForLaunch = prev })
	cfg := config.DefaultConfig()
	cfg.DefaultAccounts = map[string]string{tmux.ProgramCodex: "work"}
	req := CreateSessionRequest{Title: "t", RepoPath: "/repo", Program: tmux.ProgramCodex}

	err := applyDefaultAccount(cfg, &req)

	require.Error(t, err, "an unknown project config must not resolve to the global default account")
	require.True(t, session.IsProjectConfigUnknown(err))
	require.Empty(t, req.Account, "no account is scoped from a config that was not read")
}

// P2-1. A restore that failed because the project config could not be read is
// the environment not answering, and its own error says to retry: it waits on
// the bounded deadline share instead of charging the structural budget toward
// a durable give-up.
func TestLostRestoreWaitsOnAnUnknownProjectConfig(t *testing.T) {
	err := fmt.Errorf("recover: session %q: %w", "s", unknownProjectConfig("/repo"))
	assert.Equal(t, lostRestoreEnvDeadline, lostRestoreEnvironmentalClass(err))
	assert.Equal(t, lostRestoreEnvNone, lostRestoreEnvironmentalClass(errors.New("recover: the agent exited")),
		"control: a session defect still charges the structural budget")
}
