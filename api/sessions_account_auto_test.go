package api

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSessionsCreate_AccountAutoChecksPoolRouting is #4404's CLI half of the
// version-skew contract the pickers already honor: an omitted --account asks a
// routing-capable daemon to pick from the pool, but a pre-router daemon's gob
// decoder silently drops account_auto and applies its own legacy contract —
// a configured default without the router's wall check, or the ambient
// identity while a logged-in pool sits unrouted. The response cannot tell
// skew from honor, so the capability is checked BEFORE the create; see
// accountAutoSkewRefusal for the decision points this table walks.
func TestSessionsCreate_AccountAutoChecksPoolRouting(t *testing.T) {
	preRouter := daemon.ListAccountsResponse{
		Agents: []string{"claude", "codex"}, // PoolRouting absent: a daemon that predates the router.
	}
	loggedInPool := daemon.ListAccountsResponse{
		Agents:  preRouter.Agents,
		Entries: []daemon.AccountEntry{{Agent: "codex", Name: "work", LoggedIn: true}},
	}
	routing := daemon.PingResponse{OK: true, PoolRouting: true}
	old := daemon.PingResponse{OK: true}
	for _, tc := range []struct {
		name string
		ping daemon.PingResponse
		// pingErr and listErr fail the corresponding probe.
		pingErr  error
		resp     daemon.ListAccountsResponse
		listErr  error
		backend  string
		repoConf map[string]any
		// wantPing/wantList: whether the probe must have been asked at all.
		wantPing     bool
		wantList     bool
		wantRefused  bool
		wantInRefuse []string
	}{
		{
			name: "configured default launches without the wall check",
			ping: old, wantPing: true, wantList: true,
			resp: daemon.ListAccountsResponse{
				Agents:   preRouter.Agents,
				Defaults: map[string]string{"codex": "work"},
			},
			wantRefused:  true,
			wantInRefuse: []string{`"work"`, "predates pool routing", "wall check", `--account ""`},
		},
		{
			name: "logged-in pool sits unrouted",
			ping: old, wantPing: true, wantList: true,
			resp:         loggedInPool,
			wantRefused:  true,
			wantInRefuse: []string{"predates pool routing", "ambient identity"},
		},
		{
			name: "docker carries the account, so the skew still diverges",
			ping: old, wantPing: true, wantList: true,
			resp: loggedInPool, backend: "docker",
			wantRefused:  true,
			wantInRefuse: []string{"predates pool routing"},
		},
		{
			name: "registered but never logged in does not diverge",
			ping: old, wantPing: true, wantList: true,
			resp: daemon.ListAccountsResponse{
				Agents:  preRouter.Agents,
				Entries: []daemon.AccountEntry{{Agent: "codex", Name: "work", LoggedIn: false}},
			},
		},
		{
			name: "no accounts and no default lands ambient either way",
			ping: old, wantPing: true, wantList: true,
			resp: preRouter,
		},
		{
			// #4404 review: an unknown capability must not authorize the
			// legacy contract an old daemon would silently run.
			name:         "a failed capability probe refuses",
			pingErr:      errors.New("control socket unreachable"),
			wantPing:     true,
			wantRefused:  true,
			wantInRefuse: []string{"could not confirm", "control socket unreachable", "Retry", `--account ""`},
		},
		{
			name: "an old daemon whose accounts cannot be listed refuses",
			ping: old, wantPing: true, wantList: true,
			listErr:      errors.New("accounts: permission denied"),
			wantRefused:  true,
			wantInRefuse: []string{"predates pool routing", "permission denied", `--account ""`},
		},
		{
			name: "a routing daemon is asked to route",
			ping: routing, wantPing: true,
			resp: loggedInPool,
		},
		{
			// The canary for the Ping switch: ListAccounts fails whole on ONE
			// unreadable account, which a routing daemon's create routes
			// around. It must not be consulted, let alone refuse.
			name: "a routing daemon never needs the account listing",
			ping: routing, wantPing: true,
			listErr: errors.New("accounts: one credential directory is unreadable"),
		},
		{
			// #4404 review: the router leaves non-carrying backends on the
			// legacy contract on EVERY daemon, so there is no skew to refuse
			// — not even when the probe would have said "old".
			name: "an ssh backend flag has no skew", ping: old,
			resp: loggedInPool, backend: "ssh",
		},
		{
			name: "a hook backend flag has no skew", ping: old,
			resp: loggedInPool, backend: "hook",
		},
		{
			name: "a repo-configured ssh backend has no skew", ping: old,
			resp: loggedInPool,
			repoConf: map[string]any{
				"backend": "ssh",
				"ssh":     map[string]any{"host": "example.invalid"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			silenceStdio(t)

			repo := t.TempDir()
			require.NoError(t, exec.Command("git", "init", repo).Run())
			// The create resolves the worktree through git, which answers the
			// canonical path — /var/... comes back /private/var/... on macOS.
			wantRepo, err := filepath.EvalSymlinks(repo)
			require.NoError(t, err)
			if tc.repoConf != nil {
				dir := filepath.Join(repo, config.InRepoConfigDirName)
				require.NoError(t, os.MkdirAll(dir, 0o755))
				raw, err := json.Marshal(tc.repoConf)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, config.ConfigFileName), raw, 0o644))
			}

			var got *daemon.CreateSessionRequest
			prevCreate := createSessionViaDaemon
			createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
				got = &req
				return &session.InstanceData{Title: req.Title}, nil
			}
			t.Cleanup(func() { createSessionViaDaemon = prevCreate })

			setSessionsCreateFlags(t, "routed", repo, false, false)
			pinged, listed := false, false
			pingDaemonCapabilities = func() (daemon.PingResponse, error) {
				pinged = true
				return tc.ping, tc.pingErr
			}
			listAccountsViaDaemon = func(req daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
				listed = true
				assert.Equal(t, "codex", req.Agent, "the probe is scoped to the agent being created")
				assert.Equal(t, wantRepo, req.RepoPath, "the probe carries the project so per-project defaults resolve")
				return tc.resp, tc.listErr
			}
			prevProgram := createProgramFlag
			createProgramFlag = "codex"
			t.Cleanup(func() { createProgramFlag = prevProgram })
			createBackendFlag = tc.backend

			err = sessionsCreateCmd.RunE(sessionsCreateCmd, nil)
			assert.Equal(t, tc.wantPing, pinged, "whether the capability probe was asked")
			assert.Equal(t, tc.wantList, listed, "whether the account listing was asked")
			if tc.wantRefused {
				require.Error(t, err, "a divergent or unknown create must be refused, not silently run")
				assert.Nil(t, got, "the refusal happens BEFORE the create — nothing to clean up")
				for _, want := range tc.wantInRefuse {
					assert.Contains(t, err.Error(), want)
				}
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got, "an identical-outcome create still runs")
			assert.True(t, got.AccountAuto, "an omitted --account is this client's opt-in")
		})
	}
}
