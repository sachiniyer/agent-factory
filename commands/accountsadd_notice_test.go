package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/stretchr/testify/require"
)

func TestAccountsAddPrintsSettingsNotices(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonMode], func(t *testing.T) {
			home := t.TempDir()
			ambientHome := t.TempDir()
			t.Setenv("HOME", ambientHome)
			dir, err := agentaccount.Register(home, "codex", "work")
			require.NoError(t, err)
			source := filepath.Join(ambientHome, ".codex", "config.toml")
			require.NoError(t, os.MkdirAll(filepath.Dir(source), 0700))
			require.NoError(t, os.WriteFile(source, []byte("approval_policy = 'never'\n"), 0600))
			args := []string{"add", "codex", "work"}
			if jsonMode {
				args = append(args, "--json")
			}
			stdout, stderr, err := runAccountsInHome(t, home, args...)
			require.NoError(t, err)
			if jsonMode {
				requireEnvelope(t, stdout, "stdout")
			} else {
				require.Equal(t, dir+"\n", stdout)
			}
			notices, err := agentaccount.CheckLoginPreconditions("codex", dir)
			require.NoError(t, err)
			for _, notice := range notices {
				require.Contains(t, strings.Split(stderr, "\n"), notice)
			}
			require.Contains(t, stderr, source)
			require.Contains(t, stderr, filepath.Join(dir, "config.toml"))
		})
	}
}

func TestAccountsAddKeyringStillSucceeds(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonMode], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			dir, err := agentaccount.Register(home, "codex", "work")
			require.NoError(t, err)
			path := filepath.Join(dir, "config.toml")
			const config = "cli_auth_credentials_store = 'keyring'\n"
			require.NoError(t, os.WriteFile(path, []byte(config), 0600))
			args := []string{"add", "codex", "work"}
			if jsonMode {
				args = append(args, "--json")
			}
			stdout, stderr, err := runAccountsInHome(t, home, args...)
			require.NoError(t, err)
			if jsonMode {
				env := requireEnvelope(t, stdout, "stdout")
				require.Nil(t, env.Error)
				require.Contains(t, string(env.Data), dir)
			} else {
				require.Equal(t, dir+"\n", stdout)
			}
			require.Contains(t, stderr, "Registration seeds missing")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, config, string(data))
			_, err = agentaccount.CheckLoginPreconditions("codex", dir)
			require.ErrorContains(t, err, "machine-wide keyring identity")
		})
	}
}
