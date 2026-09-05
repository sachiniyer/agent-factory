package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #3386: the project's default account, applied on the create so TUI, web, CLI
// and task deliveries all honour it without re-implementing the precedence.
//
// These drive applyDefaultAccount directly — the function CreateSession calls
// before it reserves anything — because the ORDER and the REFUSAL are the whole
// contract, and both are decided before a worktree, a branch or a tmux session
// exists.

// defaultAccountFixture gives a temp AF home, a registered project, and a
// registered account directory when name is non-empty.
func defaultAccountFixture(t *testing.T, agent, account string) (home, repoPath string, project config.Project) {
	t.Helper()
	home = filepath.Join(testguard.CanonicalTempDir(t), "af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath = setupControlRepo(t)
	p, err := config.RegisterProject(repoPath)
	require.NoError(t, err)
	if account != "" {
		require.NoError(t, os.MkdirAll(filepath.Join(home, "accounts", agent, account), 0o700))
	}
	return home, repoPath, p
}

// writeProjectAccounts puts a `default_accounts` table in a project's personal
// config, which is the layer the whole issue is about.
func writeProjectAccounts(t *testing.T, project config.Project, body string) {
	t.Helper()
	path, err := config.ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

// TestProjectDefaultAccountIsAppliedToACreateThatNamedNone is the feature: the
// session runs as the project's account without anyone typing --account.
func TestProjectDefaultAccountIsAppliedToACreateThatNamedNone(t *testing.T) {
	_, repoPath, project := defaultAccountFixture(t, "codex", "work")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"work\"\n")

	req := CreateSessionRequest{Title: "scoped", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, applyDefaultAccount(&config.Config{}, &req))
	assert.Equal(t, "work", req.Account, "the project's configured account must reach the create")
	assert.Contains(t, req.AccountSource, "default_accounts.codex",
		"and the create must carry where it came from, so a later refusal can name it")
}

// TestExplicitAccountBeatsTheProjectDefault is the top of the precedence, and the
// one a user exercises deliberately.
func TestExplicitAccountBeatsTheProjectDefault(t *testing.T) {
	_, repoPath, project := defaultAccountFixture(t, "codex", "work")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"work\"\n")

	req := CreateSessionRequest{Title: "explicit", RepoPath: repoPath, Program: "codex", Account: "chosen"}
	require.NoError(t, applyDefaultAccount(&config.Config{}, &req))
	assert.Equal(t, "chosen", req.Account, "an account the client named must never be replaced by a default")
	assert.Empty(t, req.AccountSource,
		"and it carries no config provenance: the remedy for a refusal is the flag the user typed")
}

// TestProjectDefaultAccountBeatsTheGlobalOne is the middle of the precedence —
// the reason the key admits the personal per-project layer at all.
func TestProjectDefaultAccountBeatsTheGlobalOne(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "codex", "side")
	require.NoError(t, os.MkdirAll(filepath.Join(home, "accounts", "codex", "work"), 0o700))
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"side\"\n")
	global := &config.Config{DefaultAccounts: map[string]string{"codex": "work"}}

	req := CreateSessionRequest{Title: "layered", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, applyDefaultAccount(global, &req))
	assert.Equal(t, "side", req.Account)
	assert.Contains(t, req.AccountSource, "--project",
		"the refusal hint must point at the layer that actually set it")
}

// The global default is the bottom of the precedence, and the fallback when the
// repo cannot be resolved at all — the same shape defaultProgramFor uses.
func TestGlobalDefaultAccountAppliesWithNoProjectOverride(t *testing.T) {
	home, repoPath, _ := defaultAccountFixture(t, "codex", "work")
	global := &config.Config{DefaultAccounts: map[string]string{"codex": "work"}}

	req := CreateSessionRequest{Title: "global", RepoPath: repoPath, Program: "codex"}
	require.NoError(t, applyDefaultAccount(global, &req))
	assert.Equal(t, "work", req.Account)

	unresolvable := CreateSessionRequest{
		Title: "no-repo", RepoPath: filepath.Join(home, "nowhere"), Program: "codex",
	}
	require.NoError(t, applyDefaultAccount(global, &unresolvable))
	assert.Equal(t, "work", unresolvable.Account,
		"a path Git does not recognize falls back to the global layer rather than failing the create here")
}

