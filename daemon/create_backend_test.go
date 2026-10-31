package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// #5778: validate-before-reserve and the launch derive the backend from ONE
// resolver over the FULL request. This enumerates Host x Backend x repo default x
// ForceRemote and asserts, against an independent oracle of the documented
// precedence, that (a) the resolver picks the right kind, (b) the daemon-host
// prerequisite decision is exactly "kind is local", and (c) the options a launch
// builds resolve to the same kind validation saw. A remote create (--host,
// docker, ssh, hook) must never demand tmux or the agent on the daemon host.
func TestCreateBackendDecisionIsOneResolverForValidateAndLaunch(t *testing.T) {
	for _, repoDefault := range []string{"local", "ssh", "docker"} {
		_, repoPath, _ := defaultAccountFixture(t, "claude", "")
		if repoDefault != "local" {
			// `backend` is a repo-only key (config/manifest.go: sourceRepoOnly), read
			// from the repo's in-repo config; the personal project file ignores it.
			dir := filepath.Join(repoPath, config.InRepoConfigDirName)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
				[]byte(fmt.Sprintf("backend = %q\n", repoDefault)), 0o644))
		}
		for _, host := range []string{"", "gpu-box"} {
			for _, backend := range []string{"", "local", "ssh", "docker", "hook"} {
				for _, forceRemote := range []bool{false, true} {
					name := fmt.Sprintf("default=%s/host=%q/backend=%q/force=%v", repoDefault, host, backend, forceRemote)
					t.Run(name, func(t *testing.T) {
						req := CreateSessionRequest{
							Title: "t", RepoPath: repoPath, Program: "claude",
							Host: host, Backend: backend, ForceRemote: forceRemote,
						}

						var want session.BackendKind
						wantErr := false
						switch {
						case host != "":
							if backend != "" && backend != "ssh" {
								wantErr = true
							}
							want = session.BackendSSH
						case backend != "":
							want = session.BackendKind(backend)
						case forceRemote:
							want = session.BackendHook
						default:
							want = session.BackendKind(repoDefault)
						}

						got, err := createBackendKind(req, repoPath)
						needsLocal, nerr := createNeedsDaemonHostPrereqs(req, repoPath)
						if wantErr {
							require.Error(t, err)
							require.Error(t, nerr)
							return
						}
						require.NoError(t, err)
						require.NoError(t, nerr)
						require.Equal(t, want, got)
						require.Equal(t, want == session.BackendLocal, needsLocal,
							"daemon-host tmux/agent prerequisites apply only to a create that runs on the daemon host")
						if host != "" {
							require.False(t, needsLocal, "--host never preflights the daemon host")
						}

						// The launch builds its options through applyBackendSelection; the
						// kind they resolve to must be the one validation resolved.
						var launch session.InstanceOptions
						req.applyBackendSelection(&launch)
						launchKind, err := session.BackendKindFor(launch, repoPath)
						require.NoError(t, err)
						require.Equal(t, got, launchKind)
					})
				}
			}
		}
	}
}
