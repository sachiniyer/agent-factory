package agentaccount

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexApprovalWarningValidatesPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, policy string
		valid        bool
	}{
		{"untrusted outside current schema", "'untrusted'", false}, {"on failure outside current schema", "'on-failure'", false},
		{"never", "'never'", true}, {"interactive", "'on-request'", true},
		{"number", "123", false}, {"boolean", "true", false},
		{"typo", "'typo'", false}, {"empty", "''", false},
		{"case", "'Never'", false}, {"whitespace", "' never '", false},
		{"array", "['never']", false}, {"unknown table", "{ other = true }", false},
		{"granular", "{ granular = { sandbox_approval = false, rules = true, mcp_elicitations = false } }", true},
		{"granular optional", "{ granular = { sandbox_approval = false, rules = true, mcp_elicitations = false, request_permissions = true, skill_approval = false } }", true},
		{"granular missing field", "{ granular = { rules = true } }", false},
		{"granular wrong type", "{ granular = { sandbox_approval = 123, rules = true, mcp_elicitations = false } }", false},
		{"granular optional wrong type", "{ granular = { sandbox_approval = false, rules = true, mcp_elicitations = false, request_permissions = 'yes' } }", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			original := "approval_policy = " + tt.policy + "\n"
			require.NoError(t, os.WriteFile(path, []byte(original), 0600))
			warning := CodexApprovalWarning("work", dir)
			if tt.valid {
				require.Empty(t, warning)
			} else {
				require.NotEmpty(t, warning)
				for _, want := range []string{"work", path, "approval_policy", "could not be verified"} {
					require.Contains(t, warning, want)
				}
				require.NotContains(t, warning, "has no top-level")
				for _, accepted := range []string{"on-request", "never", "granular"} {
					require.Contains(t, warning, accepted)
				}
			}
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, original, string(data))
		})
	}
}
