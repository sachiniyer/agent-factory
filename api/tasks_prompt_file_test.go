package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTasksUpdatePromptFile(t *testing.T) {
	for _, tc := range []struct {
		name, contents           string
		missing, both, wantError bool
	}{
		{name: "verbatim", contents: "  report λ\r\n\n"},
		{name: "empty", wantError: true},
		{name: "whitespace", contents: " \n", wantError: true},
		{name: "missing", missing: true, wantError: true},
		{name: "mutually exclusive including empty prompt", contents: "report", both: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			resetUpdateFlags(t)
			calls := stubDaemon(t)
			seedTask(t, task.Task{ID: "prompt-file", Prompt: "before", CronExpr: "0 9 * * *", Enabled: true})
			file := filepath.Join(t.TempDir(), "prompt.md")
			if !tc.missing {
				require.NoError(t, os.WriteFile(file, []byte(tc.contents), 0600))
			}
			require.NoError(t, tasksUpdateCmd.Flags().Set("prompt-file", file))
			if tc.both {
				require.NoError(t, tasksUpdateCmd.Flags().Set("prompt", ""))
			}
			err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"prompt-file"})
			if tc.wantError {
				require.Error(t, err)
				assert.Zero(t, calls.writes)
			} else {
				require.NoError(t, err)
				assert.Equal(t, 1, calls.writes)
				got, err := task.GetTask("prompt-file")
				require.NoError(t, err)
				assert.Equal(t, tc.contents, got.Prompt)
			}
		})
	}
}
