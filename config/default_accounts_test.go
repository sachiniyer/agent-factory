package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// default_accounts is the project-scoped credential-identity key (#3386). These
// tests pin the three properties that make it safe to have at all: it is keyed by
// AGENT so a project cannot select a claude account for a codex session, it is
// admitted only where identity policy belongs (global and the machine-local
// personal-project layer, never a checked-in repo file), and its entries are
// validated at the same boundary every other enumerated key is.

func TestDefaultAccountsMergePerAgentAcrossLayers(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "[default_accounts]\ncodex = \"work\"\nclaude = \"personal\"\n")
	writePersonalConfig(t, project.ID, "[default_accounts]\ncodex = \"side\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "side", resolved.DefaultAccounts["codex"],
		"the personal per-project entry beats the global one for that agent")
	assert.Equal(t, "personal", resolved.DefaultAccounts["claude"],
		"an agent the project did not override keeps the global entry — the key merges per agent, like program_overrides")
}

func TestDefaultAccountsExplainNamesThePersonalProjectLayer(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "[default_accounts]\ncodex = \"work\"\nclaude = \"personal\"\n")
	writePersonalConfig(t, project.ID, "[default_accounts]\ncodex = \"side\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	value, ok := resolved.ResolvedValue("default_accounts")
	require.True(t, ok, "--explain must be able to trace default_accounts like every other manifest key")
	require.NotNil(t, value.Origins, "a map-merged key traces per agent, so the trace must attribute each one")
	assert.Equal(t, SourceProjectPersonal.String(), value.Origins["codex"].Layer)
	assert.Equal(t, SourceGlobal.String(), value.Origins["claude"].Layer)
}

func TestDefaultAccountsRejectsAnAgentThatCannotBeScoped(t *testing.T) {
	_, _, project := registeredTestProject(t)
	// aider has no credential-root variable, so an account can never scope it: a
	// default naming it would be silently inert, which is exactly the shape this
	// feature exists to remove.
	writePersonalConfig(t, project.ID, "[default_accounts]\naider = \"work\"\n")

	_, err := LoadProjectConfig(project.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aider")
	assert.Contains(t, err.Error(), "default_accounts")
}

func TestDefaultAccountsRejectsAMalformedAccountName(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[default_accounts]\ncodex = \"../elsewhere\"\n")

	_, err := LoadProjectConfig(project.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_accounts.codex")
}

func TestGlobalConfigValidatesDefaultAccounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "[default_accounts]\naider = \"work\"\n")

	_, err := LoadConfig()
	require.Error(t, err, "the global loader validates default_accounts on the same rules the project loader does")
	assert.Contains(t, err.Error(), "default_accounts")
}

func TestSetProjectConfigValueWritesDefaultAccount(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)

	res, err := SetProjectConfigValue(project.ID, "default_accounts.codex", "work")
	require.NoError(t, err, "default_accounts must be settable per project like every other personal key")
	assert.Equal(t, "work", res.Value)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "work", resolved.DefaultAccounts["codex"])
}

func TestSetProjectConfigValueRefusesADefaultAccountForAnUnscopableAgent(t *testing.T) {
	_, _, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "default_accounts.aider", "work")
	require.Error(t, err, "the write path runs the loader's own validator, so a typo fails at the command that took it")
	assert.Contains(t, err.Error(), "aider")
}

func TestSetGlobalConfigValueWritesDefaultAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "schema_version = 1\n")

	res, err := SetGlobalConfigValue("default_accounts.claude", "personal")
	require.NoError(t, err)
	assert.Equal(t, "personal", res.Value)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "personal", cfg.DefaultAccounts["claude"])
}

func TestSetGlobalConfigValueWarnsAboutAnUnregisteredDefaultAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "schema_version = 1\n")

	res, err := SetGlobalConfigValue("default_accounts.claude", "personal")
	require.NoError(t, err)
	require.NotEmpty(t, res.Warnings,
		"a default naming an account that is not registered must say so at the command that took it")
	joined := strings.Join(res.Warnings, "\n")
	assert.Contains(t, joined, "af accounts add claude personal",
		"the warning names the registry command that makes the default real")
}

