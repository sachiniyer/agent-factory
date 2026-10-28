package agentaccount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRegisterCodexEffectiveSettings(t *testing.T) {
	const root = "approval_policy = 'never'\nsandbox_mode = 'danger-full-access'\nmodel = 'root-model'\n"
	const workspace = "approval_policy = 'never'\nsandbox_mode = 'workspace-write'\n[sandbox_workspace_write]\nnetwork_access = true\n"
	const bom = "\xef\xbb\xbf"
	for _, tt := range []struct {
		name, source, account, want, notice string
		absent                              bool
	}{
		{"profile overrides", "profile = 'work'\n" + root + "[profiles.work]\napproval_policy = 'on-request'\nsandbox_mode = 'read-only'\nmodel = 'profile-model'\n", "", "approval_policy = 'on-request'\nsandbox_mode = 'read-only'\nmodel = 'profile-model'\n", "", false},
		{"profile fallback", "profile = 'work'\n" + root + "[profiles.work]\nmodel = 'profile-model'\n", "", "approval_policy = 'never'\nsandbox_mode = 'danger-full-access'\nmodel = 'profile-model'\n", "", false},
		{"profile shadows invalid roots", "profile = 'work'\napproval_policy = 'typo'\nsandbox_mode = 'typo'\n[profiles.work]\napproval_policy = 'never'\nsandbox_mode = 'read-only'\n", "", "approval_policy = 'never'\nsandbox_mode = 'read-only'\n", "", false},
		{"unresolved ambient selection", "profile = 'missing'\n" + root, "", "", `selected profile "missing" could not be verified`, true},
		{"malformed ambient selection", "profile = false\n" + root, "", "", "selected profile could not be verified", true},
		{"account mode stands", workspace, "sandbox_mode = 'read-only'\n", "approval_policy = 'never'\nsandbox_mode = 'read-only'\n", "", false},
		{"account unrestricted stands", workspace, "sandbox_mode = 'danger-full-access'\n", "approval_policy = 'never'\nsandbox_mode = 'danger-full-access'\n", "", false},
		{"account effective mode stands", workspace, "profile = 'work'\nsandbox_mode = 'workspace-write'\n[profiles.work]\nsandbox_mode = 'read-only'\n", "approval_policy = 'never'\nprofile = 'work'\nsandbox_mode = 'workspace-write'\n[profiles.work]\nsandbox_mode = 'read-only'\n", "", false},
		{"account profile policy stands", root, "profile = 'work'\n[profiles.work]\napproval_policy = 'on-request'\nsandbox_mode = 'read-only'\nmodel = 'own-model'\n", "profile = 'work'\n[profiles.work]\napproval_policy = 'on-request'\nsandbox_mode = 'read-only'\nmodel = 'own-model'\n", "", false},
		{"invalid sandbox", "approval_policy = 'never'\nsandbox_mode = 'typo'\n", "", "", "sandbox_mode could not be verified", true},
		{"invalid sandbox type", "sandbox_mode = 123\n", "", "", "sandbox_mode could not be verified", true},
		{"invalid profile sandbox", "profile = 'work'\n" + root + "[profiles.work]\nsandbox_mode = 'typo'\n", "", "", "sandbox_mode could not be verified", true},
		{"invalid workspace 0", "approval_policy = 'never'\nsandbox_mode = 'workspace-write'\n[sandbox_workspace_write]\nnetwork_access = 'yes'\n", "", "", "sandbox_workspace_write could not be verified", true},
		{"invalid workspace 1", "approval_policy = 'never'\nsandbox_mode = 'workspace-write'\n[sandbox_workspace_write]\nwritable_roots = 'foo'\n", "", "", "sandbox_workspace_write could not be verified", true},
		{"invalid workspace 2", "approval_policy = 'never'\nsandbox_mode = 'workspace-write'\n[sandbox_workspace_write]\nwritable_roots = [123]\n", "", "", "sandbox_workspace_write could not be verified", true},
		{"invalid workspace 3", "approval_policy = 'never'\nsandbox_mode = 'workspace-write'\n[sandbox_workspace_write]\nexclude_tmpdir_env_var = 1\n", "", "", "sandbox_workspace_write could not be verified", true},
		{"invalid workspace 4", "approval_policy = 'never'\nsandbox_mode = 'workspace-write'\n[sandbox_workspace_write]\nexclude_slash_tmp = 'false'\n", "", "", "sandbox_workspace_write could not be verified", true},
		{"BOM preserved", "approval_policy = 'never'\n", bom + "# Keep\n[features]\nfast = true\n", bom + "approval_policy = 'never'\n# Keep\n[features]\nfast = true\n", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ambientHome := t.TempDir()
			t.Setenv("HOME", ambientHome)
			source := filepath.Join(ambientHome, ".codex", "config.toml")
			require.NoError(t, os.MkdirAll(filepath.Dir(source), 0700))
			require.NoError(t, os.WriteFile(source, []byte(tt.source), 0600))
			home := t.TempDir()
			dir := filepath.Join(home, "accounts", "codex", "work")
			path := filepath.Join(dir, "config.toml")
			require.NoError(t, os.MkdirAll(dir, 0700))
			if tt.account != "" {
				require.NoError(t, os.WriteFile(path, []byte(tt.account), 0600))
			}
			_, err := Register(home, "codex", "work")
			require.NoError(t, err)
			data, err := os.ReadFile(path)
			if tt.absent {
				require.True(t, os.IsNotExist(err))
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, string(data))
			}
			notices, err := CheckLoginPreconditions("codex", dir)
			require.NoError(t, err)
			if tt.notice != "" {
				joined := strings.Join(notices, "\n")
				require.Contains(t, joined, "Nothing was written")
				require.Contains(t, joined, tt.notice)
				if strings.Contains(tt.notice, "sandbox_mode") {
					for _, mode := range []string{"read-only", "workspace-write", "danger-full-access"} {
						require.Contains(t, joined, mode)
					}
				}
			}
			if !tt.absent {
				old := time.Unix(1000, 0)
				require.NoError(t, os.Chtimes(path, old, old))
				_, err = Register(home, "codex", "work")
				require.NoError(t, err)
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.Equal(t, old, info.ModTime())
			}
		})
	}
}

func TestCodexApprovalWarningUsesEffectivePolicy(t *testing.T) {
	for _, tt := range []struct {
		doc  string
		warn bool
	}{
		{"profile = 'work'\n[profiles.work]\napproval_policy = 'never'\n", false},
		{"profile = 'work'\napproval_policy = 'never'\n[profiles.work]\napproval_policy = 'typo'\n", true},
		{"profile = 'missing'\napproval_policy = 'never'\n", true},
	} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tt.doc), 0600))
		warning := CodexApprovalWarning("work", dir)
		if tt.warn {
			require.NotEmpty(t, warning)
		} else {
			require.Empty(t, warning)
		}
	}
}
