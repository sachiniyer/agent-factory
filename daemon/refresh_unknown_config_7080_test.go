package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// #7080 review of the deferred-row mechanism (#7078): the probe must run off
// the refresh, rows of one project share a probe, probes are capped, a repo
// the pass did not read keeps its rows, and a deferred row's warnings do not
// repeat on every tick.

// seedRows writes rows verbatim as repoID's instances.json.
func seedRows(t *testing.T, repoID string, rows ...session.InstanceData) {
	t.Helper()
	if rows == nil {
		rows = []session.InstanceData{}
	}
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, raw))
}

func deferredRow(title, path string) session.InstanceData {
	return session.InstanceData{ID: session.NewInstanceID(), Title: title, Path: path, Liveness: session.LiveArchived}
}

// unknownOnLoadForAll makes every load report its project config unknown.
func unknownOnLoadForAll(t *testing.T) *int {
	t.Helper()
	loads := 0
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		loads++
		return nil, unknownProjectConfig(d.Path)
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })
	return &loads
}

func expireAllDeferrals(t *testing.T) {
	t.Helper()
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	for _, row := range deferredConfigRows.rows {
		row.next = time.Now().Add(-time.Second)
	}
}

// The deferral's whole point is that the probe's launch-budget config read is
// not paid where the refresh runs — under m.mu. With the production starter,
// a refresh whose probe is still reading must return without waiting for it.
// A probe run inline would hold the refresh (and so the lock) for its whole read.
func TestDeferredConfigProbeNeverRunsUnderTheRefresh(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	production := startDeferredConfigProbe
	isolateDeferredConfigRows(t)
	startDeferredConfigProbe = production
	unknownOnLoad(t, "slow")
	seedPollRepo(t, "repo-a", "slow")

	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	expireDeferral(t, daemonInstanceKey("repo-a", "slow"))

	probing := make(chan struct{})
	release := make(chan struct{})
	primeRestoreContextForRefresh = func(string) (*session.RestoreContext, error) {
		close(probing)
		<-release // the box is not answering: the read is still in flight
		return session.NewRestoreContext(), nil
	}
	t.Cleanup(func() {
		close(release)
		// Let the probe finish before the registry is restored under it.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			deferredConfigRows.Lock()
			inFlight := len(deferredConfigRows.probes)
			deferredConfigRows.Unlock()
			if inFlight == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("the released probe never finished")
	})

	returned := make(chan struct{})
	go func() {
		_, _, _, _, _ = refreshDaemonInstances(existing)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh waited on the deferred row's config probe: the probe ran under the refresh's lock")
	}
	select {
	case <-probing:
	case <-time.After(10 * time.Second):
		t.Fatal("precondition: the due row's probe started")
	}
}

// Rows of one project share one probe and its primed snapshot.
func TestDeferredRowsOfOneProjectShareAProbe(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	probes := isolateDeferredConfigRows(t)
	unknownOnLoadForAll(t)
	seedRows(t, "repo-a", deferredRow("one", "/projects/alpha"), deferredRow("two", "/projects/alpha"))

	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	expireAllDeferrals(t)
	var started []func()
	startDeferredConfigProbe = func(p func()) { started = append(started, p); *probes = append(*probes, "started") }
	existing, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Len(t, started, 1, "two rows of one project start one probe")

	primed := session.NewRestoreContext()
	var primedPaths []string
	primeRestoreContextForRefresh = func(path string) (*session.RestoreContext, error) {
		primedPaths = append(primedPaths, path)
		return primed, nil
	}
	started[0]()
	require.Equal(t, []string{"/projects/alpha"}, primedPaths)

	var loadedWith []*session.RestoreContext
	fromInstanceDataWithContextForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		loadedWith = append(loadedWith, rc)
		return &session.Instance{ID: d.ID, Title: d.Title}, nil
	}
	refreshed, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	assert.NotNil(t, refreshed[daemonInstanceKey("repo-a", "one")])
	assert.NotNil(t, refreshed[daemonInstanceKey("repo-a", "two")])
	require.Len(t, loadedWith, 2)
	assert.Same(t, primed, loadedWith[0])
	assert.Same(t, primed, loadedWith[1], "both rows load from the one primed snapshot")
}

