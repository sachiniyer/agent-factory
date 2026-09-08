package daemon

import (
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
)

func TestArchiveDirectoryInPlaceAdmission(t *testing.T) {
	for _, pair := range [][2]bool{{false, true}, {true, false}, {false, false}} {
		for _, source := range []string{"reserved", "disk"} {
			for _, derived := range []bool{false, true} {
				t.Run(fmt.Sprintf("existing_here=%t/candidate_here=%t/%s/derived=%t", pair[0], pair[1], source, derived), func(t *testing.T) {
					t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
					repoPath := setupControlRepo(t)
					m, err := NewManager(config.DefaultConfig())
					require.NoError(t, err)
					repo, _, release, _, err := m.reserveCreate(CreateSessionRequest{Title: "feature/login", RepoPath: repoPath, Program: "claude", InPlace: pair[0]})
					require.NoError(t, err)
					if source == "disk" {
						release()
						require.NoError(t, appendInstanceData(repo.ID, session.InstanceData{Title: "feature/login", Path: repoPath, Status: session.Ready, BackendType: "local", Worktree: session.GitWorktreeData{ExternalWorktree: pair[0]}}))
					} else {
						defer release()
					}
					req := CreateSessionRequest{Title: "feature-login", RepoPath: repoPath, Program: "claude", InPlace: pair[1]}
					if derived {
						req.TitleBase = req.Title
						req.Title = ""
					}
					_, title, releaseSecond, _, err := m.reserveCreate(req)
					if releaseSecond != nil {
						defer releaseSecond()
					}
					if !pair[0] && !pair[1] {
						if derived {
							require.NoError(t, err)
							require.Equal(t, "feature-login-2", title)
						} else {
							require.ErrorContains(t, err, "archive directory")
						}
					} else {
						require.NoError(t, err)
						require.Equal(t, "feature-login", title)
					}
				})
			}
		}
	}
}

func TestArchiveDirectoryIgnoresExternalInstance(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	gw, _, err := sessiongit.NewGitWorktreeInPlace(repoPath)
	require.NoError(t, err)
	inst := &session.Instance{Title: "feature/login"}
	inst.SetGitWorktreeForTest(gw)
	require.True(t, inst.IsExternalWorktree(), "use the same predicate as archive admission")
	require.True(t, inst.ToInstanceData().Worktree.ExternalWorktree)
	m.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	require.NoError(t, m.validateArchiveTitleLocked(repoID, "feature-login", nil, nil, false))
}
