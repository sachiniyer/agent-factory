package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// #7113: a refresh loads a deferred row only from a snapshot read within
// deferredPrimedMaxPasses refresh passes, never from one read before a dark
// stretch, whatever shape the stretch takes. Two shapes reach the poll:
//
//   - the repo is not listed: its directory is gone, so the loader leaves it
//     out and the pass runs on (#7083 review);
//   - the load aborts: an instances.json that exists but cannot be read fails
//     the whole load (config.RepoInstancesFileCache.LoadAll), so the refresh
//     returns before materializing any row. The poll still runs it every
//     daemon_poll_interval, so it is a pass like any other, and it ages the
//     snapshot the same way.
//
// The probe runs off the refresh, as in production: it answers after the pass
// that started it, so the snapshot is primed before the stretch begins.
func TestAPrimedSnapshotFromBeforeADarkStretchIsReprobedNotLoaded(t *testing.T) {
	stretch := int(deferredPrimedMaxPasses) + 3
	for _, dark := range []struct {
		name string
		load func(prev func() (config.RepoInstancesPollResult, error)) (config.RepoInstancesPollResult, error)
	}{
		{"the repo is not listed", func(prev func() (config.RepoInstancesPollResult, error)) (config.RepoInstancesPollResult, error) {
			result, err := prev()
			delete(result.Instances, "repo-a")
			return result, err
		}},
		{"the load aborts", func(func() (config.RepoInstancesPollResult, error)) (config.RepoInstancesPollResult, error) {
			return config.RepoInstancesPollResult{}, errors.New("failed to migrate instances for repo repo-a: failed to read repo instances: permission denied")
		}},
	} {
		t.Run(dark.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			_ = captureWarnings(t)
			isolateDeferredConfigRows(t)
			unknownOnLoadForAll(t)
			seedRows(t, "repo-a", deferredRow("slow", "/projects/alpha"))
			key := daemonInstanceKey("repo-a", "slow")
			existing, _, _, _, err := refreshDaemonInstances(nil)
			require.NoError(t, err)
			expireAllDeferrals(t)

			var probes []func()
			startDeferredConfigProbe = func(p func()) { probes = append(probes, p) }
			var primed []*session.RestoreContext
			primeRestoreContextForRefresh = func(string) (*session.RestoreContext, error) {
				rc := session.NewRestoreContext()
				primed = append(primed, rc)
				return rc, nil
			}
			var loadedWith []*session.RestoreContext
			fromInstanceDataWithContextForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
				loadedWith = append(loadedWith, rc)
				return &session.Instance{ID: d.ID, Title: d.Title}, nil
			}

			existing, _, _, _, err = refreshDaemonInstances(existing)
			require.NoError(t, err)
			require.Len(t, probes, 1, "precondition: the expired deferral starts a probe")
			probes[0]() // answers after its pass, priming the snapshot
			require.Len(t, primed, 1)
			stale := primed[0]

			prevLoad := loadAllRepoInstancesForRefresh
			loadAllRepoInstancesForRefresh = func() (config.RepoInstancesPollResult, error) { return dark.load(prevLoad) }
			t.Cleanup(func() { loadAllRepoInstancesForRefresh = prevLoad })
			for pass := 0; pass < stretch; pass++ {
				if next, _, _, _, rerr := refreshDaemonInstances(existing); rerr == nil {
					existing = next
				}
			}
			require.Empty(t, loadedWith, "precondition: the row did not load during the stretch")
			loadAllRepoInstancesForRefresh = prevLoad

			refreshed, _, _, _, err := refreshDaemonInstances(existing)
			require.NoError(t, err)
			for _, rc := range loadedWith {
				require.NotSame(t, stale, rc, "a row whose repo was dark for %d passes never loads from the snapshot primed before them", stretch)
			}
			require.Len(t, probes, 2, "the snapshot primed before the stretch is probed again")
			require.Nil(t, refreshed[key], "and the row waits for that probe rather than loading")

			probes[1]()
			require.Len(t, primed, 2)
			refreshed, _, _, _, err = refreshDaemonInstances(refreshed)
			require.NoError(t, err)
			require.NotNil(t, refreshed[key], "the row loads once the fresh probe answers")
			require.Len(t, loadedWith, 1)
			assert.Same(t, primed[1], loadedWith[0], "from the fresh snapshot")
		})
	}
}
