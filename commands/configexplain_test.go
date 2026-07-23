package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setConfigGetReadFlags(t *testing.T, project string, explain, jsonMode bool) {
	t.Helper()
	oldProject, oldExplain, oldJSON := configGetProjectFlag, configGetExplainFlag, configJSONFlag
	configGetProjectFlag, configGetExplainFlag, configJSONFlag = project, explain, jsonMode
	t.Cleanup(func() {
		configGetProjectFlag, configGetExplainFlag, configJSONFlag = oldProject, oldExplain, oldJSON
	})
}

func setConfigListReadFlags(t *testing.T, project string, explain, jsonMode bool) {
	t.Helper()
	oldProject, oldExplain, oldJSON := configListProjectFlag, configListExplainFlag, configJSONFlag
	configListProjectFlag, configListExplainFlag, configJSONFlag = project, explain, jsonMode
	t.Cleanup(func() {
		configListProjectFlag, configListExplainFlag, configJSONFlag = oldProject, oldExplain, oldJSON
	})
}

func setupConfigExplainCommandTest(t *testing.T, globalTOML string) (home, repoRoot string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte(globalTOML), 0644))
	repoRoot = t.TempDir()
	require.NoError(t, exec.Command("git", "-C", repoRoot, "init", "-q").Run())
	return home, repoRoot
}

func writeCommandTestInRepoConfig(t *testing.T, repoRoot, contents string) {
	t.Helper()
	dir := filepath.Join(repoRoot, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.TomlConfigFileName), []byte(contents), 0644))
}

func runConfigGetForTest(t *testing.T, key string) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := configGetCmd.RunE(cmd, []string{key})
	return out.String(), err
}

func runConfigListForTest(t *testing.T) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := configListCmd.RunE(cmd, nil)
	return out.String(), err
}

func TestConfigGetExplainKeepsBareGlobalContract(t *testing.T) {
	_, _ = setupConfigExplainCommandTest(t, "schema_version = 1\ndefault_program = \"codex\"\n")

	setConfigGetReadFlags(t, "", false, false)
	bare, err := runConfigGetForTest(t, "default_program")
	require.NoError(t, err)
	assert.Equal(t, "codex\n", bare)

	configGetExplainFlag = true
	explained, err := runConfigGetForTest(t, "default_program")
	require.NoError(t, err)
	assert.Contains(t, explained, "scope: global defaults")
	assert.Contains(t, explained, "runtime: on-disk config · running daemon value not checked")
	assert.Contains(t, explained, "default_program = codex")
	assert.Contains(t, explained, "policy: replace · built-in < global")
	assert.Contains(t, explained, "global")
	assert.Contains(t, explained, "winner · highest-precedence present allowed source")
}

func TestConfigGetProjectPathResolvesExistingLayersWithoutRegistering(t *testing.T) {
	home, repoRoot := setupConfigExplainCommandTest(t, "schema_version = 1\ndefault_program = \"codex\"\n")
	writeCommandTestInRepoConfig(t, repoRoot, `
default_program = "aider"

[program_overrides]
codex = "/repo/codex"
`)
	subdir := filepath.Join(repoRoot, "nested")
	require.NoError(t, os.Mkdir(subdir, 0755))

	setConfigGetReadFlags(t, subdir, false, false)
	output, err := runConfigGetForTest(t, "default_program")
	require.NoError(t, err)
	assert.Equal(t, "aider\n", output)

	configGetExplainFlag = true
	output, err = runConfigGetForTest(t, "default_program")
	require.NoError(t, err)
	assert.Contains(t, output, "project: "+repoRoot)
	assert.Contains(t, output, "legacy repo")
	assert.Contains(t, output, "repo-shared")
	assert.Contains(t, output, "repo-shared")
	assert.Contains(t, output, "winner · highest-precedence present allowed source")

	_, statErr := os.Stat(filepath.Join(home, config.ProjectRegistryDirName))
	assert.True(t, os.IsNotExist(statErr), "a path selector must not register durable identity in stage two")
	_, statErr = os.Stat(filepath.Join(home, "repos"))
	assert.True(t, os.IsNotExist(statErr), "a project config read must not persist load-observation state")
}

