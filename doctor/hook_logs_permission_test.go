package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHookLogPermissionErrorsKeepOperationPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	denied := filepath.Join(dir, "denied")
	for _, op := range []string{"lstat", "stat", "readlink", "readdir", "access"} {
		for _, cause := range []error{syscall.EACCES, syscall.EPERM} {
			t.Run(op+"/"+cause.Error(), func(t *testing.T) {
				report := &Report{}
				err := fmt.Errorf("scan hook logs: %w", &os.PathError{Op: op, Path: denied, Err: cause})
				require.True(t, failHookLogPermission(report, dir, "", err))
				row := findCheck(t, report, "hook logs")
				require.Equal(t, StatusFail, row.Status)
				require.Contains(t, row.Detail, denied)
				require.Contains(t, row.Remediation, denied)
				require.Equal(t, 1, report.UnresolvedCount())
			})
		}
	}
	// Access may return a bare errno; retain its explicitly resolved blocker.
	report := &Report{}
	require.True(t, failHookLogPermission(report, dir, denied, syscall.EACCES))
	require.Contains(t, findCheck(t, report, "hook logs").Detail, denied)
	// Non-permission I/O errors still belong to advisory incomplete reporting.
	require.False(t, failHookLogPermission(&Report{}, dir, denied, syscall.EIO))
}
