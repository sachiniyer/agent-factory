package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

// The daemon's create-time account router (#4404) decides a session's account
// from what its launch resolves to — the program's agent and whether the
// backend takes an account — and the launch boundary resolves both again later,
// after the create has reserved its title and waited its turn behind the repo's
// start lock. An edit landing in between made the two answers disagree (#4404
// review): the router left a create ambient because codex pointed at a shim, the
// override was removed, and real codex launched on the ambient identity instead
// of the pool.

// stubInstanceFactory records whether NewInstance reached provisioning — the
// point after which a refusal would cost a worktree.
func stubInstanceFactory(t *testing.T) *bool {
	t.Helper()
	reached := false
	restore := SetBackendFactoryForTest(func(InstanceOptions, string) (Backend, error) {
		reached = true
		return &LocalBackend{}, nil
	})
	t.Cleanup(restore)
	return &reached
}

func saveProgramOverride(t *testing.T, program, command string) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{program: command}
	require.NoError(t, config.SaveConfig(cfg))
}

func writeRepoBackend(t *testing.T, repo, backend string) {
	t.Helper()
	fields := map[string]any{"backend": backend}
	if backend == "ssh" {
		fields["ssh"] = map[string]any{"host": "example.invalid"}
	}
	writeInRepoConfig(t, repo, fields)
}

func TestNewInstance_RefusesAnAccountRouteDecidedForAnotherLaunch(t *testing.T) {
	for _, tc := range []struct {
		name        string
		account     string
		decision    AccountRouteDecision
		override    string
		repoBackend string
		wantSetting string
	}{
		{
			// The router saw codex pointed at a shim no agent owns, so it routed
			// nothing; by launch time the override is gone and the label runs codex.
			name:        "override removed after an ambient decision",
			decision:    AccountRouteDecision{Agent: "", BackendScoped: true},
			wantSetting: "program_overrides",
		},
		{
			name:        "override added under a routed account",
			account:     "work",
			decision:    AccountRouteDecision{Agent: "codex", BackendScoped: true},
			override:    "/bin/fake-agent",
			wantSetting: "program_overrides",
		},
		{
			// The router saw an ssh-default repo and routed nothing; by launch
			// time the repo is local and would start on the ambient identity.
			name:        "backend moved to local after an ambient decision",
			decision:    AccountRouteDecision{Agent: "codex", BackendScoped: false},
			wantSetting: "`backend`",
		},
		{
			// Named before the off-box refusal, which would otherwise blame an
			// account the user never configured for a backend they did not pick.
			name:        "backend moved off-box under a routed account",
			account:     "work",
			decision:    AccountRouteDecision{Agent: "codex", BackendScoped: true},
			repoBackend: "ssh",
			wantSetting: "`backend`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			_, err := agentaccount.Register(home, "codex", "work")
			require.NoError(t, err)
			repo := initTempGitRepo(t)
			reached := stubInstanceFactory(t)
			if tc.override != "" {
				saveProgramOverride(t, "codex", tc.override)
			}
			if tc.repoBackend != "" {
				writeRepoBackend(t, repo, tc.repoBackend)
			}
			decision := tc.decision

			_, err = NewInstance(InstanceOptions{
				Title:               "drifted",
				Path:                repo,
				Program:             "codex",
				Account:             tc.account,
				AccountAutoSelected: tc.account != "",
				AccountRoute:        &decision,
			})
			require.Error(t, err, "an account decision made for a different launch must not launch")
			assert.False(t, *reached, "the refusal must land before provisioning")
			assert.Contains(t, err.Error(), tc.wantSetting)
			assert.Contains(t, err.Error(), "changed while")
			assert.Contains(t, err.Error(), "create the session again")
		})
	}
}

// Canaries: the guard is stricter, so pin the creates it must keep admitting —
// every shape where the router and the launch agree, including the persistent
// shim override the web selftest runs on, an ssh-default repo the router left
// alone, and a create the router never saw.
func TestNewInstance_AccountRouteThatMatchesTheLaunchStillCreates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		override    string
		repoBackend string
		account     string
		decision    *AccountRouteDecision
	}{
		{name: "routed pool pick", account: "work", decision: &AccountRouteDecision{Agent: "codex", BackendScoped: true}},
		{name: "evaluated but left ambient", decision: &AccountRouteDecision{Agent: "codex", BackendScoped: true}},
		{name: "persistent shim override", override: "/bin/fake-agent", decision: &AccountRouteDecision{Agent: "", BackendScoped: true}},
		{name: "persistent ssh-default repo", repoBackend: "ssh", decision: &AccountRouteDecision{Agent: "codex", BackendScoped: false}},
		{name: "router never ran", override: "/bin/fake-agent", repoBackend: "ssh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			_, err := agentaccount.Register(home, "codex", "work")
			require.NoError(t, err)
			repo := initTempGitRepo(t)
			reached := stubInstanceFactory(t)
			if tc.override != "" {
				saveProgramOverride(t, "codex", tc.override)
			}
			if tc.repoBackend != "" {
				writeRepoBackend(t, repo, tc.repoBackend)
			}

			inst, err := NewInstance(InstanceOptions{
				Title:               "agreed",
				Path:                repo,
				Program:             "codex",
				Account:             tc.account,
				AccountAutoSelected: tc.account != "",
				AccountRoute:        tc.decision,
			})
			require.NoError(t, err)
			require.NotNil(t, inst)
			assert.True(t, *reached)
		})
	}
}

// The router must read the overrides the LAUNCH reads. The daemon's op-entry
// snapshot is not that: a hand-edited global config is only picked up by
// ApplyConfig or a restart, while the launch resolves from disk every time, so
// a router reading the snapshot could disagree with every create until the
// daemon reloaded — not just during a race.
func TestResolveLaunchProgramReadsTheOverridesTheLaunchReads(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := initTempGitRepo(t)
	require.Equal(t, "codex", ResolveLaunchProgram("codex", repo))

	saveProgramOverride(t, "codex", "/bin/fake-agent")
	require.Equal(t, "/bin/fake-agent", ResolveLaunchProgram("codex", repo),
		"a saved override is visible without any daemon reload")
}
