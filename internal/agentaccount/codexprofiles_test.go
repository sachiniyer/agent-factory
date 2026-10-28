package agentaccount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRegisterCodexSelectedProfileProvider(t *testing.T) {
	const ambientBase = "approval_policy = 'never'\nmodel = 'ambient-model'\n"
	const selected = "profile = 'work'\n[profiles.work]\nmodel_provider = 'custom'\n"
	const inactive = "[profiles.unselected]\nmodel_provider = 'custom'\n"
	for _, tt := range []struct {
		name, ambient, account, want, notice string
	}{
		{"ambient selected", "profile = 'work'\n" + ambientBase + "[profiles.work]\nmodel_provider = 'custom'\n", "", "approval_policy = 'never'\n", `~/.codex/config.toml selected profile "work" sets model_provider`},
		{"account selected", ambientBase, selected, "approval_policy = 'never'\n" + selected, `this account's config.toml selected profile "work" sets model_provider`},
		{"selected provider any value", ambientBase, "profile = 'work'\n[profiles.work]\nmodel_provider = false\n", "approval_policy = 'never'\nprofile = 'work'\n[profiles.work]\nmodel_provider = false\n", `selected profile "work" sets model_provider`},
		{"inactive ambient", ambientBase + inactive, "", "approval_policy = 'never'\nmodel = 'ambient-model'\n", ""},
		{"inactive account", ambientBase, inactive, "approval_policy = 'never'\nmodel = 'ambient-model'\n" + inactive, ""},
		{"selected default provider", ambientBase, "profile = 'work'\n[profiles.work]\nmodel_reasoning_effort = 'high'\n", "approval_policy = 'never'\nmodel = 'ambient-model'\nprofile = 'work'\n[profiles.work]\nmodel_reasoning_effort = 'high'\n", ""},
		{"unresolved selection", ambientBase, "profile = 'missing'\n", "approval_policy = 'never'\nprofile = 'missing'\n", `selected profile "missing" could not be verified`},
		{"malformed selection", ambientBase, "profile = 123\n", "approval_policy = 'never'\nprofile = 123\n", "selected profile could not be verified"},
		{"existing model", ambientBase, "model = 'account-model'\n" + selected, "approval_policy = 'never'\nmodel = 'account-model'\n" + selected, `selected profile "work" sets model_provider`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ambientHome := t.TempDir()
			t.Setenv("HOME", ambientHome)
			source := filepath.Join(ambientHome, ".codex", "config.toml")
			require.NoError(t, os.MkdirAll(filepath.Dir(source), 0700))
			require.NoError(t, os.WriteFile(source, []byte(tt.ambient), 0600))
			home := t.TempDir()
			dir := filepath.Join(home, "accounts", "codex", "work")
			require.NoError(t, os.MkdirAll(dir, 0700))
			path := filepath.Join(dir, "config.toml")
			if tt.account != "" {
				require.NoError(t, os.WriteFile(path, []byte(tt.account), 0600))
			}
			_, err := Register(home, "codex", "work")
			require.NoError(t, err)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, tt.want, string(data))
			notices, err := CheckLoginPreconditions("codex", dir)
			require.NoError(t, err)
			joined := strings.Join(notices, "\n")
			if tt.notice != "" {
				require.Contains(t, joined, "model not seeded: ")
				require.Contains(t, joined, tt.notice)
			} else {
				require.NotContains(t, joined, "model not seeded:")
			}
			old := time.Unix(1000, 0)
			require.NoError(t, os.Chtimes(path, old, old))
			_, err = Register(home, "codex", "work")
			require.NoError(t, err)
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, old, info.ModTime())
		})
	}
}