func TestConfigGetExplainJSONCarriesContextAndCompleteTrace(t *testing.T) {
	_, repoRoot := setupConfigExplainCommandTest(t, "schema_version = 1\ndefault_program = \"codex\"\n")
	writeCommandTestInRepoConfig(t, repoRoot, "default_program = \"aider\"\n")
	setConfigGetReadFlags(t, repoRoot, true, true)

	output, err := runConfigGetForTest(t, "default_program")
	require.NoError(t, err)
	var envelope struct {
		Data  configGetExplanation `json:"data"`
		Error any                  `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &envelope))
	assert.Nil(t, envelope.Error)
	assert.Equal(t, "project", envelope.Data.Context.Scope)
	assert.Equal(t, repoRoot, envelope.Data.Context.ProjectRoot)
	assert.Equal(t, "on-disk", envelope.Data.Context.View)
	assert.False(t, envelope.Data.Context.RunningValueChecked)
	assert.Equal(t, "default_program", envelope.Data.Key)
	assert.Equal(t, "aider", envelope.Data.Value)
	assert.Equal(t, "claude", envelope.Data.Default)
	require.NotNil(t, envelope.Data.Winner)
	assert.Equal(t, config.SourceRepoShared.String(), envelope.Data.Winner.Layer)
	assert.Len(t, envelope.Data.Candidates, 4)
	for _, candidate := range envelope.Data.Candidates {
		assert.NotEmpty(t, candidate.Result)
		assert.NotEmpty(t, candidate.Reason)
		if candidate.Layer == config.SourceRepoShared.String() {
			assert.Equal(t, "TOML", candidate.Format)
		}
	}
}

func TestConfigGetDottedLeafUsesResolvedProvenance(t *testing.T) {
	_, repoRoot := setupConfigExplainCommandTest(t, `
schema_version = 1
default_program = "claude"

[program_overrides]
codex = "/global/codex"
`)
	writeCommandTestInRepoConfig(t, repoRoot, "[program_overrides]\ncodex = \"/repo/codex\"\n")
	setConfigGetReadFlags(t, repoRoot, false, false)

	output, err := runConfigGetForTest(t, "program_overrides.codex")
	require.NoError(t, err)
	assert.Equal(t, "/repo/codex\n", output)

	configGetExplainFlag = true
	output, err = runConfigGetForTest(t, "program_overrides.codex")
	require.NoError(t, err)
	assert.Contains(t, output, "program_overrides.codex = /repo/codex")
	assert.Contains(t, output, "global")
	assert.Contains(t, output, "shadowed · leaf is overridden by higher-precedence repo-shared")
	assert.Contains(t, output, "repo-shared")
	assert.Contains(t, output, "winner · supplies the effective leaf")
}

func TestConfigListProjectIncludesRepoOnlyKeysButBareListDoesNot(t *testing.T) {
	_, repoRoot := setupConfigExplainCommandTest(t, "schema_version = 1\ndefault_program = \"codex\"\n")
	writeCommandTestInRepoConfig(t, repoRoot, "backend = \"docker\"\n[docker]\nimage = \"af-test\"\n")

	setConfigListReadFlags(t, "", false, false)
	globalOutput, err := runConfigListForTest(t)
	require.NoError(t, err)
	assert.NotContains(t, globalOutput, "backend")
	assert.NotContains(t, globalOutput, "post_worktree_commands")

	configListProjectFlag = repoRoot
	projectOutput, err := runConfigListForTest(t)
	require.NoError(t, err)
	assert.Contains(t, projectOutput, "backend")
	assert.Contains(t, projectOutput, "docker")
	assert.Contains(t, projectOutput, "post_worktree_commands")
}

func TestConfigGetProjectReportsLocalForAbsentBackend(t *testing.T) {
	_, repoRoot := setupConfigExplainCommandTest(t, "schema_version = 1\ndefault_program = \"codex\"\n")
	setConfigGetReadFlags(t, repoRoot, false, false)

	output, err := runConfigGetForTest(t, "backend")
	require.NoError(t, err)
	assert.Equal(t, config.BackendLocal+"\n", output)

	configGetExplainFlag = true
	output, err = runConfigGetForTest(t, "backend")
	require.NoError(t, err)
	assert.Contains(t, output, "backend = "+config.BackendLocal)
	assert.Contains(t, output, "built-in")
	assert.Contains(t, output, "winner · highest-precedence present allowed source")
}

func TestConfigListExplainJSONContainsEveryProjectResolution(t *testing.T) {
	_, repoRoot := setupConfigExplainCommandTest(t, "schema_version = 1\ndefault_program = \"codex\"\n")
	setConfigListReadFlags(t, repoRoot, true, true)

	output, err := runConfigListForTest(t)
	require.NoError(t, err)
	var envelope struct {
		Data  configListExplanation `json:"data"`
		Error any                   `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &envelope))
	assert.Nil(t, envelope.Error)
	assert.Equal(t, repoRoot, envelope.Data.Context.ProjectRoot)
	assert.Len(t, envelope.Data.Values, len(config.AllManifest()))
	for _, value := range envelope.Data.Values {
		assert.NotEmpty(t, value.Precedence)
		assert.NotEmpty(t, value.Candidates)
	}
}

