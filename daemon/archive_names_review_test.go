package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

type archiveScopeBackend struct {
	readyFakeBackend
	kind string
}

func (b archiveScopeBackend) Type() string { return b.kind }
func (b archiveScopeBackend) Capabilities() session.Capabilities {
	caps := b.readyFakeBackend.Capabilities()
	if b.kind != "local" {
		caps.Workspace = session.WorkspaceRemote
	}
	return caps
}

// Exercise the real create admission boundary without provisioning off-box runtimes.
func TestArchiveDirectoryBackendScope(t *testing.T) {
	for _, pair := range [][2]string{{"docker", "docker"}, {"ssh", "ssh"}, {"local", "docker"}, {"docker", "local"}, {"local", "local"}} {
		for _, source := range []string{"reserved", "persisted"} {
			t.Run(pair[0]+"_then_"+pair[1]+"_"+source, func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				repoPath := setupControlRepo(t)
				m, err := NewManager(config.DefaultConfig())
				require.NoError(t, err)
				repo, _, release, _, err := m.reserveCreate(CreateSessionRequest{Title: "feature/login", RepoPath: repoPath, Program: "claude", Backend: pair[0]})
				require.NoError(t, err)
				if source == "persisted" {
					release()
					require.NoError(t, appendInstanceData(repo.ID, session.InstanceData{Title: "feature/login", Path: repoPath, Status: session.Archived, BackendType: pair[0]}))
				} else {
					defer release()
				}
				_, _, releaseSecond, _, err := m.reserveCreate(CreateSessionRequest{Title: "feature-login", RepoPath: repoPath, Program: "claude", Backend: pair[1]})
				if releaseSecond != nil {
					defer releaseSecond()
				}
				if pair == [2]string{"local", "local"} {
					require.ErrorContains(t, err, "archive directory")
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestArchiveDirectoryPortableComparison(t *testing.T) {
	for _, pair := range [][2]string{{"Caf\u00e9", "Cafe\u0301"}, {"Feature", "feature"}, {"x/Σz", "x-ςz"}, {"x/Σz", "x-σz"}, {"Feature", "unrelated"}} {
		for _, source := range []string{"live", "disk", "reserved"} {
			t.Run(pair[0]+"_"+pair[1]+"_"+source, func(t *testing.T) {
				m := &Manager{instances: make(map[string]*session.Instance), reservedTitles: make(map[string]struct{}), reservedArchiveTitles: make(map[string]struct{})}
				var disk []session.InstanceData
				switch source {
				case "live":
					m.instances[daemonInstanceKey("repo", pair[0])] = &session.Instance{Title: pair[0]}
				case "disk":
					disk = []session.InstanceData{{Title: pair[0], Status: session.Archived}}
				case "reserved":
					m.reservedTitles[daemonInstanceKey("repo", pair[0])] = struct{}{}
					m.reservedArchiveTitles[daemonInstanceKey("repo", pair[0])] = struct{}{}
				}
				err := m.validateArchiveTitleLocked("repo", pair[1], disk, nil, false)
				if pair[1] == "unrelated" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "archive directory")
				}
			})
		}
	}
}

func TestArchiveDirectoryIgnoresRemoteClaims(t *testing.T) {
	for _, source := range []string{"live", "disk", "reserved"} {
		for _, kind := range []string{"docker", "ssh", "remote"} {
			t.Run(source+"_"+kind, func(t *testing.T) {
				m := &Manager{instances: make(map[string]*session.Instance), reservedTitles: make(map[string]struct{})}
				var disk []session.InstanceData
				const title = "feature/login"
				switch source {
				case "live":
					inst := &session.Instance{Title: title}
					inst.SetBackend(archiveScopeBackend{readyFakeBackend{session.NewFakeBackend()}, kind})
					m.instances[daemonInstanceKey("repo", title)] = inst
				case "disk":
					disk = []session.InstanceData{{Title: title, BackendType: kind, Status: session.Archived}}
				case "reserved":
					m.reservedTitles[daemonInstanceKey("repo", title)] = struct{}{}
				}
				require.NoError(t, m.validateArchiveTitleLocked("repo", "feature-login", disk, nil, false))
			})
		}
	}
}
