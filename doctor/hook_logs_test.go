package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHomeHealthHookLogs(t *testing.T) {
	const threshold = int64(100 * 1024 * 1024)
	for _, tc := range []struct {
		name string
		size int64
		want CheckStatus
	}{
		{"missing", -1, StatusPass},
		{"empty", 0, StatusPass},
		{"below", threshold - 1, StatusPass},
		{"at threshold", threshold, StatusPass},
		{"over threshold", threshold + 1, StatusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, "logs", "hooks")
			if tc.size >= 0 {
				require.NoError(t, os.MkdirAll(dir, 0o700))
				for i, name := range []string{"post-worktree-kept.log", "on-archive-kept.log"} {
					file, err := os.Create(filepath.Join(dir, name))
					require.NoError(t, err)
					size := tc.size / 2
					if i == 1 {
						size = tc.size - size
					}
					require.NoError(t, file.Truncate(size))
					require.NoError(t, file.Close())
				}
			}
			report := &Report{}
			checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
			row := findCheck(t, report, "hook logs")
			require.Equal(t, tc.want, row.Status)
			require.Contains(t, row.Detail, dir)
			if tc.want == StatusWarn {
				require.Contains(t, row.Detail, "100 MiB")
				require.NotEmpty(t, row.Remediation)
			}
		})
	}
}