func TestConfigExplainPreservesSelectedPathSpellingForEverySourceReference(t *testing.T) {
	container := t.TempDir()
	realParent := filepath.Join(container, "real")
	selectedParent := filepath.Join(container, "selected")
	require.NoError(t, os.Mkdir(realParent, 0755))
	require.NoError(t, os.Symlink(realParent, selectedParent))

	home := filepath.Join(selectedParent, "home")
	repoRoot := filepath.Join(selectedParent, "repo")
	require.NoError(t, os.MkdirAll(home, 0755))
	require.NoError(t, os.MkdirAll(repoRoot, 0755))
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte(`
schema_version = 1
default_program = "codex"

[program_overrides]
codex = "/global/codex"
`), 0644))
	require.NoError(t, exec.Command("git", "-C", repoRoot, "init", "-q").Run())
	writeCommandTestInRepoConfig(t, repoRoot, `
default_program = "aider"

[program_overrides]
codex = "/repo/codex"
`)
	selector := filepath.Join(repoRoot, "nested")
	require.NoError(t, os.Mkdir(selector, 0755))

	resolvedRepoRoot := pathutil.ResolveForCompare(repoRoot)
	require.NotEqual(t, repoRoot, resolvedRepoRoot,
		"the test must exercise two lexical spellings for one repository")
	setConfigListReadFlags(t, selector, true, true)
	output, err := runConfigListForTest(t)
	require.NoError(t, err)

	var envelope struct {
		Data  configListExplanation `json:"data"`
		Error any                   `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &envelope))
	assert.Nil(t, envelope.Error)
	assert.Equal(t, repoRoot, envelope.Data.Context.ProjectRoot)

	repo, err := config.RepoFromPath(repoRoot)
	require.NoError(t, err)
	wantPaths := map[string]string{
		config.SourceGlobal.String():     filepath.Join(home, config.TomlConfigFileName),
		config.SourceLegacyRepo.String(): filepath.Join(home, "repos", repo.ID, config.ConfigFileName),
		config.SourceRepoShared.String(): filepath.Join(repoRoot, config.InRepoConfigDirName, config.TomlConfigFileName),
	}
	assertSource := func(layer, path string) {
		t.Helper()
		want, hasLocation := wantPaths[layer]
		if !hasLocation {
			assert.Empty(t, path, "the built-in source must not acquire a filesystem location")
			return
		}
		assert.Equal(t, want, path, "source %s must keep the selected/configured spelling", layer)
		assert.NotContains(t, path, resolvedRepoRoot,
			"display paths must not leak the symlink-resolved project spelling")
	}
	for _, value := range envelope.Data.Values {
		if value.Winner != nil {
			assertSource(value.Winner.Layer, value.Winner.Path)
		}
		for _, origin := range value.Origins {
			assertSource(origin.Layer, origin.Path)
		}
		for _, candidate := range value.Candidates {
			assertSource(candidate.Layer, candidate.Path)
		}
	}

	setConfigGetReadFlags(t, selector, true, false)
	human, err := runConfigGetForTest(t, "default_program")
	require.NoError(t, err)
	assert.Contains(t, human, "project: "+repoRoot)
	assert.Contains(t, human, wantPaths[config.SourceGlobal.String()]+":default_program")
	assert.Contains(t, human, wantPaths[config.SourceLegacyRepo.String()]+":default_program")
	assert.Contains(t, human, wantPaths[config.SourceRepoShared.String()]+":default_program")
}

func TestConfigGetProjectRejectsNonRepositoryWithJSONEnvelope(t *testing.T) {
	_, _ = setupConfigExplainCommandTest(t, "schema_version = 1\ndefault_program = \"codex\"\n")
	notRepo := t.TempDir()
	setConfigGetReadFlags(t, notRepo, true, true)

	output, err := runConfigGetForTest(t, "default_program")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to resolve --project path")
	assert.True(t, strings.HasSuffix(output, "\n"))
	var envelope struct {
		Data  any `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &envelope))
	require.NotNil(t, envelope.Error)
	assert.Contains(t, envelope.Error.Message, "failed to resolve --project path")
}
