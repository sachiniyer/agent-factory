package api

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSessionsCreate_AccountAutoChecksPoolRouting is #4404's CLI half of the
// version-skew contract the pickers already honor: an omitted --account asks a
// routing-capable daemon to pick from the pool, but a pre-router daemon's gob
// decoder silently drops account_auto and applies its own legacy contract —
// a configured default without the router's wall check, or the ambient
// identity while a logged-in pool sits unrouted. The response cannot tell
// skew from honor — a configured default applied by an old daemon reads
// exactly like the same default the new contract prefers — so the capability
// probe runs BEFORE the create.
//
// The refusal fires only when the outcome can differ: with no logged-in
// accounts and no configured default, both contracts land on ambient, so a
// plain create is not a version-skewed request and still runs.
func TestSessionsCreate_AccountAutoChecksPoolRouting(t *testing.T) {
	preRouter := daemon.ListAccountsResponse{
		Agents: []string{"claude", "codex"}, // PoolRouting absent: a daemon that predates the router.
	}
	for _, tc := range []struct {
		name         string
		resp         daemon.ListAccountsResponse
		probeErr     error
		wantRefused  bool
		wantInRefuse []string
	}{
		{
			name: "configured default launches without the wall check",
			resp: daemon.ListAccountsResponse{
				Agents:   preRouter.Agents,
				Defaults: map[string]string{"codex": "work"},
			},
			wantRefused:  true,
			wantInRefuse: []string{`"work"`, "predates pool routing", "wall check", `--account ""`},
		},
		{
			name: "logged-in pool sits unrouted",
			resp: daemon.ListAccountsResponse{
				Agents:  preRouter.Agents,
				Entries: []daemon.AccountEntry{{Agent: "codex", Name: "work", LoggedIn: true}},
			},
			wantRefused:  true,
			wantInRefuse: []string{"predates pool routing", "ambient identity"},
		},
		{
			name: "registered but never logged in does not diverge",
			resp: daemon.ListAccountsResponse{
				Agents:  preRouter.Agents,
				Entries: []daemon.AccountEntry{{Agent: "codex", Name: "work", LoggedIn: false}},
			},
			wantRefused: false,
		},
		{
			name:        "no accounts and no default lands ambient either way",
			resp:        preRouter,
			wantRefused: false,
		},
		{
			name:        "a failed probe stays open for the create's own errors",
			probeErr:    errors.New("control socket unreachable"),
			wantRefused: false,
		},
		{
			name: "a routing daemon is asked to route",
			resp: daemon.ListAccountsResponse{
				Agents:      preRouter.Agents,
				Entries:     []daemon.AccountEntry{{Agent: "codex", Name: "work", LoggedIn: true}},
				PoolRouting: true,
			},
			wantRefused: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			silenceStdio(t)

			repo := t.TempDir()
			require.NoError(t, exec.Command("git", "init", repo).Run())

			var got *daemon.CreateSessionRequest
			prevCreate := createSessionViaDaemon
			createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
				got = &req
				return &session.InstanceData{Title: req.Title}, nil
			}
			t.Cleanup(func() { createSessionViaDaemon = prevCreate })

			setSessionsCreateFlags(t, "routed", repo, false, false)
			listAccountsViaDaemon = func(req daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
				assert.Equal(t, "codex", req.Agent, "the probe is scoped to the agent being created")
				assert.Equal(t, repo, req.RepoPath, "the probe carries the project so per-project defaults resolve")
				return tc.resp, tc.probeErr
			}
			prevProgram := createProgramFlag
			createProgramFlag = "codex"
			t.Cleanup(func() { createProgramFlag = prevProgram })

			err := sessionsCreateCmd.RunE(sessionsCreateCmd, nil)
			if tc.wantRefused {
				require.Error(t, err, "a divergent pre-router create must be refused, not silently run")
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
