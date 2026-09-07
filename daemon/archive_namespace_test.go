package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateSessionRejectsArchiveDirectoryCollision(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	_, err = m.CreateSession(context.Background(), CreateSessionRequest{Title: "feature/login", RepoPath: repoPath, Program: "claude"})
	require.NoError(t, err)
	_, err = m.CreateSession(context.Background(), CreateSessionRequest{Title: "feature-login", RepoPath: repoPath, Program: "claude"})
	require.Error(t, err, "second create must refuse the shared archive directory")
	assert.Contains(t, err.Error(), `session titled "feature/login" already maps to archive directory "feature-login"`)
}

func TestArchiveDirectoryNamespaceClaims(t *testing.T) {
	for _, source := range []string{"live", "archived", "disk live", "disk archived", "reserved", "other repo"} {
		t.Run(source, func(t *testing.T) {
			m := &Manager{instances: make(map[string]*session.Instance), reservedTmuxNames: make(map[string]string)}
			inst := &session.Instance{Title: "feature/login"}
			var disk []session.InstanceData
			switch source {
			case "live", "archived":
				if source == "archived" {
					inst.SetStatusForTest(session.Archived)
				}
				m.instances[daemonInstanceKey("repo", inst.Title)] = inst
			case "disk live", "disk archived":
				status := session.Ready
				if source == "disk archived" {
					status = session.Archived
				}
				disk = []session.InstanceData{{Title: inst.Title, Status: status}}
			case "reserved":
				m.reservedTmuxNames[daemonInstanceKey("repo", "reserved-tmux-name")] = inst.Title
			case "other repo":
				m.instances[daemonInstanceKey("other", inst.Title)] = inst
			}
			err := m.validateArchiveTitleLocked("repo", "feature-login", disk, nil)
			if source == "other repo" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), inst.Title)
			assert.Contains(t, err.Error(), "archive directory")
		})
	}
}

func TestArchiveOccupiedDestinationPreservesTabs(t *testing.T) {
	for _, occupied := range []string{"empty directory", "nonempty directory", "symlink"} {
		t.Run(occupied, func(t *testing.T) {
			m, repoID, repoPath := newStatusTestManager(t)
			inst, src := registerArchivable(t, m, repoID, repoPath, "feature-login")
			inst.AddTabForTest("agent", session.TabKindAgent)
			inst.AddTabForTest("shell", session.TabKindShell)
			inst.AddTabForTest("watcher", session.TabKindProcess)
			before := inst.ToInstanceData()
			dest, err := archivedWorktreePath(repoID, inst.Title)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0755))
			if occupied == "symlink" {
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "missing"), dest))
			} else {
				require.NoError(t, os.Mkdir(dest, 0755))
				if occupied == "nonempty directory" {
					require.NoError(t, os.WriteFile(filepath.Join(dest, "owned"), []byte("keep"), 0644))
				}
			}
			owner := session.InstanceData{Title: "feature/login", Status: session.Archived, Worktree: session.GitWorktreeData{WorktreePath: dest}}
			require.NoError(t, appendInstanceData(repoID, owner))
			teardown := archiveTeardown
			called := false
			archiveTeardown = func(i *session.Instance, path string, claim sessiongit.RelocationClaim, hook func() error, trust bool) (error, error) {
				called = true
				return teardown(i, path, claim, hook, trust)
			}
			t.Cleanup(func() { archiveTeardown = teardown })
			_, _, err = m.ArchiveSession(ArchiveSessionRequest{Title: inst.Title, RepoID: repoID})
			require.Error(t, err)
			assert.False(t, called, "occupied destination must refuse before any tab teardown")
			assert.Equal(t, before.Tabs, inst.ToInstanceData().Tabs, "tabs must remain untouched")
			assert.Equal(t, before.Liveness, inst.GetLiveness(), "session must stay live")
			assert.Equal(t, session.OpNone, inst.GetInFlightOp())
			assert.True(t, inst.Started())
			assert.Equal(t, src, inst.GetWorktreePath())
			assert.Contains(t, err.Error(), dest)
			assert.Contains(t, err.Error(), owner.Title)
		})
	}
}
