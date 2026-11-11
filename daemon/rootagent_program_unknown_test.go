package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// #7079: the root agent's default profile picked bare `claude` whenever the
// project config failed to resolve — a config that could not be READ in time
// included — so the root ran without the project's program_overrides.claude
// and nothing but a warning showed it.

func unknownLaunchConfig(t *testing.T) {
	t.Helper()
	prev := resolveProjectConfigForLaunch
	resolveProjectConfigForLaunch = func(path string) (*config.ResolvedConfig, error) {
		return nil, unknownProjectConfig(path)
	}
	t.Cleanup(func() { resolveProjectConfigForLaunch = prev })
}

func TestRootAgentProgramRefusesAnUnknownProjectConfig(t *testing.T) {
	unknownLaunchConfig(t)

	program, err := rootAgentProgramForProfile("/repo", config.RootAgent{})

	require.Error(t, err, "an unknown project config is not bare claude")
	require.True(t, session.IsProjectConfigUnknown(err))
	require.Empty(t, program)
}

// An explicit profile program names no config, and answers as it always did.
func TestRootAgentExplicitProgramNeedsNoProjectConfig(t *testing.T) {
	unknownLaunchConfig(t)

	program, err := rootAgentProgramForProfile("/repo", config.RootAgent{Program: "/opt/root"})

	require.NoError(t, err)
	require.Equal(t, "/opt/root", program)
}

// The refusal is placed BEFORE the heal's reap: the dead root's record holds
// the only pointer to the conversation (#2616) and tab roster (#2628) the heal
// carries, so a program that cannot be picked must leave it in place for the
// next pass rather than delete it and then refuse.
func TestRootHealRefusesAnUnknownProgramWithoutReaping(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true")

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "precondition: the first ensure creates the root")
	require.Len(t, *seen, 1)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	first.SetStatusForTest(session.Dead)
	unknownLaunchConfig(t)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 1, "no root is created on a program that could not be picked")
	require.Same(t, first, findRootInstance(t, manager, repoPath),
		"the refusal leaves the dead root record in place — reaping it first would discard what the heal carries")
	data, err := loadRepoInstanceData(repo.ID)
	require.NoError(t, err)
	roots := 0
	for _, d := range data {
		if d.Title == session.RootSessionTitle {
			roots++
		}
	}
	require.Equal(t, 1, roots, "the persisted root record survives the refused heal")
}

// An unknown project config is the "git never answered" class the escalation
// must not report as an established cause.
func TestRootEnsureCountsAnUnknownProjectConfigAsUnanswered(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	warnings := captureWarnings(t)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	st := &rootEnsureState{}

	manager.rootEnsureFailed("/repo", st, unknownProjectConfig("/repo"))

	require.Equal(t, 1, st.unansweredFailures)
	require.True(t, strings.Contains(warnings.String(), "couldn't read the project config"), warnings.String())
}
