package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
)

func TestArchiveReadDirTimeoutCancelsArchive(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, m, repoID, repoPath, "worker")
	inst.AddTabForTest("shell", session.TabKindShell)
	before := inst.ToInstanceData()
	dest, err := archivedWorktreePath(repoID, inst.Title)
	require.NoError(t, err)
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	restore := sessiongit.SetArchiveReadDirForTest(filepath.Dir(dest), func(string) ([]os.DirEntry, error) {
		close(entered)
		<-unblock
		return nil, nil
	})
	defer restore()
	result := make(chan error, 1)
	go func() {
		_, _, err := m.ArchiveSession(ArchiveSessionRequest{ID: inst.ID, RepoID: repoID})
		result <- err
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("archive bypassed directory probe: %v", err)
	case <-time.After(5 * time.Second):
		release()
		<-result
		t.Fatal("archive did not reach directory probe")
	}
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "retry")
	case <-time.After(5 * time.Second):
		release()
		<-result
		t.Fatal("archive remained blocked in directory enumeration")
	}
	require.Equal(t, session.OpNone, inst.GetInFlightOp())
	require.Equal(t, before.Liveness, inst.GetLiveness())
	require.Equal(t, before.Tabs, inst.ToInstanceData().Tabs)
	require.Empty(t, m.reservedArchiveDestinations)
	disk, err := loadRepoInstanceData(repoID)
	require.NoError(t, err)
	require.Equal(t, session.OpNone, disk[0].InFlightOp)
}