func TestDefaultAccountLayersForAttributesTheWinningLayer(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "[default_accounts]\ncodex = \"work\"\nclaude = \"shared\"\n")
	writePersonalConfig(t, project.ID, "[default_accounts]\ncodex = \"side\"\n")
	global, err := LoadConfig()
	require.NoError(t, err)

	codex, codexGlobal := DefaultAccountLayersFor(global, repoRoot, "codex")
	assert.Equal(t, "side", codex.Name)
	assert.Equal(t, SourceProjectPersonal, codex.Layer, "the project overrode it, so the refusal must name that file")
	assert.Contains(t, codex.Source(), "default_accounts.codex")
	assert.Contains(t, codex.ClearHint(repoRoot), "--project "+repoRoot)
	assert.Equal(t, "work", codexGlobal.Name, "the global layer is reported separately as the no-repo fallback")

	claude, _ := DefaultAccountLayersFor(global, repoRoot, "claude")
	assert.Equal(t, "shared", claude.Name)
	assert.Equal(t, SourceGlobal, claude.Layer,
		"an agent the project did not override resolves through the global layer, and the trace must say so")
	assert.NotContains(t, claude.ClearHint(repoRoot), "--project")
}

func TestDefaultAccountLayersForFallsBackWhenTheRepoDoesNotResolve(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "[default_accounts]\ncodex = \"work\"\n")
	global, err := LoadConfig()
	require.NoError(t, err)

	project, globalLayer := DefaultAccountLayersFor(global, filepath.Join(t.TempDir(), "not-a-repo"), "codex")
	assert.Empty(t, project.Name, "an unresolvable repo contributes no project layer")
	assert.Equal(t, "work", globalLayer.Name,
		"the global default still applies, exactly as defaultProgramFor falls back rather than failing the create")
}

func TestDefaultAccountLayersForIgnoresAnAgentWithNoEntry(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	writeGlobalTOML(t, home, "[default_accounts]\ncodex = \"work\"\n")
	global, err := LoadConfig()
	require.NoError(t, err)

	project, globalLayer := DefaultAccountLayersFor(global, repoRoot, "claude")
	assert.Empty(t, project.Name)
	assert.Empty(t, globalLayer.Name,
		"an account belongs to ONE agent, so a codex default must never be offered to a claude session")
}

func TestCheckDefaultAccountRefusesAnUnregisteredAccountNamingTheKey(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[default_accounts]\ncodex = \"work\"\n")
	global, err := LoadConfig()
	require.NoError(t, err)

	selection, _ := DefaultAccountLayersFor(global, repoRoot, "codex")
	err = CheckDefaultAccount(home, repoRoot, selection)
	require.Error(t, err, "a default that cannot be honoured must fail the create, never fall back to the ambient identity")
	message := err.Error()
	assert.Contains(t, message, "default_accounts.codex", "the refusal names the key the user set")
	assert.Contains(t, message, "af accounts add codex work", "and how to make it real")
	assert.Contains(t, message, "af config unset default_accounts.codex --project "+repoRoot,
		"and how to clear it for this project")
}

func TestCheckDefaultAccountAcceptsARegisteredAccount(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[default_accounts]\ncodex = \"work\"\n")
	require.NoError(t, os.MkdirAll(filepath.Join(home, "accounts", "codex", "work"), 0o700))
	global, err := LoadConfig()
	require.NoError(t, err)

	selection, _ := DefaultAccountLayersFor(global, repoRoot, "codex")
	require.NoError(t, CheckDefaultAccount(home, repoRoot, selection))
}

func TestCheckDefaultAccountIsSilentWhenNoDefaultApplies(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	require.NoError(t, CheckDefaultAccount(home, repoRoot, DefaultAccountSelection{Agent: "codex"}),
		"no default is the ordinary state, not a failure")
}

// The catalog path (what a picker preselects) and the create path (what the
// daemon applies) are two functions, and a picker that promised an identity the
// create did not use would be worse than no preselection at all. This pins that
// they agree — including on the fallbacks, which is where two implementations of
// a precedence usually diverge.
func TestResolvedDefaultAccountsAgreesWithThePerAgentResolution(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "[default_accounts]\ncodex = \"work\"\nclaude = \"shared\"\n")
	writePersonalConfig(t, project.ID, "[default_accounts]\ncodex = \"side\"\n")
	global, err := LoadConfig()
	require.NoError(t, err)

	for _, repoPath := range []string{repoRoot, "", filepath.Join(t.TempDir(), "not-a-repo")} {
		effective := ResolvedDefaultAccountsFor(global, repoPath)
		for _, agent := range []string{"claude", "codex", "gemini"} {
			project, globalLayer := DefaultAccountLayersFor(global, repoPath, agent)
			want := project.Name
			if want == "" {
				want = globalLayer.Name
			}
			assert.Equal(t, want, effective[agent],
				"catalog and create must resolve %s identically for repoPath %q", agent, repoPath)
		}
	}

	assert.Equal(t, "side", ResolvedDefaultAccountsFor(global, repoRoot)["codex"],
		"and the answer is the project's, not the global one")
}