// At most maxDeferredConfigProbes projects are probed at once; the rest stay
// due and start once a slot frees.
func TestDeferredConfigProbesAreCapped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	isolateDeferredConfigRows(t)
	unknownOnLoadForAll(t)
	var rows []session.InstanceData
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		rows = append(rows, deferredRow(name, "/projects/"+name))
	}
	seedRows(t, "repo-a", rows...)

	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	expireAllDeferrals(t)
	var started []func()
	startDeferredConfigProbe = func(p func()) { started = append(started, p) }
	existing, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.Len(t, started, maxDeferredConfigProbes, "a load window does not start one probe per project at once")

	primeRestoreContextForRefresh = func(path string) (*session.RestoreContext, error) {
		return session.NewRestoreContext(), unknownProjectConfig(path)
	}
	started[0]() // one probe finishes (still unknown) and frees its slot
	_, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	assert.Len(t, started, maxDeferredConfigProbes+1, "a still-due project takes the freed slot")
}

// A repo the pass could not read keeps its deferred rows and their backoff; a
// repo it read without the row drops it.
func TestDeferredRowsSurviveATickTheirRepoWasNotRead(t *testing.T) {
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
		delete(result.Instances, "repo-a") // its instances.json could not be read this tick
		return result, lerr
	}
	_, _, _, _, err = refreshDaemonInstances(existing)
	loadAllRepoInstancesForRefresh = prevLoad
	require.NoError(t, err)
	deferredConfigRows.Lock()
	row := deferredConfigRows.rows[key]
	deferredConfigRows.Unlock()
	require.NotNil(t, row, "an unread repo proves nothing about its rows")
	assert.Equal(t, 1, row.failures, "and its backoff is not reset")

	seedRows(t, "repo-a")
	_, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	deferredConfigRows.Lock()
	defer deferredConfigRows.Unlock()
	assert.Empty(t, deferredConfigRows.rows, "a repo read without the row drops it")
}

// A record that moved while deferred is probed at its current path.
func TestDeferredRowFollowsItsRecordsPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	isolateDeferredConfigRows(t)
	unknownOnLoadForAll(t)
	row := deferredRow("moved", "/projects/old")
	seedRows(t, "repo-a", row)
	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)

	row.Path = "/projects/new"
	seedRows(t, "repo-a", row)
	expireAllDeferrals(t)
	var probed []string
	startDeferredConfigProbe = func(p func()) { p() }
	primeRestoreContextForRefresh = func(path string) (*session.RestoreContext, error) {
		probed = append(probed, path)
		return session.NewRestoreContext(), unknownProjectConfig(path)
	}
	_, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	assert.Equal(t, []string{"/projects/new"}, probed)
}

// A deferred row recovers on its own: its owed-teardown and task-slot warnings
// say so once, and do not repeat — or advise a repair — on every tick.
func TestDeferredRowWarnsOnceAndNotAboutRepair(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	warnings := captureWarnings(t)
	isolateDeferredConfigRows(t)
	unknownOnLoadForAll(t)
	row := deferredRow("owed", "/projects/alpha")
	row.TaskID = "task-1"
	row.TaskRunActive = true
	row.PendingOnComplete = &session.PendingOnCompleteData{TaskID: "task-1", FiledAt: time.Now()}
	seedRows(t, "repo-a", row)

	existing, ghosts, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)
	for tick := 0; tick < 4; tick++ {
		existing, ghosts, _, _, err = refreshDaemonInstances(existing)
		require.NoError(t, err)
	}
	logged := warnings.String()
	assert.Equal(t, 1, strings.Count(logged, "is owed an on_complete teardown"), "the owed-teardown warning does not repeat per tick:\n%s", logged)
	assert.NotContains(t, logged, "repair", "a row that recovers on its own is not sent for repair")
	assert.Positive(t, ghosts[taskRunReservationKey("repo-a", "task-1")], "its run still counts against the task's cap")
}