// An account belongs to ONE agent, and this is the property the map-shaped key
// exists to guarantee: a codex default can never scope a claude session.
func TestADefaultForAnotherAgentIsNotApplied(t *testing.T) {
	_, repoPath, project := defaultAccountFixture(t, "codex", "work")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"work\"\n")

	req := CreateSessionRequest{Title: "claude-session", RepoPath: repoPath, Program: "claude"}
	require.NoError(t, applyDefaultAccount(&config.Config{}, &req))
	assert.Empty(t, req.Account,
		"claude's registry has no entry, so the session keeps the ambient identity rather than borrowing codex's")
}

// TestAnUnregisteredProjectDefaultRefusesTheCreate is the #2983 rule applied to
// configuration: a default af cannot honour must fail loudly, never silently fall
// back to whatever identity the agent finds ambiently.
func TestAnUnregisteredProjectDefaultRefusesTheCreate(t *testing.T) {
	_, repoPath, project := defaultAccountFixture(t, "codex", "")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"missing\"\n")

	req := CreateSessionRequest{Title: "doomed", RepoPath: repoPath, Program: "codex"}
	err := applyDefaultAccount(&config.Config{}, &req)
	require.Error(t, err)
	message := err.Error()
	assert.Contains(t, message, "default_accounts.codex", "the refusal names the key the user set")
	assert.Contains(t, message, "af accounts add codex missing", "and how to make it real")
	assert.Contains(t, message, "af config unset default_accounts.codex --project "+repoPath,
		"and how to clear it, because the user never typed --account on this command")
	assert.Empty(t, req.Account, "nothing is applied when the default cannot be honoured")
}

// A program that is not a bare agent invocation resolves to no registry, so
// there is no default to look up — and asking one would be inventing an answer.
func TestNoDefaultIsAppliedForAnUnrecognizedProgram(t *testing.T) {
	_, repoPath, project := defaultAccountFixture(t, "codex", "work")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"work\"\n")

	req := CreateSessionRequest{Title: "wrapped", RepoPath: repoPath, Program: "/opt/wrapper/run --agent codex"}
	require.NoError(t, applyDefaultAccount(&config.Config{}, &req))
	assert.Empty(t, req.Account)
}

// The daemon's always-ensured root agent is deliberately out of scope: its
// command comes from `[root_agent]` config rather than an agent enum, and the
// account boundary fails closed on a launch shape nobody has proven. A key set
// about ordinary creates must not be able to stop a project's guaranteed session
// from coming up.
func TestTheReservedRootAgentCreateTakesNoProjectDefault(t *testing.T) {
	_, repoPath, project := defaultAccountFixture(t, "codex", "work")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"work\"\n")

	req := CreateSessionRequest{Title: "root", RepoPath: repoPath, Program: "codex", allowReserved: true}
	require.NoError(t, applyDefaultAccount(&config.Config{}, &req))
	assert.Empty(t, req.Account, "the root agent keeps the identity it has always had")
}

// TestListAccountsReportsTheProjectDefaults is the catalog half: the pickers
// preselect what the create will apply, from the same resolver, so the two cannot
// disagree about which identity a create is about to use.
func TestListAccountsReportsTheProjectDefaults(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "codex", "work")
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"work\"\n")

	defaults := defaultAccountsFor(&config.Config{}, repoPath, []string{"claude", "codex"})
	assert.Equal(t, map[string]string{"codex": "work"}, defaults,
		"only the agents the project actually scoped are reported")

	// The unregistered case is reported too, deliberately: dropping it would hide a
	// misconfiguration behind an "ambient identity" the picker would then be lying
	// about, and the create is about to refuse it by name.
	writeProjectAccounts(t, project, "[default_accounts]\ncodex = \"gone\"\n")
	assert.Equal(t, map[string]string{"codex": "gone"},
		defaultAccountsFor(&config.Config{}, repoPath, []string{"codex"}))
	assert.NoDirExists(t, filepath.Join(home, "accounts", "codex", "gone"))
}
