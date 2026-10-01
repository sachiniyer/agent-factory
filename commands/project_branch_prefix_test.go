package commands

import (
	"bytes"
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigSetProjectBranchPrefixWarnsOnStderr pins the #4539 write contract:
// `af config set --project branch_prefix` still stores the value — existing
// configs stay valid — but stderr names the global prefix that actually
// applies so the write is not mistaken for an applied override.
func TestConfigSetProjectBranchPrefixWarnsOnStderr(t *testing.T) {
	_, repo := setupConfigExplainCommandTest(t, "schema_version = 1\nbranch_prefix = \"global/\"\n")
	t.Setenv("AF_DAEMON_URL", "")
	project, err := config.RegisterProject(repo)
	require.NoError(t, err)
	t.Chdir(repo)

	oldProject := configSetProjectFlag
	configSetProjectFlag = "."
	t.Cleanup(func() { configSetProjectFlag = oldProject })

	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	require.NoError(t, configSetCmd.RunE(cmd, []string{"branch_prefix", "feat/"}))

	assert.Equal(t,
		"branch_prefix is not supported per project yet; the global branch_prefix (global/) applies to all projects. See #4539.\n",
		errOut.String())
	assert.Contains(t, out.String(), "set branch_prefix = feat/")

	cfg, err := config.LoadProjectConfig(project.ID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "feat/", cfg.BranchPrefix, "the warning must not block the write")
}

// TestConfigGetListRepoBranchPrefixMarksIgnoredOverride pins the inspection
// contract: with a stored personal value, --repo reads still resolve the
// GLOBAL prefix as effective and mark it "(project override ignored: not
// supported yet)" rather than silently showing the stored one.
func TestConfigGetListRepoBranchPrefixMarksIgnoredOverride(t *testing.T) {
	_, repo := setupConfigExplainCommandTest(t, "schema_version = 1\nbranch_prefix = \"global/\"\n")
	t.Setenv("AF_DAEMON_URL", "")
	_, err := config.RegisterProject(repo)
	require.NoError(t, err)
	_, err = config.SetProjectConfigValue(repo, "branch_prefix", "feat/")
	require.NoError(t, err)

	oldRepo, oldProject, oldExplain, oldJSON := configGetRepoFlag, configGetProjectFlag, configGetExplainFlag, configJSONFlag
	configGetRepoFlag, configGetProjectFlag, configGetExplainFlag, configJSONFlag = repo, "", false, false
	t.Cleanup(func() {
		configGetRepoFlag, configGetProjectFlag, configGetExplainFlag, configJSONFlag = oldRepo, oldProject, oldExplain, oldJSON
	})
	got, err := runConfigGetForTest(t, "branch_prefix")
	require.NoError(t, err)
	assert.Equal(t, "global/ (project override ignored: not supported yet)\n", got,
		"the effective value is the global prefix, marked — not the stored project value")

	oldLRepo, oldLProject, oldLExplain, oldLJSON := configListRepoFlag, configListProjectFlag, configListExplainFlag, configJSONFlag
	configListRepoFlag, configListProjectFlag, configListExplainFlag, configJSONFlag = repo, "", false, false
	t.Cleanup(func() {
		configListRepoFlag, configListProjectFlag, configListExplainFlag, configJSONFlag = oldLRepo, oldLProject, oldLExplain, oldLJSON
	})
	listed, err := runConfigListForTest(t)
	require.NoError(t, err)
	assert.Contains(t, listed, "global/ (project override ignored: not supported yet)")
	assert.NotContains(t, listed, "feat/")
}
