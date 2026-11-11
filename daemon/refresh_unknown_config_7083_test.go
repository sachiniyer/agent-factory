package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// #7083 review, P3-4: a deferred row whose repo stops being listed is kept
// only for a while, and a primed snapshot is only used while it is fresh — so
// a row whose repo comes back never loads from a config read long ago.

func TestDeferredRowOfARepoNoLongerListedIsForgottenAfterAWhile(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	isolateDeferredConfigRows(t)
	unknownOnLoadForAll(t)
	seedRows(t, "repo-a", deferredRow("slow", "/projects/alpha"))
	key := daemonInstanceKey("repo-a", "slow")
	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)

	prevLoad := loadAllRepoInstancesForRefresh
	loadAllRepoInstancesForRefresh = func() (config.RepoInstancesPollResult, error) {
		result, lerr := prevLoad()
		delete(result.Instances, "repo-a") // the repo is not listed any more
		return result, lerr
	}
	t.Cleanup(func() { loadAllRepoInstancesForRefresh = prevLoad })

	_, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	deferredConfigRows.Lock()
	_, kept := deferredConfigRows.rows[key]
	deferredConfigRows.rows[key].seenAt = time.Now().Add(-deferredRowForgetAfter - time.Minute)
	deferredConfigRows.Unlock()
	require.True(t, kept, "precondition: one unlisted tick keeps the row")

	_, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	assert.NotContains(t, deferredConfigRows.rows, key, "a row unlisted for longer than deferredRowForgetAfter is dropped")
}

func TestAStalePrimedSnapshotIsReprobedNotLoaded(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	isolateDeferredConfigRows(t)
	unknownOnLoadForAll(t)
	seedRows(t, "repo-a", deferredRow("slow", "/projects/alpha"))
	key := daemonInstanceKey("repo-a", "slow")
	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)

	expireAllDeferrals(t)
	probes := 0
	primeRestoreContextForRefresh = func(string) (*session.RestoreContext, error) {
		return session.NewRestoreContext(), nil
	}
	loaded := 0
	fromInstanceDataWithContextForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		loaded++
		return &session.Instance{ID: d.ID, Title: d.Title}, nil
	}
	// The inline probe primes the row; age that snapshot past the cap — in
	// refresh passes (#7087 review) — before the pass that would load it.
	startDeferredConfigProbe = func(p func()) {
		probes++
		p()
		deferredConfigRows.Lock()
		if row := deferredConfigRows.rows[key]; row != nil && probes == 1 {
			row.primedTick = 0                   // primed long ago, in passes
			row.primed, row.probing = nil, false // hold the load for the next pass
			row.next = time.Now().Add(time.Hour)
		}
		deferredConfigRows.Unlock()
	}
	existing, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Equal(t, 1, probes)

	deferredConfigRows.Lock()
	row := deferredConfigRows.rows[key]
	// Older than deferredPrimedMaxPasses passes by the next one.
	row.primed, row.primedTick = session.NewRestoreContext(), 0
	deferredConfigRows.Unlock()

	_, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	assert.Equal(t, 2, probes, "a snapshot older than deferredPrimedMaxPasses passes is probed again")
	assert.Equal(t, 1, loaded, "and the row loads from the fresh one, never the stale one")
}
