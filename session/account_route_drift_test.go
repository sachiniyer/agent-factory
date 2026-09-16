package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

// The daemon's create-time account router (#4404) decides a session's account
// from the command its program label resolves to, and the launch boundary
// resolves that label again later — after the create has reserved its title
// and waited its turn behind the repo's start lock. A program_overrides edit
// landing in between made the two answers disagree (#4404 review): the router
// left a create ambient because codex pointed at a shim, the override was
// removed, and real codex launched on the ambient identity instead of the pool.

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

func TestNewInstance_RefusesAnAccountRouteDecidedForAnotherCommand(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := initTempGitRepo(t)
	reached := stubInstanceFactory(t)

	// The router saw codex pointed at a shim no agent owns, so it routed
	// nothing; by launch time the override is gone and the label runs codex.
	_, err := NewInstance(InstanceOptions{
		Title:                 "drifted",
		Path:                  repo,
		Program:               "codex",
		AccountRouteEvaluated: true,
		AccountRouteAgent:     "",
	})
	require.Error(t, err, "an account decision made for a different command must not launch")
	assert.False(t, *reached, "the refusal must land before provisioning")
	assert.Contains(t, err.Error(), "program_overrides")
	assert.Contains(t, err.Error(), "create the session again")
}

func TestNewInstance_RefusesARoutedAccountWhoseCommandMovedAway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	_, err := agentaccount.Register(home, "codex", "work")
	require.NoError(t, err)
	repo := initTempGitRepo(t)
	reached := stubInstanceFactory(t)
	saveProgramOverride(t, "codex", "/bin/fake-agent")

	_, err = NewInstance(InstanceOptions{
		Title:                 "moved",
		Path:                  repo,
		Program:               "codex",
		Account:               "work",
		AccountAutoSelected:   true,
		AccountRouteEvaluated: true,
		AccountRouteAgent:     "codex",
	})
	require.Error(t, err)
	assert.False(t, *reached)
	assert.Contains(t, err.Error(), "changed while",
		"the refusal names the race, not an account-namespace mismatch the user never configured")
}

// Canaries: the guard is stricter, so pin the creates it must keep admitting —
// every shape where the router and the launch agree, including the persistent
// shim override the web selftest runs on and a create the router never saw.
func TestNewInstance_AccountRouteThatMatchesTheLaunchStillCreates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
		account  string
		agent    string
		routed   bool
	}{
		{name: "routed pool pick", account: "work", agent: "codex", routed: true},
		{name: "evaluated but left ambient", agent: "codex", routed: true},
		{name: "persistent shim override", override: "/bin/fake-agent", agent: "", routed: true},
		{name: "router never ran", override: "/bin/fake-agent", agent: "codex", routed: false},
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

			inst, err := NewInstance(InstanceOptions{
				Title:                 "agreed",
				Path:                  repo,
				Program:               "codex",
				Account:               tc.account,
				AccountAutoSelected:   tc.account != "",
				AccountRouteEvaluated: tc.routed,
				AccountRouteAgent:     tc.agent,
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
