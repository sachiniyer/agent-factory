package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// #2864. Registry mode (launched outside a repo, #2477) has no active project,
// so newHome deliberately skips the cold-start snapshot: an empty repoID makes
// the daemon answer with the CROSS-REPO list, and those sessions are not merely
// irrelevant to an empty scope — they are INERT. Every per-session verb keys on
// m.repoID and resolveSessionActionTarget requires target.repoID == m.repoID, so
// each one silently no-ops.
//
// The refresh tick then undid that a second later: it fetched with the same
// empty repoID and reconciled the answer straight into the projection. The rail
// filled with rows whose `s` and `enter` do nothing — the dead-affordance class
// #2830 fixed one layer up, arriving here by a different route.
//
// Driven through the real Update path with a real snapshotFetchedMsg, because
// what regressed is the poll's behavior, not a helper's.
func TestRegistryModeSnapshotPollLeavesTheRailEmpty(t *testing.T) {
	h := newTestHome(t)
	pinSnapshotInstanceBuilder(t)
	h.repoRoot = "" // registry mode: no active project…
	h.repoID = ""   // …so the daemon reads every fetch as all-repos
	require.Equal(t, 0, h.store.NumInstances(), "precondition: the rail starts empty")

	h.Update(snapshotFetchedMsg{
		repoID: "",
		data:   []session.InstanceData{crossRepoInstance("alpha"), crossRepoInstance("beta")},
	})

	assert.Equal(t, 0, h.store.NumInstances(),
		"a cross-repo row the empty scope cannot act on must not reach the projection")
}

// The poll must keep working normally the moment a project IS active — the guard
// is about an empty scope, not about polling.
func TestActiveProjectSnapshotPollStillReconciles(t *testing.T) {
	h := newTestHome(t)
	pinSnapshotInstanceBuilder(t)
	h.repoRoot = t.TempDir()
	require.NotEmpty(t, h.repoID, "precondition: newTestHome gives an active scope")

	h.Update(snapshotFetchedMsg{
		repoID: h.repoID,
		data:   []session.InstanceData{crossRepoInstance("mine")},
	})

	assert.Equal(t, 1, h.store.NumInstances(),
		"an active scope still reconciles its own sessions on every poll")
}

// Alarms are deliberately NOT gated with the sessions. deliveryAlarms reads an
// empty repoID as every repo the same way, but an alarm is a notification, not a
// target: it needs no active scope to be worth showing, and suppressing it would
// hide a real delivery failure from the mode a user is most likely idling in.
func TestRegistryModeSnapshotPollStillRaisesDeliveryAlarms(t *testing.T) {
	h := newTestHome(t)
	pinSnapshotInstanceBuilder(t)
	h.repoRoot = ""
	h.repoID = ""

	h.Update(snapshotFetchedMsg{
		repoID: "",
		data:   []session.InstanceData{crossRepoInstance("alpha")},
		alarms: []daemon.DeliveryAlarm{{
			TaskID: "t1", TaskName: "watcher", TargetSession: "alpha",
			Pending: 3, Consecutive: 5, Since: time.Now().Add(-time.Hour),
		}},
	})

	assert.Equal(t, 0, h.store.NumInstances(), "the sessions are still refused")
	assert.True(t, h.alarmBanner.Active(),
		"a delivery failure is worth showing with or without an active project")
}

// pinSnapshotInstanceBuilder makes materialization deterministic so these tests
// measure the SCOPE guard and nothing else — a build failure would otherwise
// leave an empty rail for entirely the wrong reason and read as a pass.
func pinSnapshotInstanceBuilder(t *testing.T) {
	t.Helper()
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return instanceWithFakeBackend(t, d.Title), nil
	}))
}

func crossRepoInstance(title string) session.InstanceData {
	d := session.InstanceData{Title: title, CreatedAt: time.Now()}
	d.Worktree.RepoPath = "/repos/elsewhere"
	return d
}
