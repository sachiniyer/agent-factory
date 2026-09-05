package agentaccount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"
)

func TestRegisterCodexRuntimeSettings(t *testing.T) {
	const source = "approval_policy = 'never'\nsandbox_mode = 'danger-full-access'\nmodel = 'gpt-example'\napi_key = 'secret'\ncli_auth_credentials_store = 'keyring'\n[projects.'/private']\ntrust_level = 'trusted'\n"
	const seeded = "approval_policy = 'never'\nsandbox_mode = 'danger-full-access'\nmodel = 'gpt-example'\n"
	for _, tt := range []struct {
		name, ambient, existing, want string
		absent                        bool
	}{
		{"new", source, "", seeded, false},
		{"tables", source, "# Keep me\n[features]\nfast = true\n", seeded + "# Keep me\n[features]\nfast = true\n", false},
		{"existing keys", source, "approval_policy = 'on-request'\nmodel = 'chosen'\n[features]\nfast = true\n", "sandbox_mode = 'danger-full-access'\napproval_policy = 'on-request'\nmodel = 'chosen'\n[features]\nfast = true\n", false},
		{"complete", source, "approval_policy = 'never'\nsandbox_mode = 'read-only'\n", "approval_policy = 'never'\nsandbox_mode = 'read-only'\n", false},
		{"approval only", "approval_policy = 'never'\n", "", "approval_policy = 'never'\n", false},
		{"no source keys preserves account", "model = 'unused'\n", "approval_policy = 'on-request'\n# Keep bytes\n", "approval_policy = 'on-request'\n# Keep bytes\n", false},
		{"multiline string", source, "notes = '''\n[not a table]\n'''\n", seeded + "notes = '''\n[not a table]\n'''\n", false},
		{"model only", "model = 'unused'\n", "", "", true},
		{"absent ambient", "", "", "", true},
		{"invalid ambient", "approval_policy = [", "", "", true},
		{"invalid account", source, "[broken", "[broken", false},
		{"nested only", "[projects.foo]\napproval_policy = 'never'\n", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ambientHome := t.TempDir()
			t.Setenv("HOME", ambientHome)
			t.Setenv("CODEX_HOME", t.TempDir()) // Ambient means ~/.codex, not this lane's account.
			if tt.ambient != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(ambientHome, ".codex"), 0700))
				require.NoError(t, os.WriteFile(filepath.Join(ambientHome, ".codex/config.toml"), []byte(tt.ambient), 0600))
			}
			home := t.TempDir()
			dir := filepath.Join(home, "accounts/codex/work")
			require.NoError(t, os.MkdirAll(dir, 0700))
			path := filepath.Join(dir, "config.toml")
			if tt.existing != "" {
				require.NoError(t, os.WriteFile(path, []byte(tt.existing), 0600))
			}
			_, err := Register(home, "codex", "work")
			require.NoError(t, err)
			data, err := os.ReadFile(path)
			if tt.absent {
				require.True(t, os.IsNotExist(err))
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, string(data))
			if tt.name != "invalid account" {
				var doc map[string]any
				require.NoError(t, toml.Unmarshal(data, &doc))
				require.Contains(t, doc, "approval_policy")
			}
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			old := time.Unix(1000, 0)
			require.NoError(t, os.Chtimes(path, old, old))
			_, err = Register(home, "codex", "work")
			require.NoError(t, err)
			info, err = os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, old, info.ModTime())
		})
	}
}

func TestCodexSettingsNotice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	notices, err := CheckLoginPreconditions("codex", dir)
	require.NoError(t, err)
	joined := strings.Join(notices, "\n")
	for _, want := range []string{filepath.Join(home, ".codex/config.toml"), filepath.Join(dir, "config.toml"), "approval_policy", "sandbox_mode", "Nothing was written", "absent"} {
		require.Contains(t, joined, want)
	}
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex/config.toml"), []byte("approval_policy = 'never'\n"), 0600))
	require.NoError(t, answerLoginPrompts("codex", dir))
	notices, err = CheckLoginPreconditions("codex", dir)
	require.NoError(t, err)
	joined = strings.Join(notices, "\n")
	require.Contains(t, joined, "Existing keys stand")
	require.Contains(t, joined, "approval_policy is present")
	require.NotContains(t, joined, "Nothing was written")
}
