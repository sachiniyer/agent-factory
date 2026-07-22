package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestTUIHasNoInstancesWritePath is the #959 structural guard for the
// single-writer model (#960 PR 4). The dual-writer clobber class existed because
// the TUI could write its whole-list view of instances.json and overwrite a
// daemon-authored record (e.g. an out-of-band tab). That is now impossible by
// construction: the TUI has NO instances.json write path at all. This drives
// every TUI mutation that used to persist — quit, new tab, PR-info — and asserts
// the TUI's instances file stays empty: with nothing to write, nothing can be
// clobbered.
func TestTUIHasNoInstancesWritePath(t *testing.T) {
	h := newTestHome(t)

	inst := startedLocalInstance(t, "sess")
	selectInstance(h, inst)

	// quit: previously flushed the sidebar to instances.json (T1); now a no-op.
	_, cmd := h.handleQuit()
	require.NotNil(t, cmd, "quit must still proceed")

	// new shell tab: routed through the daemon (stubbed), reflected locally only.
	createRestore := SetTabCreatorForTest(func(daemon.CreateTabRequest) (string, string, error) {
		return spawnDaemonTab(inst)
	})
	defer createRestore()
	_, _ = h.handleNewTab()
	require.Equal(t, 3, inst.TabCount(), "the daemon-created tab must appear locally")

	// PR-info update: the write routes through the daemon (stubbed no-op).
	prRestore := SetPRInfoSetterForTest(func(title, repoID string, info session.PRInfoData) error { return nil })
	defer prRestore()
	_, _ = h.Update(prInfoUpdatedMsg{
		instance: inst,
		branch:   inst.GetBranch(),
		repoID:   h.repoID,
		info:     &sessiongit.PRInfo{Number: 5, State: "OPEN"},
	})

	// After every previously-persisting path, the TUI's instances.json is still
	// empty — the TUI never wrote it, so an out-of-band daemon record can never be
	// clobbered (#959).
	requireTUIInstancesEmpty(t, h)
}

// TestSnapshotSurfacesOutOfBandSessionWithoutTUIWrite proves the other half of the
// single-writer contract: the TUI learns about daemon-authored state ONLY through
// the Snapshot reconcile (its sole sync path), and surfacing that state writes
// nothing back to disk. Together with the no-write-path guard above, this is why
// the #959 clobber is structurally gone — the daemon owns the state, the TUI
// mirrors it.
func TestSnapshotSurfacesOutOfBandSessionWithoutTUIWrite(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	// A session the daemon created out of band appears in its snapshot.
	added := h.reconcileSnapshot([]session.InstanceData{{Title: "daemon-made", CreatedAt: time.Now()}})
	require.True(t, added, "an out-of-band session in the snapshot is a change")
	require.NotNil(t, findSidebarInstance(h, "daemon-made"),
		"the out-of-band session must surface via the snapshot reconcile")

	// Mirroring daemon state must not cause the TUI to write instances.json.
	requireTUIInstancesEmpty(t, h)
}
