package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigBackendAliasesGetAndListCanonicalNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	leaveAmbientRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte(`
ssh_host_key_verification = "insecure"
docker_mount_agent_credentials = true
sandbox_ssh = "ssh old"

[ssh]
host_key_verification = "accept-new"
[docker]
mount_agent_credentials = false
[sandbox]
ssh = ""
`), 0o644))

	setConfigGetReadFlags(t, "", false, false)
	for _, pair := range []struct {
		canonical string
		legacy    string
		want      string
	}{
		{canonical: "ssh.host_key_verification", legacy: "ssh_host_key_verification", want: "accept-new\n"},
		{canonical: "docker.mount_agent_credentials", legacy: "docker_mount_agent_credentials", want: "false\n"},
		{canonical: "sandbox.ssh", legacy: "sandbox_ssh", want: "\n"},
	} {
		canonical, err := runConfigGetForTest(t, pair.canonical)
		require.NoError(t, err)
		legacy, err := runConfigGetForTest(t, pair.legacy)
		require.NoError(t, err)
		assert.Equal(t, pair.want, canonical)
		assert.Equal(t, canonical, legacy)
	}

	setConfigListReadFlags(t, "", false, false)
	listed, err := runConfigListForTest(t)
	require.NoError(t, err)
	for _, key := range []string{"ssh.host_key_verification", "docker.mount_agent_credentials", "sandbox.ssh"} {
		assert.Contains(t, listed, key)
	}
	for _, key := range []string{"ssh_host_key_verification", "docker_mount_agent_credentials", "sandbox_ssh"} {
		for _, line := range strings.Split(listed, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				assert.NotEqual(t, key, fields[0], "list must not add a second row for %s", key)
			}
		}
	}
}

func TestConfigBackendCanonicalGetStaysOnGlobalOnlyPath(t *testing.T) {
	_, repoRoot := setupConfigExplainCommandTest(t, "schema_version = 1\n[ssh]\nhost_key_verification = \"accept-new\"\n")
	writeCommandTestInRepoConfig(t, repoRoot, "this is not valid TOML\n")
	t.Chdir(repoRoot)
	setConfigGetReadFlags(t, "", false, false)

	canonical, err := runConfigGetForTest(t, "ssh.host_key_verification")
	require.NoError(t, err)
	legacy, err := runConfigGetForTest(t, "ssh_host_key_verification")
	require.NoError(t, err)
	assert.Equal(t, "accept-new\n", canonical)
	assert.Equal(t, canonical, legacy)
}

func TestConfigBackendLegacySetAndUnsetOperateOnCanonicalWinner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	leaveAmbientRepo(t)
	path := filepath.Join(home, config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte("# keep\nssh_host_key_verification = \"insecure\"\n[ssh]\nhost_key_verification = \"strict\"\n"), 0o644))

	oldSetProject, oldUnsetProject, oldJSON := configSetProjectFlag, configUnsetProjectFlag, configJSONFlag
	t.Cleanup(func() {
		configSetProjectFlag, configUnsetProjectFlag, configJSONFlag = oldSetProject, oldUnsetProject, oldJSON
	})
	configSetProjectFlag, configUnsetProjectFlag, configJSONFlag = "", "", false

	setCmd := &cobra.Command{}
	var setOut bytes.Buffer
	setCmd.SetOut(&setOut)
	setCmd.SetErr(&setOut)
	require.NoError(t, configSetCmd.RunE(setCmd, []string{"ssh_host_key_verification", "accept-new"}))
	assert.Contains(t, setOut.String(), "set ssh.host_key_verification = accept-new")

	unsetCmd := &cobra.Command{}
	var unsetOut bytes.Buffer
	unsetCmd.SetOut(&unsetOut)
	unsetCmd.SetErr(&unsetOut)
	require.NoError(t, configUnsetCmd.RunE(unsetCmd, []string{"ssh_host_key_verification"}))
	assert.Contains(t, unsetOut.String(), "cleared ssh.host_key_verification")

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(written), "# keep")
	assert.NotContains(t, string(written), "ssh_host_key_verification")
	assert.NotContains(t, string(written), "host_key_verification =")
}
