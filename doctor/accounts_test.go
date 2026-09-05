package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckCodexAccounts(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		warn          bool
	}{
		{"missing", "", true}, {"table key", "[features]\napproval_policy = 'never'\n", true},
		{"explicit policy", "approval_policy = 'on-request'\n", false}, {"unparseable", "[broken", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, "accounts/codex/work")
			require.NoError(t, os.MkdirAll(dir, 0700))
			if tt.content != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tt.content), 0600))
			}
			report := &Report{}
			checkCodexAccounts(home, report)
			if !tt.warn {
				require.Empty(t, report.Findings)
				return
			}
			require.Len(t, report.Findings, 1)
			for _, want := range []string{"work", filepath.Join(dir, "config.toml"), "approval_policy", "approval picker"} {
				require.Contains(t, report.Findings[0].Detail, want)
			}
		})
	}
}
