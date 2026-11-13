package daemon

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// #7209 review R2-1: the local archived restore is #7206's own route (/clear,
// archive, restore). Its re-spawn refuses the stale id and stages a fresh
// replacement, which the respawn makes durable before the spawn through the
// persist callback a daemon route installs. The archived restore installed
// none, so the replacement lived in memory alone until the restore committed,
// after the spawn.

// persistHookRecoverBackend is recoverFakeBackend whose Recover runs the
// persist callback the route installed, as LocalBackend's respawn does with a
// staged replacement, and counts the writes that callback made.
type persistHookRecoverBackend struct {
	*recoverFakeBackend
	writes      *atomic.Int32
	installed   bool
	fired       error
	hookWritten int32
}

func (b *persistHookRecoverBackend) Recover(inst *session.Instance) error {
	if notify := inst.RelocationSettleNotifyForTest(); notify != nil {
		b.installed = true
		before := b.writes.Load()
		b.fired = notify(inst)
		b.hookWritten = b.writes.Load() - before
	}
	return b.recoverFakeBackend.Recover(inst)
}

func TestRestoreArchived_ReSpawnHasADurablePersistHook(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	var writes atomic.Int32
	backend := &persistHookRecoverBackend{
		recoverFakeBackend: &recoverFakeBackend{FakeBackend: session.NewFakeBackend()},
		writes:             &writes,
	}
	inst.SetBackend(backend)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "worker" {
			writes.Add(1)
		}
		return nil
	}

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	require.True(t, backend.installed,
		"the archived restore's re-spawn must have a persist hook, so a staged replacement is durable before its spawn")
	require.NoError(t, backend.fired, "the hook writes the record")
	require.Equal(t, int32(1), backend.hookWritten, "the hook's own write reaches disk")
	require.Nil(t, inst.RelocationSettleNotifyForTest(), "the route's hook does not outlive its runtime call")
}
