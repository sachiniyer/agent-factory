package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestArchivePortableLiteralDiskNames(t *testing.T) {
	for _, tc := range []struct {
		name, title string
		collision   bool
	}{
		{".foo", "foo", false}, {"-foo", "foo", false}, {"..foo", "foo", false},
		{"foo..bar", "foobar", false}, {".git", "git", false}, {".DS_Store", "DS_Store", false},
		{"foo", "foo", true}, {"bar", "foo", false}, {"Foo", "foo", true}, {"e\u0301", "é", true}, {"Σ", "ς", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repoID, _ := newStatusTestManager(t)
			dest, err := archivedWorktreePath(repoID, tc.title)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Join(filepath.Dir(dest), tc.name), 0755))
			existing, err := archiveDirectoryCollision(dest, false)
			require.NoError(t, err)
			require.Equal(t, tc.collision, existing != "", "literal entry %q for %q", tc.name, tc.title)
			names, err := archiveRelocationSnapshot(repoID)
			require.NoError(t, err)
			err = names.validateArchiveRelocationDestination(tc.title)
			require.Equal(t, tc.collision, err != nil)
			err = inspectArchivePortableNamespace(repoID, &session.Instance{Title: tc.title}, dest, false)
			require.Equal(t, tc.collision, err != nil)
		})
	}
}

func TestArchivePortableSuffixSnapshot(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	dest, err := archivedWorktreePath(repoID, "foo")
	require.NoError(t, err)
	parent := filepath.Dir(dest)
	require.NoError(t, os.MkdirAll(parent, 0755))
	for i := 1; i <= 8; i++ {
		name := "foo (archived)"
		if i > 1 {
			name = fmt.Sprintf("foo (archived %d)", i)
		}
		require.NoError(t, os.Mkdir(filepath.Join(parent, name), 0755))
	}
	var calls atomic.Int32
	restore := sessiongit.SetArchiveReadDirForTest(parent, func(path string) ([]os.DirEntry, error) {
		calls.Add(1)
		return os.ReadDir(path)
	})
	defer restore()
	m.mu.Lock()
	title, err := m.uniqueArchivedTitleLocked(repoID, repoPath, "foo", "claude", runtimeNamespaceLocalTmux, nil)
	m.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, "foo (archived 9)", title)
	require.EqualValues(t, 1, calls.Load(), "one parent enumeration for all nine rungs")
}

func TestArchivePortableRenamedRecordClaimsBothNames(t *testing.T) {
	for _, title := range []string{"old", "feature-login"} {
		t.Run(title, func(t *testing.T) {
			_, repoID, _ := newStatusTestManager(t)
			oldDest, err := archivedWorktreePath(repoID, "old")
			require.NoError(t, err)
			data := session.InstanceData{Title: "feature/login", Liveness: session.LiveArchived}
			data.Worktree.WorktreePath = oldDest
			raw, err := json.Marshal([]session.InstanceData{data})
			require.NoError(t, err)
			require.NoError(t, config.SaveRepoInstances(repoID, raw))
			dest, err := archivedWorktreePath(repoID, title)
			require.NoError(t, err)
			err = inspectArchivePortableNamespace(repoID, &session.Instance{Title: title}, dest, false)
			require.ErrorContains(t, err, "collides with existing archive")
		})
	}
}

func TestArchivePortableOwnerReadDeadline(t *testing.T) {
	m, repoID, _ := newStatusTestManager(t)
	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	if err := os.Remove(path); err != nil {
		require.True(t, os.IsNotExist(err))
	}
	require.NoError(t, unix.Mkfifo(path, 0600))
	// Opening both ends avoids blocking the test; no bytes or EOF reach the reader
	// until cleanup. This exercises the real config read behind the reservation.
	pipe, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0600)
	require.NoError(t, err)
	defer pipe.Close()
	done := make(chan error, 1)
	dest, err := archivedWorktreePath(repoID, "foo")
	require.NoError(t, err)
	inst := &session.Instance{Title: "foo"}
	source := filepath.Join(t.TempDir(), "source")
	go func() {
		_, err := m.checkArchiveDestination(repoID, inst, dest, source)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "filesystem is responsive")
		m.mu.Lock()
		require.Empty(t, m.reservedArchiveDestinations)
		m.mu.Unlock()
	case <-time.After(4 * time.Second):
		require.NoError(t, pipe.Close())
		<-done
		t.Fatal("owner read exceeded deadline while holding the archive reservation")
	}
}

func TestArchivePortableSnapshotReadErrorIsFatal(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	dest, err := archivedWorktreePath(repoID, "foo")
	require.NoError(t, err)
	var calls atomic.Int32
	restore := sessiongit.SetArchiveReadDirForTest(filepath.Dir(dest), func(string) ([]os.DirEntry, error) {
		calls.Add(1)
		return nil, os.ErrPermission
	})
	defer restore()
	m.mu.Lock()
	_, err = m.uniqueArchivedTitleLocked(repoID, repoPath, "foo", "claude", runtimeNamespaceLocalTmux, nil)
	m.mu.Unlock()
	require.ErrorIs(t, err, errTitleCheckFatal)
	require.ErrorIs(t, err, os.ErrPermission)
	require.ErrorContains(t, err, "filesystem is responsive")
	require.EqualValues(t, 1, calls.Load())
}

func TestArchivePortableRecordedLiteralName(t *testing.T) {
	_, repoID, _ := newStatusTestManager(t)
	dest, err := archivedWorktreePath(repoID, "foo")
	require.NoError(t, err)
	data := session.InstanceData{Title: "other", Liveness: session.LiveArchived}
	data.Worktree.WorktreePath = filepath.Join(filepath.Dir(dest), ".foo")
	require.False(t, archiveRecordClaimsTitle(data, "foo"))
	require.True(t, archiveRecordClaimsTitle(data, "other"))
	raw, err := json.Marshal([]session.InstanceData{data})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances(repoID, raw))
	require.NoError(t, inspectArchivePortableNamespace(repoID, &session.Instance{Title: "foo"}, dest, false))
}
